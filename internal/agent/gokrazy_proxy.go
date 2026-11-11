package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// GokrazyPrefix is the API path the gokrazy web interface is served under.
const GokrazyPrefix = "/v1/gokrazy"

// FeatureGokrazy in DeviceStatus.Features says the device serves GokrazyPrefix.
const FeatureGokrazy = "gokrazy"

// NewGokrazyProxy serves gokrazy's web interface over the device API, behind
// the API token, so `ramekin --dashboard` can open it. gokrazy always serves
// it on its unix socket; without a network password (official firmware)
// that's the only place, so this is the only way to reach it from outside.
func NewGokrazyProxy() http.Handler {
	return newGokrazyProxy(gokrazySocket, gokrazyPassword)
}

func newGokrazyProxy(socket string, password func() string) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			path := strings.TrimPrefix(pr.In.URL.Path, GokrazyPrefix)
			if path == "" {
				path = "/"
			}
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "gokrazy"
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Host = "gokrazy"
			pr.Out.Header.Del("Authorization") // the device API token stays here
			// gokrazy asks for its password on the socket too, when it has one.
			if pw := password(); pw != "" {
				pr.Out.SetBasicAuth("gokrazy", pw)
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
		FlushInterval: -1, // gokrazy streams logs
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("gokrazy's web interface isn't answering on %s: %v", socket, err), http.StatusBadGateway)
		},
	}
}
