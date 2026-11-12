package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceUUID(t *testing.T) {
	// Reference value from Python: uuid.uuid5(UUID("4e6728a8-5c45-477d-b01a-3b94ed71b777"), "dt:119ef33bdeceb626").
	if got, want := DeviceUUID("dt:119ef33bdeceb626"), "b8eafc38-a150-5c76-a06e-93a1fe3cccde"; got != want {
		t.Fatalf("DeviceUUID = %s, want %s", got, want)
	}
	if DeviceUUID("dt:a") == DeviceUUID("dt:b") {
		t.Fatal("different boards got the same UUID")
	}
	if DeviceUUID("") != "" {
		t.Fatal("a board without a hardware ID got a derived UUID")
	}
}

func TestIdentityUUID(t *testing.T) {
	dir := t.TempDir()
	i := &Identity{Dir: dir}
	// With a hardware ID: derived, nothing stored.
	if u, err := i.UUID("dt:119ef33bdeceb626"); err != nil || u != "b8eafc38-a150-5c76-a06e-93a1fe3cccde" {
		t.Fatalf("derived: %q, %v", u, err)
	}
	if _, err := os.Stat(filepath.Join(dir, uuidFile)); !os.IsNotExist(err) {
		t.Fatal("stored a UUID for a board with a hardware ID")
	}
	// Without: random, version 4, and the same one every time after.
	u1, err := i.UUID("")
	if err != nil || !ValidUUID(u1) || u1[14] != '4' {
		t.Fatalf("random: %q, %v", u1, err)
	}
	if u2, _ := (&Identity{Dir: dir}).UUID(""); u2 != u1 {
		t.Fatalf("random UUID changed: %s then %s", u1, u2)
	}
}

func TestValidUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"b8eafc38-a150-5c76-a06e-93a1fe3cccde":  true,
		"B8EAFC38-A150-5C76-A06E-93A1FE3CCCDE":  false, // only the canonical lowercase form
		"b8eafc38a1505c76a06e93a1fe3cccde":      false,
		"b8eafc38-a150-5c76-a06e-93a1fe3cccd":   false,
		"b8eafc38-a150-5c76-a06e-93a1fe3cccdg":  false,
		"<svg/onload=x>-a150-5c76-a06e-93a1fe3": false,
	} {
		if ValidUUID(s) != want {
			t.Errorf("ValidUUID(%q) = %v", s, !want)
		}
	}
}

func TestShortID(t *testing.T) {
	// Reference value computed separately in Python.
	if got, want := ShortID("b8eafc38-a150-5c76-a06e-93a1fe3cccde"), "Q3NFR-E51A1"; got != want {
		t.Fatalf("ShortID = %s, want %s", got, want)
	}
	if ShortID("not a uuid") != "" {
		t.Fatal("short ID for something that isn't a UUID")
	}
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := ShortID(DeviceUUID("dt:" + string(rune('a'+i%26)) + string(rune('0'+i))))
		if len(id) != 11 || id[5] != '-' || strings.Trim(id[:5]+id[6:], crockford) != "" {
			t.Fatalf("malformed short ID %q", id)
		}
		if seen[id] {
			t.Fatalf("short ID %s repeated", id)
		}
		seen[id] = true
	}
}

func TestShortIDClashes(t *testing.T) {
	a := "b8eafc38-a150-5c76-a06e-93a1fe3cccde"
	// Same first 50 bits (bytes 0-5, top of byte 7), different elsewhere.
	b := "b8eafc38-a150-4c76-b06e-93a1fe3c0000"
	if ShortID(a) != ShortID(b) {
		t.Fatalf("test UUIDs should share a short ID: %s %s", ShortID(a), ShortID(b))
	}
	got := ShortIDClashes(map[string]string{"pi-1": a, "pi-2": b, "other": "0f1e2d3c-4b5a-4968-8776-655443322110", "pi-1-old": a})
	if len(got) != 1 || strings.Join(got[ShortID(a)], ",") != "pi-1,pi-1-old,pi-2" {
		t.Fatalf("clashes: %v", got)
	}
	if got := ShortIDClashes(map[string]string{"pi": a, "pi-again": a}); len(got) != 0 {
		t.Fatalf("one device under two names reported as a clash: %v", got)
	}
}
