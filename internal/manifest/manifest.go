// Package manifest defines the signed channel pointer that devices poll.
//
// A channel object (channels/<name>.json) is an Envelope whose payload is a
// Manifest. The signature binds the payload to the project's signing key, so
// a party with write access to the bucket cannot point devices at arbitrary
// content, and Serial lets devices reject replays of older manifests.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
)

type Manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Channel string `json:"channel"`
	// Serial strictly increases every time a channel is pointed somewhere,
	// including rollbacks to an older version.
	Serial int64  `json:"serial"`
	Arch   string `json:"arch"`
	// Object is the bucket key of the age-encrypted SIF.
	Object string `json:"object"`
	// SHA256 and Size describe the decrypted SIF.
	SHA256    string         `json:"sha256"`
	Size      int64          `json:"size"`
	SBOM      string         `json:"sbom,omitempty"`
	Published time.Time      `json:"published"`
	Vulns     map[string]int `json:"vulnerabilities,omitempty"`
}

type Envelope struct {
	Payload   []byte `json:"payload"`   // JSON-encoded Manifest
	Signature []byte `json:"signature"` // over Payload
}

func Sign(m *Manifest, s signature.Signer) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig, err := s.SignMessage(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(Envelope{Payload: payload, Signature: sig}, "", "  ")
}

// Open verifies an envelope and returns its manifest.
func Open(b []byte, v signature.Verifier) (*Manifest, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("manifest envelope: %w", err)
	}
	if len(env.Signature) == 0 {
		return nil, errors.New("manifest is unsigned")
	}
	if err := v.VerifySignature(bytes.NewReader(env.Signature), bytes.NewReader(env.Payload)); err != nil {
		return nil, fmt.Errorf("manifest signature: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		return nil, fmt.Errorf("manifest payload: %w", err)
	}
	return &m, nil
}

// Unverified decodes an envelope without checking the signature. Only for
// display and for computing the next serial on the publisher side.
func Unverified(b []byte) (*Manifest, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(env.Payload, &m)
}

// NextSerial returns a serial greater than prev that also tracks wall time,
// so serials stay monotonic even if the previous channel object is lost.
func NextSerial(prev int64, now time.Time) int64 {
	s := now.UnixMilli()
	if s <= prev {
		s = prev + 1
	}
	return s
}
