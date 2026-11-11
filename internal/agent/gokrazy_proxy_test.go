package agent

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGokrazyProxy serves gokrazy's web interface from its unix socket under
// /v1/gokrazy/, only with the API token, which gokrazy never sees.
func TestGokrazyProxy(t *testing.T) {
	dir, err := os.MkdirTemp("", "gk") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "gokrazy-http.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct{ method, path, query, auth, body string }
	got := make(chan seen, 1)
	gk := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(b)}
		io.WriteString(w, "gokrazy page")
	})}
	go gk.Serve(ln)
	t.Cleanup(func() { gk.Close() })

	password := "pw"
	api := &API{Token: "tok", Gokrazy: newGokrazyProxy(sock, func() string { return password })}
	h := api.Handler()
	do := func(method, target, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := do("GET", "/v1/gokrazy/", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", w.Code)
	}
	if w := do("GET", "/v1/gokrazy/", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}

	w := do("GET", "/v1/gokrazy/status?path=%2Fuser%2Framekind", "tok", "")
	if w.Code != http.StatusOK || w.Body.String() != "gokrazy page" {
		t.Fatalf("status page: %d %q", w.Code, w.Body.String())
	}
	s := <-got
	wantAuth := "Basic Z29rcmF6eTpwdw==" // gokrazy:pw
	if s.path != "/status" || s.query != "path=%2Fuser%2Framekind" || s.auth != wantAuth {
		t.Fatalf("gokrazy saw %+v", s)
	}

	do("POST", "/v1/gokrazy/restart?path=x", "tok", "xsrftoken=1")
	if s := <-got; s.method != "POST" || s.path != "/restart" || s.body != "xsrftoken=1" {
		t.Fatalf("POST: gokrazy saw %+v", s)
	}

	do("GET", "/v1/gokrazy/", "tok", "")
	if s := <-got; s.path != "/" {
		t.Fatalf("root: gokrazy saw path %q", s.path)
	}

	// Without a gokrazy password (official firmware), nothing replaces the token.
	password = ""
	do("GET", "/v1/gokrazy/", "tok", "")
	if s := <-got; s.auth != "" {
		t.Fatalf("no password: gokrazy saw Authorization %q", s.auth)
	}

	gone := &API{Token: "tok", Gokrazy: newGokrazyProxy(filepath.Join(dir, "missing.sock"), func() string { return "" })}
	r := httptest.NewRequest("GET", "/v1/gokrazy/", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	gone.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "isn't answering") {
		t.Fatalf("missing socket: %d %q", rec.Code, rec.Body.String())
	}
}
