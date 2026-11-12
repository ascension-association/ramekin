package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
)

func TestLabelSVG(t *testing.T) {
	const u = "b8eafc38-a150-5c76-a06e-93a1fe3cccde"
	svg, err := labelSVG(u, `pi<&>"1`, 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`width="30.00mm"`, "pi&lt;&amp;&gt;&#34;1", "b8eafc38-a150-5c76-", ">a06e-93a1fe3cccde<"} {
		if !strings.Contains(svg, want) {
			t.Errorf("label missing %q:\n%s", want, svg)
		}
	}
	if strings.Contains(svg, "pi<&") {
		t.Error("device name not escaped")
	}
	if _, err := labelSVG("not-a-uuid", "pi", 30); err == nil {
		t.Error("made a label for something that isn't a UUID")
	}

	// Decode the QR code for real when the tools are installed.
	render, err1 := exec.LookPath("rsvg-convert")
	decode, err2 := exec.LookPath("zbarimg")
	if err1 != nil || err2 != nil {
		t.Skip("rsvg-convert and zbarimg not installed; skipping the scan check")
	}
	dir := t.TempDir()
	in, png := filepath.Join(dir, "l.svg"), filepath.Join(dir, "l.png")
	os.WriteFile(in, []byte(svg), 0o644)
	if out, err := exec.Command(render, "-w", "200", in, "-o", png).CombinedOutput(); err != nil {
		t.Fatalf("rendering: %v\n%s", err, out)
	}
	out, err := exec.Command(decode, "-q", "--raw", png).Output()
	if err != nil || strings.TrimSpace(string(out)) != u {
		t.Fatalf("scanning the label gave %q, %v; want %s", out, err, u)
	}
}

func TestRecordUUID(t *testing.T) {
	derived := identity.DeviceUUID("dt:119ef33bdeceb626")
	for _, c := range []struct {
		d    deviceRecord
		want string
	}{
		{deviceRecord{HardwareID: "dt:119ef33bdeceb626"}, derived},
		{deviceRecord{UUID: "0f1e2d3c-4b5a-4968-8776-655443322110"}, "0f1e2d3c-4b5a-4968-8776-655443322110"}, // reported by a board with no serial
		{deviceRecord{UUID: "junk", HardwareID: "dt:119ef33bdeceb626"}, derived},
		{deviceRecord{}, ""},
	} {
		if got := recordUUID(c.d); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.d, got, c.want)
		}
	}
}

func TestConsoleLabel(t *testing.T) {
	const u = "b8eafc38-a150-5c76-a06e-93a1fe3cccde"
	c := &console{p: &config.Project{Name: "demo", Dir: t.TempDir()}, key: "k", devices: map[string]*fleetDevice{
		"pi":  {Name: "pi", UUID: u},
		"old": {Name: "old"}, // older ramekind, no hardware ID known
	}}
	h := c.handler("127.0.0.1:8099")
	get := func(path, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "127.0.0.1:8099"
		if key != "" {
			r.Header.Set("X-Ramekin-Console", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("/api/devices/pi/label.svg", ""); w.Code != http.StatusForbidden {
		t.Fatalf("without key: %d", w.Code)
	}
	if w := get("/api/devices/nope/label.svg", "k"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", w.Code)
	}
	if w := get("/api/devices/old/label.svg", "k"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no UUID") {
		t.Fatalf("device without UUID: %d %q", w.Code, w.Body.String())
	}
	w := get("/api/devices/pi/label.svg", "k")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Body.String(), "a06e-93a1fe3cccde") {
		t.Fatalf("label: %d %s\n%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}
