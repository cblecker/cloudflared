package ingress

import (
	"flag"
	"fmt"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/config"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/tlsconfig"
)

func TestParseUnixSocket(t *testing.T) {
	rawYAML := `
ingress:
- service: unix:/tmp/echo.sock
`
	ing, err := ParseIngress(MustReadIngress(rawYAML))
	require.NoError(t, err)
	_, ok := ing.Rules[0].Service.(*unixSocketPath)
	require.True(t, ok)
}

func Test_parseIngress(t *testing.T) {
	localhost8000 := MustParseURL(t, "https://localhost:8000")
	localhost8001 := MustParseURL(t, "https://localhost:8001")
	fourOhFour := newStatusCode(404)
	defaultConfig := setConfig(originRequestFromYAML(config.OriginRequestConfig{}), config.OriginRequestConfig{})
	require.Equal(t, defaultKeepAliveConnections, defaultConfig.KeepAliveConnections)
	type args struct {
		rawYAML string
	}
	tests := []struct {
		name    string
		args    args
		want    []Rule
		wantErr bool
	}{
		{
			name:    "Empty file",
			args:    args{rawYAML: ""},
			wantErr: true,
		},
		{
			name: "Multiple rules",
			args: args{rawYAML: `
ingress:
 - hostname: tunnel1.example.com
   service: https://localhost:8000
 - hostname: "*"
   service: https://localhost:8001
`},
			want: []Rule{
				{
					Hostname: "tunnel1.example.com",
					Service:  &localService{URL: localhost8000},
					Config:   defaultConfig,
				},
				{
					Hostname: "*",
					Service:  &localService{URL: localhost8001},
					Config:   defaultConfig,
				},
			},
		},
		{
			name: "Extra keys",
			args: args{rawYAML: `
ingress:
 - hostname: "*"
   service: https://localhost:8000
extraKey: extraValue
`},
			want: []Rule{
				{
					Hostname: "*",
					Service:  &localService{URL: localhost8000},
					Config:   defaultConfig,
				},
			},
		},
		{
			name: "Hostname can be omitted",
			args: args{rawYAML: `
ingress:
 - service: https://localhost:8000
`},
			want: []Rule{
				{
					Service: &localService{URL: localhost8000},
					Config:  defaultConfig,
				},
			},
		},
		{
			name: "Invalid service",
			args: args{rawYAML: `
ingress:
 - hostname: "*"
   service: https://local host:8000
`},
			wantErr: true,
		},
		{
			name: "Last rule isn't catchall",
			args: args{rawYAML: `
ingress:
 - hostname: example.com
   service: https://localhost:8000
`},
			wantErr: true,
		},
		{
			name: "First rule is catchall",
			args: args{rawYAML: `
ingress:
 - service: https://localhost:8000
 - hostname: example.com
   service: https://localhost:8000
`},
			wantErr: true,
		},
		{
			name: "Catch-all rule can't have a path",
			args: args{rawYAML: `
ingress:
 - service: https://localhost:8001
   path: /subpath1/(.*)/subpath2
`},
			wantErr: true,
		},
		{
			name: "Invalid regex",
			args: args{rawYAML: `
ingress:
 - hostname: example.com
   service: https://localhost:8000
   path: "*/subpath2"
 - service: https://localhost:8001
`},
			wantErr: true,
		},
		{
			name: "Service must have a scheme",
			args: args{rawYAML: `
ingress:
 - service: localhost:8000
`},
			wantErr: true,
		},
		{
			name: "Wildcard not at start",
			args: args{rawYAML: `
ingress:
 - hostname: "test.*.example.com"
   service: https://localhost:8000
`},
			wantErr: true,
		},
		{
			name: "Service can't have a path",
			args: args{rawYAML: `
ingress:
 - service: https://localhost:8000/static/
`},
			wantErr: true,
		},
		{
			name: "Invalid HTTP status",
			args: args{rawYAML: `
ingress:
 - service: http_status:asdf
`},
			wantErr: true,
		},
		{
			name: "Valid HTTP status",
			args: args{rawYAML: `
ingress:
 - service: http_status:404
`},
			want: []Rule{
				{
					Hostname: "",
					Service:  &fourOhFour,
					Config:   defaultConfig,
				},
			},
		},
		{
			name: "Valid hello world service",
			args: args{rawYAML: `
ingress:
 - service: hello_world
`},
			want: []Rule{
				{
					Hostname: "",
					Service:  new(helloWorld),
					Config:   defaultConfig,
				},
			},
		},
		{
			name: "Hostname contains port",
			args: args{rawYAML: `
ingress:
 - hostname: "test.example.com:443"
   service: https://localhost:8000
 - hostname: "*"
   service: https://localhost:8001
`},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIngress(MustReadIngress(tt.args.rawYAML))
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseIngress() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.want, got.Rules)
		})
	}
}

func TestSingleOriginSetsConfig(t *testing.T) {
	flagSet := flag.NewFlagSet(t.Name(), flag.PanicOnError)
	flagSet.Bool("hello-world", true, "")
	flagSet.Duration(ProxyConnectTimeoutFlag, time.Second, "")
	flagSet.Duration(ProxyTLSTimeoutFlag, time.Second, "")
	flagSet.Duration(ProxyTCPKeepAlive, time.Second, "")
	flagSet.Bool(ProxyNoHappyEyeballsFlag, true, "")
	flagSet.Int(ProxyKeepAliveConnectionsFlag, 10, "")
	flagSet.Duration(ProxyKeepAliveTimeoutFlag, time.Second, "")
	flagSet.String(HTTPHostHeaderFlag, "example.com:8080", "")
	flagSet.String(OriginServerNameFlag, "example.com", "")
	flagSet.String(tlsconfig.OriginCAPoolFlag, "/etc/certs/ca.pem", "")
	flagSet.Bool(NoTLSVerifyFlag, true, "")
	flagSet.Bool(NoChunkedEncodingFlag, true, "")
	flagSet.Bool(config.BastionFlag, true, "")
	flagSet.String(ProxyAddressFlag, "localhost:8080", "")
	flagSet.Uint(ProxyPortFlag, 8080, "")
	flagSet.Bool(Socks5Flag, true, "")

	cliCtx := cli.NewContext(cli.NewApp(), flagSet, nil)
	err := cliCtx.Set("hello-world", "true")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyConnectTimeoutFlag, "1s")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyTLSTimeoutFlag, "1s")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyTCPKeepAlive, "1s")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyNoHappyEyeballsFlag, "true")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyKeepAliveConnectionsFlag, "10")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyKeepAliveTimeoutFlag, "1s")
	require.NoError(t, err)
	err = cliCtx.Set(HTTPHostHeaderFlag, "example.com:8080")
	require.NoError(t, err)
	err = cliCtx.Set(OriginServerNameFlag, "example.com")
	require.NoError(t, err)
	err = cliCtx.Set(tlsconfig.OriginCAPoolFlag, "/etc/certs/ca.pem")
	require.NoError(t, err)
	err = cliCtx.Set(NoTLSVerifyFlag, "true")
	require.NoError(t, err)
	err = cliCtx.Set(NoChunkedEncodingFlag, "true")
	require.NoError(t, err)
	err = cliCtx.Set(config.BastionFlag, "true")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyAddressFlag, "localhost:8080")
	require.NoError(t, err)
	err = cliCtx.Set(ProxyPortFlag, "8080")
	require.NoError(t, err)
	err = cliCtx.Set(Socks5Flag, "true")
	require.NoError(t, err)

	allowURLFromArgs := false
	logger, err := logger.New()
	require.NoError(t, err)
	ingress, err := NewSingleOrigin(cliCtx, allowURLFromArgs, logger)
	require.NoError(t, err)

	assert.Equal(t, time.Second, ingress.Rules[0].Config.ConnectTimeout)
	assert.Equal(t, time.Second, ingress.Rules[0].Config.TLSTimeout)
	assert.Equal(t, time.Second, ingress.Rules[0].Config.TCPKeepAlive)
	assert.True(t, ingress.Rules[0].Config.NoHappyEyeballs)
	assert.Equal(t, 10, ingress.Rules[0].Config.KeepAliveConnections)
	assert.Equal(t, time.Second, ingress.Rules[0].Config.KeepAliveTimeout)
	assert.Equal(t, "example.com:8080", ingress.Rules[0].Config.HTTPHostHeader)
	assert.Equal(t, "example.com", ingress.Rules[0].Config.OriginServerName)
	assert.Equal(t, "/etc/certs/ca.pem", ingress.Rules[0].Config.CAPool)
	assert.True(t, ingress.Rules[0].Config.NoTLSVerify)
	assert.True(t, ingress.Rules[0].Config.DisableChunkedEncoding)
	assert.True(t, ingress.Rules[0].Config.BastionMode)
	assert.Equal(t, "localhost:8080", ingress.Rules[0].Config.ProxyAddress)
	assert.Equal(t, uint(8080), ingress.Rules[0].Config.ProxyPort)
	assert.Equal(t, socksProxy, ingress.Rules[0].Config.ProxyType)
}

func TestFindMatchingRule(t *testing.T) {
	ingress := Ingress{
		Rules: []Rule{
			{
				Hostname: "tunnel-a.example.com",
				Path:     nil,
			},
			{
				Hostname: "tunnel-b.example.com",
				Path:     mustParsePath(t, "/health"),
			},
			{
				Hostname: "*",
			},
		},
	}

	tests := []struct {
		host          string
		path          string
		wantRuleIndex int
	}{
		{
			host:          "tunnel-a.example.com",
			path:          "/",
			wantRuleIndex: 0,
		},
		{
			host:          "tunnel-a.example.com",
			path:          "/pages/about",
			wantRuleIndex: 0,
		},
		{
			host:          "tunnel-a.example.com:443",
			path:          "/pages/about",
			wantRuleIndex: 0,
		},
		{
			host:          "tunnel-b.example.com",
			path:          "/health",
			wantRuleIndex: 1,
		},
		{
			host:          "tunnel-b.example.com",
			path:          "/index.html",
			wantRuleIndex: 2,
		},
		{
			host:          "tunnel-c.example.com",
			path:          "/",
			wantRuleIndex: 2,
		},
	}

	for i, test := range tests {
		_, ruleIndex := ingress.FindMatchingRule(test.host, test.path)
		assert.Equal(t, test.wantRuleIndex, ruleIndex, fmt.Sprintf("Expect host=%s, path=%s to match rule %d, got %d", test.host, test.path, test.wantRuleIndex, i))
	}
}

func mustParsePath(t *testing.T, path string) *regexp.Regexp {
	regexp, err := regexp.Compile(path)
	assert.NoError(t, err)
	return regexp
}

func MustParseURL(t *testing.T, rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u
}

func BenchmarkFindMatch(b *testing.B) {
	rulesYAML := `
ingress:
 - hostname: tunnel1.example.com
   service: https://localhost:8000
 - hostname: tunnel2.example.com
   service: https://localhost:8001
 - hostname: "*"
   service: https://localhost:8002
`

	ing, err := ParseIngress(MustReadIngress(rulesYAML))
	if err != nil {
		b.Error(err)
	}
	for n := 0; n < b.N; n++ {
		ing.FindMatchingRule("tunnel1.example.com", "")
		ing.FindMatchingRule("tunnel2.example.com", "")
		ing.FindMatchingRule("tunnel3.example.com", "")
	}
}

func MustReadIngress(s string) *config.Configuration {
	var conf config.Configuration
	err := yaml.Unmarshal([]byte(s), &conf)
	if err != nil {
		panic(err)
	}
	return &conf
}
