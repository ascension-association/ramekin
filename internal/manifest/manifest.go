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

	// Kind is "firmware" for a gokrazy OS image (boot and root partitions,
	// in Parts); empty for an app release.
	Kind  string `json:"kind,omitempty"`
	Parts []Part `json:"parts,omitempty"`
	// Plain is set when the parts aren't encrypted: official firmware,
	// which anyone may download and flash.
	Plain bool `json:"plain,omitempty"`
	// Image is a gzipped disk image for flashing new devices (official
	// firmware); devices don't use it. SHA256 and Size are of the .gz file.
	Image *Part `json:"image,omitempty"`
}

// Part is one encrypted file of a firmware release.
type Part struct {
	Name   string `json:"name"`   // "boot" or "root"
	Object string `json:"object"` // relative to the channel file
	SHA256 string `json:"sha256"` // of the decrypted file
	Size   int64  `json:"size"`
}

type Envelope struct {
	Payload   []byte `json:"payload"`   // JSON-encoded Manifest
	Signature []byte `json:"signature"` // over Payload
}

func Sign(m *Manifest, s signature.Signer) ([]byte, error) {
	return Seal(m, s)
}

// Open verifies an envelope and returns its manifest.
func Open(b []byte, v signature.Verifier) (*Manifest, error) {
	var m Manifest
	if err := OpenInto(b, v, &m); err != nil {
		return nil, fmt.Errorf("manifest %w", err)
	}
	return &m, nil
}

// Seal signs any JSON-encodable value into an Envelope.
func Seal(v any, s signature.Signer) ([]byte, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	sig, err := s.SignMessage(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(Envelope{Payload: payload, Signature: sig}, "", "  ")
}

// OpenInto verifies an Envelope and decodes its payload into out.
func OpenInto(b []byte, v signature.Verifier, out any) error {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	if len(env.Signature) == 0 {
		return errors.New("is unsigned")
	}
	if err := v.VerifySignature(bytes.NewReader(env.Signature), bytes.NewReader(env.Payload)); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if err := json.Unmarshal(env.Payload, out); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	return nil
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
