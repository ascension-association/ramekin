package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
)

func TestParseLabelSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		dpi  int
		want labelSize
	}{
		{"1.5x0.75in", 300, labelSize{W: 450, H: 225, DPI: 300, Unit: "in"}},
		{"1.5x0.75in", 203, labelSize{W: 305, H: 152, DPI: 203, Unit: "in"}},
		{"38x19mm", 300, labelSize{W: 449, H: 224, DPI: 300, Unit: "mm"}},
		{"1x1in", 300, labelSize{W: 300, H: 300, DPI: 300, Unit: "in"}},
	} {
		got, err := parseLabelSize(c.in, c.dpi)
		if err != nil || got != c.want {
			t.Errorf("%s at %d dpi: got %+v, %v; want %+v", c.in, c.dpi, got, err, c.want)
		}
	}
	for _, bad := range []string{"1.5x0.75", "1.5in", "x0.75in", "1.5x-1in", "0.1x0.1in", "30x30in", "abcxdefmm"} {
		if _, err := parseLabelSize(bad, 300); err == nil {
			t.Errorf("accepted -size %q", bad)
		}
	}
	if _, err := parseLabelSize("1.5x0.75in", 20); err == nil {
		t.Error("accepted -dpi 20")
	}
	if got := mustLabelSize("1.5x0.75in", 203).svgLength(305); got != "1.5025in" {
		t.Errorf("SVG length %q", got)
	}
}

func TestLabelSVG(t *testing.T) {
	const u, id = "b8eafc38-a150-5c76-a06e-93a1fe3cccde", "Q3NFR-E51A1"
	svg, err := labelSVG(u, mustLabelSize(defaultLabelSize, 300))
	if err != nil {
		t.Fatal(err)
	}
	// The white background covers the whole label in explicit units: some
	// viewers sized a width="100%" rect to a fraction of it.
	for _, want := range []string{`width="1.5in" height="0.75in" viewBox="0 0 450 225"`, `<rect x="0" y="0" width="450" height="225" fill="#fff"/>`, ">Q3NFR-<", ">E51A1<", "(UUID " + u + ")"} {
		if !strings.Contains(svg, want) {
			t.Errorf("label missing %q:\n%s", want, svg)
		}
	}
	// Modules are whole dots: 225 dots / 29 modules (code and quiet zone) = 7.
	if strings.Contains(svg, "%") {
		t.Errorf("label uses percentages:\n%s", svg)
	}
	if !strings.Contains(svg, "v7h-") {
		t.Errorf("QR modules aren't 7 dots high:\n%s", svg)
	}
	if _, err := labelSVG("not-a-uuid", mustLabelSize(defaultLabelSize, 300)); err == nil {
		t.Error("made a label for something that isn't a UUID")
	}

	// Render at the printer's resolution and scan, when the tools are installed.
	render, err1 := exec.LookPath("rsvg-convert")
	decode, err2 := exec.LookPath("zbarimg")
	if err1 != nil || err2 != nil {
		t.Skip("rsvg-convert and zbarimg not installed; skipping the scan check")
	}
	for _, c := range []struct {
		size string
		dpi  int
	}{{defaultLabelSize, 300}, {defaultLabelSize, 203}, {"1x1in", 300}, {"25x40mm", 300}} {
		svg, err := labelSVG(u, mustLabelSize(c.size, c.dpi))
		if err != nil {
			t.Fatalf("%s at %d dpi: %v", c.size, c.dpi, err)
		}
		dir := t.TempDir()
		in, png := filepath.Join(dir, "l.svg"), filepath.Join(dir, "l.png")
		os.WriteFile(in, []byte(svg), 0o644)
		dpi := strconv.Itoa(c.dpi)
		if out, err := exec.Command(render, "-d", dpi, "-p", dpi, in, "-o", png).CombinedOutput(); err != nil {
			t.Fatalf("rendering: %v\n%s", err, out)
		}
		out, err := exec.Command(decode, "-q", "--raw", png).Output()
		if err != nil || strings.TrimSpace(string(out)) != id {
			t.Errorf("%s at %d dpi: scanning gave %q, %v; want %s", c.size, c.dpi, out, err, id)
		}
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
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.Contains(w.Body.String(), ">E51A1<") {
		t.Fatalf("label: %d %s\n%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}
