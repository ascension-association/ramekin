// Package agent implements ramekind: it supervises the single container,
// installs new images with health-gated A/B switching and rollback, polls the
// release bucket, and serves the developer push API.
package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// SeenSerials is the highest manifest serial acted upon per channel;
	// older or replayed manifests are ignored. Tracking each channel
	// separately lets a device switch channels without missing a release.
	SeenSerials map[string]int64 `json:"seen_serials,omitempty"`
	// LegacySeenSerial is the single serial older agents kept for all
	// channels; it is migrated on load.
	LegacySeenSerial int64                 `json:"seen_serial,omitempty"`
	Bad              map[string]BadRelease `json:"bad,omitempty"` // by sha256
}

func (s *State) clone() State {
	c := *s
	c.Bad = make(map[string]BadRelease, len(s.Bad))
	for k, v := range s.Bad {
		c.Bad[k] = v
	}
	c.SeenSerials = make(map[string]int64, len(s.SeenSerials))
	for k, v := range s.SeenSerials {
		c.SeenSerials[k] = v
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
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	s.migrate()
	return s, nil
}

// migrate moves an old agent's single serial to the channels its releases
// came from, keeping their replay protection. (A dev push may be current
// while the previous release came from a channel.)
func (s *State) migrate() {
	if s.LegacySeenSerial == 0 {
		return
	}
	for _, r := range []*Release{s.Current, s.Previous, s.Trial} {
		if r != nil {
			s.markSeen(channelOf(*r), s.LegacySeenSerial)
		}
	}
	s.LegacySeenSerial = 0
}

// markSeen raises a channel's serial and reports whether it changed.
func (s *State) markSeen(channel string, serial int64) bool {
	if channel == "" || serial <= s.SeenSerials[channel] {
		return false
	}
	if s.SeenSerials == nil {
		s.SeenSerials = map[string]int64{}
	}
	s.SeenSerials[channel] = serial
	return true
}

// channelOf returns the channel a release came from, or "" for pushes.
func channelOf(rel Release) string {
	ch, _ := strings.CutPrefix(rel.Source, "channel:")
	if ch == rel.Source {
		return ""
	}
	return ch
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
