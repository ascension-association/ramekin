package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
