// Package agent implements ramekind: it supervises the single container,
// installs new images with health-gated A/B switching and rollback, polls the
// release bucket, and serves the developer push API.
package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Release struct {
	Version     string    `json:"version"`
	Serial      int64     `json:"serial"`
	SHA256      string    `json:"sha256"`
	Source      string    `json:"source"` // "push" or "channel:<name>"
	Path        string    `json:"path"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
	// Attempts counts boots that tried this release while it was on trial.
	Attempts int `json:"attempts,omitempty"`
}

type BadRelease struct {
	Version string    `json:"version"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// State is persisted to <data_dir>/state.json after every transition.
type State struct {
	Current  *Release `json:"current,omitempty"`
	Previous *Release `json:"previous,omitempty"`
	// Trial is set while a new release is being health-checked. If the device
	// reboots mid-trial, the next boot retries it (up to maxTrialAttempts).
	Trial *Release `json:"trial,omitempty"`
	// SeenSerial is the highest manifest serial acted upon; older or replayed
	// manifests are ignored.
	SeenSerial int64                 `json:"seen_serial"`
	Bad        map[string]BadRelease `json:"bad,omitempty"` // by sha256
}

func (s *State) clone() State {
	c := *s
	c.Bad = make(map[string]BadRelease, len(s.Bad))
	for k, v := range s.Bad {
		c.Bad[k] = v
	}
	for _, r := range []**Release{&c.Current, &c.Previous, &c.Trial} {
		if *r != nil {
			cp := **r
			*r = &cp
		}
	}
	return c
}

func loadState(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// saveState writes atomically so a power cut never leaves a torn file.
func saveState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
