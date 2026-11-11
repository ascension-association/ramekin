package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
)

func TestClassify(t *testing.T) {
	rel := &agent.Release{Version: "1"}
	m := &agent.Metrics{}
	for _, c := range []struct {
		st   agent.DeviceStatus
		want string
	}{
		{agent.DeviceStatus{Current: rel, App: agent.AppStatus{Running: true}, Metrics: m}, "healthy"},
		{agent.DeviceStatus{Current: rel, Trial: &agent.Release{Version: "2"}, Metrics: m}, "updating"},
		{agent.DeviceStatus{Metrics: m}, "idle"},
		{agent.DeviceStatus{Current: rel, App: agent.AppStatus{LastExit: "exit status 1"}, Metrics: m}, "unhealthy"},
		{agent.DeviceStatus{Current: rel, App: agent.AppStatus{Running: true}, LastError: "fetch", Metrics: m}, "warning"},
		{agent.DeviceStatus{Current: rel, App: agent.AppStatus{Running: true, Restarts: 2}, Metrics: m}, "warning"},
		// An older ramekind reports no app status or metrics; that isn't "unhealthy".
		{agent.DeviceStatus{Device: "old", Current: rel}, "warning"},
	} {
		if got, detail := classify(&c.st); got != c.want {
			t.Errorf("%+v: got %s (%s), want %s", c.st, got, detail, c.want)
		}
	}
}

func TestConsoleGuard(t *testing.T) {
	c := &console{p: &config.Project{Name: "demo", Dir: t.TempDir()}, key: "k"}
	h := c.handler("127.0.0.1:8099")
	do := func(method, host, path, key string) int {
		r := httptest.NewRequest(method, path, nil)
		r.Host = host
		if key != "" {
			r.Header.Set("X-Ramekin-Console", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := do("GET", "127.0.0.1:8099", "/", ""); code != http.StatusOK {
		t.Fatalf("page: %d", code)
	}
	if code := do("GET", "localhost:8099", "/api/fleet", "k"); code != http.StatusOK {
		t.Fatalf("fleet: %d", code)
	}
	if code := do("GET", "evil.example:8099", "/", ""); code != http.StatusForbidden {
		t.Fatalf("rebound host allowed: %d", code)
	}
	if code := do("POST", "127.0.0.1:8099", "/api/devices/x/switch?sha=a", ""); code != http.StatusForbidden {
		t.Fatalf("switch without key allowed: %d", code)
	}
	if code := do("POST", "127.0.0.1:8099", "/api/devices/x/switch?sha=a", "k"); code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", code)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:8099"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `content="k"`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("page missing key or CSP")
	}
	for _, addr := range []string{"0.0.0.0:8099", "192.168.1.5:8099", ":8099"} {
		if loopbackOnly(addr) == nil {
			t.Errorf("%s accepted", addr)
		}
	}
	if loopbackOnly("localhost:1") != nil || loopbackOnly("[::1]:1") != nil {
		t.Error("loopback refused")
	}
}

// TestConsoleLogs relays a device's log stream, live, with the device token
// added by the console. The page hears it's connected before the first line,
// and the console stops reading from the device when the page does.
func TestConsoleLogs(t *testing.T) {
	backlog, live, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once1, once2 sync.Once
	sendBacklog := func() { once1.Do(func() { close(backlog) }) }
	sendLive := func() { once2.Do(func() { close(live) }) }
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" || r.URL.Query().Get("follow") != "1" {
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.(http.Flusher).Flush() // like ramekind: headers first, even with nothing logged yet
		for _, step := range []struct {
			gate chan struct{}
			line string
		}{{backlog, "backlog line\n"}, {live, "live line\n"}} {
			select {
			case <-step.gate:
			case <-r.Context().Done():
				close(closed)
				return
			}
			io.WriteString(w, step.line)
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
		close(closed)
	}))
	defer dev.Close()

	p := &config.Project{Name: "demo", Dir: t.TempDir()}
	rec := deviceRecordPath(p, "pi")
	if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(`{"url": "`+dev.URL+`", "token": "secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &console{p: p, key: "k", devices: map[string]*fleetDevice{}, searched: map[string]time.Time{}}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = c.handler(srv.Listener.Addr().String())
	srv.Start()
	defer srv.Close()
	// Runs before the servers close, so a failed test can't leave them waiting.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer sendLive()
	defer sendBacklog()

	within := func(what string, f func()) {
		done := make(chan struct{})
		go func() { f(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	get := func(path, key string) (resp *http.Response) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
		if key != "" {
			req.Header.Set("X-Ramekin-Console", key)
		}
		var err error
		within("the response headers", func() { resp, err = http.DefaultClient.Do(req) })
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := get("/api/devices/pi/logs", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logs without key: %d", resp.StatusCode)
	}
	if resp := get("/api/devices/nope/logs", "k"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}

	resp := get("/api/devices/pi/logs", "k") // before the device has logged anything
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("logs: %d %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	readLine := func() (l string) {
		within("a log line", func() { l, _ = br.ReadString('\n') })
		return l
	}
	sendBacklog()
	if l := readLine(); l != "backlog line\n" {
		t.Fatalf("first line %q", l)
	}
	sendLive()
	if l := readLine(); l != "live line\n" {
		t.Fatalf("second line %q", l)
	}
	resp.Body.Close()
	within("the device's stream to close after the page stopped reading", func() { <-closed })
}
