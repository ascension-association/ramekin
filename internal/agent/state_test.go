package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeenSerialsPerChannel(t *testing.T) {
	var s State
	if !s.markSeen("stable", 100) || s.markSeen("stable", 100) || s.markSeen("stable", 50) {
		t.Fatal("stable: serials must only increase")
	}
	// A channel last published before stable is still new to this device.
	if !s.markSeen("beta", 60) {
		t.Fatal("beta blocked by stable's serial")
	}
	if s.markSeen("", 999) || len(s.SeenSerials) != 2 {
		t.Fatalf("pushes have no channel: %v", s.SeenSerials)
	}
}

func TestMigrateLegacySerial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// An older agent: a dev push is current, the previous came from stable.
	os.WriteFile(path, []byte(`{
		"current": {"version": "dev-1", "source": "push"},
		"previous": {"version": "1.0.0", "source": "channel:stable", "serial": 70},
		"seen_serial": 70
	}`), 0o600)
	s, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.SeenSerials["stable"] != 70 || s.LegacySeenSerial != 0 {
		t.Fatalf("not migrated: %+v", s)
	}
	// Saved state uses only the new field.
	if err := saveState(path, s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if s2, _ := loadState(path); s2.SeenSerials["stable"] != 70 || len(b) == 0 {
		t.Fatalf("round trip: %s", b)
	}
}
