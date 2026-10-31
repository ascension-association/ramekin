// Package identity gives each device an identity that lives on its /perm
// partition rather than in its OS image, so one image can serve a fleet.
//
// On first boot a device generates an Ed25519 key (to sign what it reports)
// and an age key (to decrypt releases). When the workstation claims it, it
// signs a Record with the project key binding the device's name to those
// public keys and to the board's hardware serial. The device checks that
// record at every start, so an edited record, or one copied from another
// device, is refused. The private keys never leave the device.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/manifest"
)

// Record is what the workstation signs when it claims a device.
type Record struct {
	Name         string    `json:"name"`
	PublicKey    string    `json:"public_key"` // Ed25519, base64
	AgeRecipient string    `json:"age_recipient"`
	HardwareID   string    `json:"hardware_id"`
	ClaimedAt    time.Time `json:"claimed_at"`
}

// SignatureHeader carries a device's Ed25519 signature over a response or
// report body, as base64.
const SignatureHeader = "Ramekin-Signature"

// Files in the identity directory (on /perm, readable only by root).
const (
	keyFile    = "device.key"  // Ed25519 seed, hex
	ageFile    = "age.key"     // AGE-SECRET-KEY-1...
	fleetFile  = "fleet.key"   // the project's shared age key, if it gave one
	recordFile = "record.json" // manifest.Envelope of a Record
	tokenFile  = "api-token"
)

// Identity is a device's keys and, once claimed, its signed record.
type Identity struct {
	Dir    string
	Key    ed25519.PrivateKey
	Age    *age.X25519Identity
	Record *Record // nil until claimed
	Token  string  // API token, set when claimed
	// Fleet is the project's shared age key, given at claim time so the
	// device can install releases published before it was claimed.
	Fleet *age.X25519Identity
}

// Ensure loads the device's keys from dir, generating any that are missing.
func Ensure(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := &Identity{Dir: dir}
	seed, err := readOrCreate(filepath.Join(dir, keyFile), func() string {
		s := make([]byte, ed25519.SeedSize)
		rand.Read(s)
		return hex.EncodeToString(s)
	})
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(seed)
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is corrupt", keyFile)
	}
	id.Key = ed25519.NewKeyFromSeed(b)
	ak, err := readOrCreate(filepath.Join(dir, ageFile), func() string {
		k, _ := age.GenerateX25519Identity()
		return k.String()
	})
	if err != nil {
		return nil, err
	}
	if id.Age, err = age.ParseX25519Identity(ak); err != nil {
		return nil, fmt.Errorf("%s: %w", ageFile, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, fleetFile)); err == nil {
		if id.Fleet, err = age.ParseX25519Identity(strings.TrimSpace(string(b))); err != nil {
			return nil, fmt.Errorf("%s: %w", fleetFile, err)
		}
	}
	return id, nil
}

// SaveFleetKey decrypts the project's fleet key, which the claim sent
// encrypted to this device's age key, and keeps it.
func (id *Identity) SaveFleetKey(encrypted []byte) error {
	r, err := age.Decrypt(bytes.NewReader(encrypted), id.Age)
	if err != nil {
		return fmt.Errorf("fleet key: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(r, 1<<10))
	if err != nil {
		return err
	}
	k, err := age.ParseX25519Identity(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("fleet key: %w", err)
	}
	if err := writeFile(filepath.Join(id.Dir, fleetFile), []byte(k.String()+"\n")); err != nil {
		return err
	}
	id.Fleet = k
	return nil
}

func readOrCreate(path string, gen func() string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	v := gen()
	return v, writeFile(path, []byte(v+"\n"))
}

// PublicKey is the device's Ed25519 public key, base64.
func (id *Identity) PublicKey() string {
	return base64.StdEncoding.EncodeToString(id.Key.Public().(ed25519.PublicKey))
}

// Sign signs b with the device key.
func (id *Identity) Sign(b []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(id.Key, b))
}

// Load reads the signed record, if any, and checks it against the project
// key, this device's keys and its hardware. A missing record means the
// device is unclaimed (nil error, nil Record); any other problem is an error,
// and the device should be treated as unclaimed until it is claimed again.
func (id *Identity) Load(v signature.Verifier, hardwareID string) error {
	b, err := os.ReadFile(filepath.Join(id.Dir, recordFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	rec, err := id.check(b, v, hardwareID)
	if err != nil {
		return err
	}
	tok, err := os.ReadFile(filepath.Join(id.Dir, tokenFile))
	if err != nil {
		return fmt.Errorf("identity has no API token: %w", err)
	}
	id.Record, id.Token = rec, strings.TrimSpace(string(tok))
	return nil
}

func (id *Identity) check(envelope []byte, v signature.Verifier, hardwareID string) (*Record, error) {
	if v == nil {
		return nil, errors.New("the device has no project key to check its identity record with")
	}
	var rec Record
	if err := manifest.OpenInto(envelope, v, &rec); err != nil {
		return nil, fmt.Errorf("identity record %w", err)
	}
	switch {
	case rec.Name == "":
		return nil, errors.New("identity record has no name")
	case rec.PublicKey != id.PublicKey():
		return nil, errors.New("identity record is for another device's key")
	case rec.AgeRecipient != id.Age.Recipient().String():
		return nil, errors.New("identity record is for another device's age key")
	case rec.HardwareID != hardwareID:
		return nil, fmt.Errorf("identity record is for hardware %q, but this is %q", rec.HardwareID, hardwareID)
	}
	return &rec, nil
}

// Claim checks a record signed by the workstation and stores it with the
// API token, completing the claim.
func (id *Identity) Claim(envelope []byte, token string, v signature.Verifier, hardwareID string) error {
	rec, err := id.check(envelope, v, hardwareID)
	if err != nil {
		return err
	}
	if len(token) < 16 {
		return errors.New("API token is too short")
	}
	if err := writeFile(filepath.Join(id.Dir, tokenFile), []byte(token+"\n")); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(id.Dir, recordFile), envelope); err != nil {
		return err
	}
	id.Record, id.Token = rec, token
	return nil
}

// Verify checks a device's signature (from SignatureHeader) over b.
func Verify(publicKey string, b []byte, sig string) error {
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid device public key")
	}
	s, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, b, s) {
		return errors.New("signature does not match the device's key")
	}
	return nil
}

// writeFile replaces path atomically, readable only by its owner.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
