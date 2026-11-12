package identity

import (
	"os"
	"path/filepath"
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
