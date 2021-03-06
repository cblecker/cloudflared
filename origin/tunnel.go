package origin

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/buildinfo"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/ui"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/h2mux"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/tunnelrpc"
	tunnelpogs "github.com/cloudflare/cloudflared/tunnelrpc/pogs"
)

const (
	dialTimeout              = 15 * time.Second
	muxerTimeout             = 5 * time.Second
	lbProbeUserAgentPrefix   = "Mozilla/5.0 (compatible; Cloudflare-Traffic-Manager/1.0; +https://www.cloudflare.com/traffic-manager/;"
	DuplicateConnectionError = "EDUPCONN"
	FeatureSerializedHeaders = "serialized_headers"
	FeatureQuickReconnects   = "quick_reconnects"
)

type rpcName string

const (
	register     rpcName = "register"
	reconnect    rpcName = "reconnect"
	unregister   rpcName = "unregister"
	authenticate rpcName = " authenticate"
)

type TunnelConfig struct {
	ConnectionConfig *connection.Config
	BuildInfo        *buildinfo.BuildInfo
	ClientID         string
	CloseConnOnce    *sync.Once // Used to close connectedSignal no more than once
	EdgeAddrs        []string
	HAConnections    int
	IncidentLookup   IncidentLookup
	IsAutoupdated    bool
	IsFreeTunnel     bool
	LBPool           string
	Tags             []tunnelpogs.Tag
	Logger           logger.Service
	Observer         *connection.Observer
	ReportedVersion  string
	Retries          uint
	RunFromTerminal  bool

	NamedTunnel      *connection.NamedTunnelConfig
	ClassicTunnel    *connection.ClassicTunnelConfig
	MuxerConfig      *connection.MuxerConfig
	TunnelEventChan  chan ui.TunnelEvent
	ProtocolSelector connection.ProtocolSelector
	EdgeTLSConfigs   map[connection.Protocol]*tls.Config
}

type muxerShutdownError struct{}

func (e muxerShutdownError) Error() string {
	return "muxer shutdown"
}

// RegisterTunnel error from server
type serverRegisterTunnelError struct {
	cause     error
	permanent bool
}

func (e serverRegisterTunnelError) Error() string {
	return e.cause.Error()
}

// RegisterTunnel error from client
type clientRegisterTunnelError struct {
	cause error
}

func newRPCError(cause error, counter *prometheus.CounterVec, name rpcName) clientRegisterTunnelError {
	counter.WithLabelValues(cause.Error(), string(name)).Inc()
	return clientRegisterTunnelError{cause: cause}
}

func (e clientRegisterTunnelError) Error() string {
	return e.cause.Error()
}

func (c *TunnelConfig) RegistrationOptions(connectionID uint8, OriginLocalIP string, uuid uuid.UUID) *tunnelpogs.RegistrationOptions {
	policy := tunnelrpc.ExistingTunnelPolicy_balance
	if c.HAConnections <= 1 && c.LBPool == "" {
		policy = tunnelrpc.ExistingTunnelPolicy_disconnect
	}
	return &tunnelpogs.RegistrationOptions{
		ClientID:             c.ClientID,
		Version:              c.ReportedVersion,
		OS:                   fmt.Sprintf("%s_%s", c.BuildInfo.GoOS, c.BuildInfo.GoArch),
		ExistingTunnelPolicy: policy,
		PoolName:             c.LBPool,
		Tags:                 c.Tags,
		ConnectionID:         connectionID,
		OriginLocalIP:        OriginLocalIP,
		IsAutoupdated:        c.IsAutoupdated,
		RunFromTerminal:      c.RunFromTerminal,
		CompressionQuality:   uint64(c.MuxerConfig.CompressionSetting),
		UUID:                 uuid.String(),
		Features:             c.SupportedFeatures(),
	}
}

func (c *TunnelConfig) ConnectionOptions(originLocalAddr string, numPreviousAttempts uint8) *tunnelpogs.ConnectionOptions {
	// attempt to parse out origin IP, but don't fail since it's informational field
	host, _, _ := net.SplitHostPort(originLocalAddr)
	originIP := net.ParseIP(host)

	return &tunnelpogs.ConnectionOptions{
		Client:              c.NamedTunnel.Client,
		OriginLocalIP:       originIP,
		ReplaceExisting:     c.ConnectionConfig.ReplaceExisting,
		CompressionQuality:  uint8(c.MuxerConfig.CompressionSetting),
		NumPreviousAttempts: numPreviousAttempts,
	}
}

func (c *TunnelConfig) SupportedFeatures() []string {
	features := []string{FeatureSerializedHeaders}
	if c.NamedTunnel == nil {
		features = append(features, FeatureQuickReconnects)
	}
	return features
}

func StartTunnelDaemon(ctx context.Context, config *TunnelConfig, connectedSignal *signal.Signal, cloudflaredID uuid.UUID, reconnectCh chan ReconnectSignal) error {
	s, err := NewSupervisor(config, cloudflaredID)
	if err != nil {
		return err
	}
	return s.Run(ctx, connectedSignal, reconnectCh)
}

func ServeTunnelLoop(ctx context.Context,
	credentialManager *reconnectCredentialManager,
	config *TunnelConfig,
	addr *net.TCPAddr,
	connIndex uint8,
	connectedSignal *signal.Signal,
	cloudflaredUUID uuid.UUID,
	reconnectCh chan ReconnectSignal,
) error {
	haConnections.Inc()
	defer haConnections.Dec()

	protocallFallback := &protocallFallback{
		BackoffHandler{MaxRetries: config.Retries},
		config.ProtocolSelector.Current(),
		false,
	}
	connectedFuse := h2mux.NewBooleanFuse()
	go func() {
		if connectedFuse.Await() {
			connectedSignal.Notify()
		}
	}()
	// Ensure the above goroutine will terminate if we return without connecting
	defer connectedFuse.Fuse(false)
	// Each connection to keep its own copy of protocol, because individual connections might fallback
	// to another protocol when a particular metal doesn't support new protocol
	for {
		err, recoverable := ServeTunnel(
			ctx,
			credentialManager,
			config,
			addr,
			connIndex,
			connectedFuse,
			protocallFallback,
			cloudflaredUUID,
			reconnectCh,
			protocallFallback.protocol,
		)
		if !recoverable {
			return err
		}

		err = waitForBackoff(ctx, protocallFallback, config, connIndex, err)
		if err != nil {
			return err
		}
	}
}

// protocallFallback is a wrapper around backoffHandler that will try fallback option when backoff reaches
// max retries
type protocallFallback struct {
	BackoffHandler
	protocol   connection.Protocol
	inFallback bool
}

func (pf *protocallFallback) reset() {
	pf.resetNow()
	pf.inFallback = false
}

func (pf *protocallFallback) fallback(fallback connection.Protocol) {
	pf.resetNow()
	pf.protocol = fallback
	pf.inFallback = true
}

// Expect err to always be non nil
func waitForBackoff(
	ctx context.Context,
	protobackoff *protocallFallback,
	config *TunnelConfig,
	connIndex uint8,
	err error,
) error {
	duration, ok := protobackoff.GetBackoffDuration(ctx)
	if !ok {
		return err
	}

	if config.TunnelEventChan != nil {
		config.TunnelEventChan <- ui.TunnelEvent{Index: connIndex, EventType: ui.Reconnecting}
	}

	config.Logger.Infof("Retrying connection %d in %s seconds, error %v", connIndex, duration, err)
	protobackoff.Backoff(ctx)

	if protobackoff.ReachedMaxRetries() {
		fallback, hasFallback := config.ProtocolSelector.Fallback()
		if !hasFallback {
			return err
		}
		// Already using fallback protocol, no point to retry
		if protobackoff.protocol == fallback {
			return err
		}
		config.Logger.Infof("Fallback to use %s", fallback)
		protobackoff.fallback(fallback)
	} else if !protobackoff.inFallback {
		current := config.ProtocolSelector.Current()
		if protobackoff.protocol != current {
			protobackoff.protocol = current
			config.Logger.Infof("Change protocol to %s", current)
		}
	}
	return nil
}

func ServeTunnel(
	ctx context.Context,
	credentialManager *reconnectCredentialManager,
	config *TunnelConfig,
	addr *net.TCPAddr,
	connIndex uint8,
	fuse *h2mux.BooleanFuse,
	backoff *protocallFallback,
	cloudflaredUUID uuid.UUID,
	reconnectCh chan ReconnectSignal,
	protocol connection.Protocol,
) (err error, recoverable bool) {
	// Treat panics as recoverable errors
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("ServeTunnel: %v", r)
			}
			err = errors.Wrapf(err, "stack trace: %s", string(debug.Stack()))
			recoverable = true
		}
	}()

	// If launch-ui flag is set, send disconnect msg
	if config.TunnelEventChan != nil {
		defer func() {
			config.TunnelEventChan <- ui.TunnelEvent{Index: connIndex, EventType: ui.Disconnected}
		}()
	}

	edgeConn, err := edgediscovery.DialEdge(ctx, dialTimeout, config.EdgeTLSConfigs[protocol], addr)
	if err != nil {
		return err, true
	}
	connectedFuse := &connectedFuse{
		fuse:    fuse,
		backoff: backoff,
	}
	if protocol == connection.HTTP2 {
		connOptions := config.ConnectionOptions(edgeConn.LocalAddr().String(), uint8(backoff.retries))
		return ServeHTTP2(ctx, config, edgeConn, connOptions, connIndex, connectedFuse, reconnectCh)
	}
	return ServeH2mux(ctx, credentialManager, config, edgeConn, connIndex, connectedFuse, cloudflaredUUID, reconnectCh)
}

func ServeH2mux(
	ctx context.Context,
	credentialManager *reconnectCredentialManager,
	config *TunnelConfig,
	edgeConn net.Conn,
	connectionIndex uint8,
	connectedFuse *connectedFuse,
	cloudflaredUUID uuid.UUID,
	reconnectCh chan ReconnectSignal,
) (err error, recoverable bool) {
	config.Logger.Debugf("Connecting via h2mux")
	// Returns error from parsing the origin URL or handshake errors
	handler, err, recoverable := connection.NewH2muxConnection(ctx, config.ConnectionConfig, config.MuxerConfig, edgeConn, connectionIndex, config.Observer)
	if err != nil {
		return err, recoverable
	}

	errGroup, serveCtx := errgroup.WithContext(ctx)

	errGroup.Go(func() (err error) {
		if config.NamedTunnel != nil {
			connOptions := config.ConnectionOptions(edgeConn.LocalAddr().String(), uint8(connectedFuse.backoff.retries))
			return handler.ServeNamedTunnel(serveCtx, config.NamedTunnel, credentialManager, connOptions, connectedFuse)
		}
		registrationOptions := config.RegistrationOptions(connectionIndex, edgeConn.LocalAddr().String(), cloudflaredUUID)
		return handler.ServeClassicTunnel(serveCtx, config.ClassicTunnel, credentialManager, registrationOptions, connectedFuse)
	})

	errGroup.Go(listenReconnect(serveCtx, reconnectCh))

	err = errGroup.Wait()
	if err != nil {
		switch err := err.(type) {
		case *connection.DupConnRegisterTunnelError:
			// don't retry this connection anymore, let supervisor pick new a address
			return err, false
		case *serverRegisterTunnelError:
			config.Logger.Errorf("Register tunnel error from server side: %s", err.cause)
			// Don't send registration error return from server to Sentry. They are
			// logged on server side
			if incidents := config.IncidentLookup.ActiveIncidents(); len(incidents) > 0 {
				config.Logger.Error(activeIncidentsMsg(incidents))
			}
			return err.cause, !err.permanent
		case *clientRegisterTunnelError:
			config.Logger.Errorf("Register tunnel error on client side: %s", err.cause)
			return err, true
		case *muxerShutdownError:
			config.Logger.Info("Muxer shutdown")
			return err, true
		case *ReconnectSignal:
			config.Logger.Infof("Restarting connection %d due to reconnect signal in %s", connectionIndex, err.Delay)
			err.DelayBeforeReconnect()
			return err, true
		default:
			if err == context.Canceled {
				config.Logger.Debugf("Serve tunnel error: %s", err)
				return err, false
			}
			config.Logger.Errorf("Serve tunnel error: %s", err)
			return err, true
		}
	}
	return nil, true
}

func ServeHTTP2(
	ctx context.Context,
	config *TunnelConfig,
	tlsServerConn net.Conn,
	connOptions *tunnelpogs.ConnectionOptions,
	connIndex uint8,
	connectedFuse connection.ConnectedFuse,
	reconnectCh chan ReconnectSignal,
) (err error, recoverable bool) {
	config.Logger.Debugf("Connecting via http2")
	server := connection.NewHTTP2Connection(tlsServerConn, config.ConnectionConfig, config.NamedTunnel, connOptions, config.Observer, connIndex, connectedFuse)

	errGroup, serveCtx := errgroup.WithContext(ctx)
	errGroup.Go(func() error {
		server.Serve(serveCtx)
		return fmt.Errorf("Connection with edge closed")
	})

	errGroup.Go(listenReconnect(serveCtx, reconnectCh))

	err = errGroup.Wait()
	if err != nil {
		return err, true
	}
	return nil, false
}

func listenReconnect(ctx context.Context, reconnectCh <-chan ReconnectSignal) func() error {
	return func() error {
		select {
		case reconnect := <-reconnectCh:
			return &reconnect
		case <-ctx.Done():
			return nil
		}
	}
}

type connectedFuse struct {
	fuse    *h2mux.BooleanFuse
	backoff *protocallFallback
}

func (cf *connectedFuse) Connected() {
	cf.fuse.Fuse(true)
	cf.backoff.reset()
}

func (cf *connectedFuse) IsConnected() bool {
	return cf.fuse.Value()
}

func activeIncidentsMsg(incidents []Incident) string {
	preamble := "There is an active Cloudflare incident that may be related:"
	if len(incidents) > 1 {
		preamble = "There are active Cloudflare incidents that may be related:"
	}
	incidentStrings := []string{}
	for _, incident := range incidents {
		incidentString := fmt.Sprintf("%s (%s)", incident.Name, incident.URL())
		incidentStrings = append(incidentStrings, incidentString)
	}
	return preamble + " " + strings.Join(incidentStrings, "; ")

}
