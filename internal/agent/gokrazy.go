package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gokrazy/updater"
)

// gokrazySocket is where gokrazy serves its HTTP API on the device itself.
const gokrazySocket = "/run/gokrazy-http.sock"

// LocalGokrazy connects to the device's own gokrazy update API over its unix
// socket, with the password gokrazy itself uses (/perm overrides /etc).
func LocalGokrazy(ctx context.Context) (Gokrazy, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", gokrazySocket)
		},
	}}
	t, err := updater.NewTarget(ctx, "http://gokrazy/", basicAuth{hc, gokrazyPassword()})
	if err != nil {
		return nil, err
	}
	return localGokrazy{t}, nil
}

func gokrazyPassword() string {
	for _, p := range []string{"/perm/gokr-pw.txt", "/etc/gokr-pw.txt"} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

type basicAuth struct {
	hc       *http.Client
	password string
}

func (b basicAuth) Do(r *http.Request) (*http.Response, error) {
	if b.password != "" {
		r.SetBasicAuth("gokrazy", b.password)
	}
	return b.hc.Do(r)
}

type localGokrazy struct{ t *updater.Target }

func (g localGokrazy) StreamTo(ctx context.Context, dest string, r io.Reader) error {
	return g.t.StreamTo(ctx, dest, r)
}
func (g localGokrazy) Testboot(ctx context.Context) error { return g.t.Testboot(ctx) }
func (g localGokrazy) Switch(ctx context.Context) error   { return g.t.Switch(ctx) }

// Reboot without kexec, so the board's boot loader loads the new kernel, and
// asynchronously, since gokrazy stops ramekind on the way down.
func (g localGokrazy) Reboot(ctx context.Context) error {
	return g.t.Reboot(ctx, updater.WithKexec(false), updater.WithAsync(true))
}
