// Package keys manages the signing keypair (PEM, compatible with
// `apptainer sign --key` / `apptainer verify --key`) and age encryption keys.
package keys

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
)

// GenerateSigning writes a new ECDSA P-256 keypair as PEM files.
func GenerateSigning(privPath, pubPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	privPEM, err := cryptoutils.MarshalPrivateKeyToPEM(priv)
	if err != nil {
		return err
	}
	pubPEM, err := cryptoutils.MarshalPublicKeyToPEM(priv.Public())
	if err != nil {
		return err
	}
	if err := writeNew(privPath, privPEM, 0o600); err != nil {
		return err
	}
	return writeNew(pubPath, pubPEM, 0o644)
}

// LoadSigner loads a PEM private key. The env var RAMEKIN_SIGNING_KEY, when
// set, holds the PEM contents and takes precedence (useful in CI).
func LoadSigner(path string) (signature.Signer, error) {
	b := []byte(os.Getenv("RAMEKIN_SIGNING_KEY"))
	if len(b) == 0 {
		var err error
		if b, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("signing key: %w (generate one with `ramekin keys generate`)", err)
		}
	}
	priv, err := cryptoutils.UnmarshalPEMToPrivateKey(b, cryptoutils.SkipPassword)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	return signature.LoadSigner(priv, crypto.SHA256)
}

// LoadVerifier parses a PEM public key.
func LoadVerifier(pemBytes []byte) (signature.Verifier, error) {
	pub, err := cryptoutils.UnmarshalPEMToPublicKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("signing public key: %w", err)
	}
	return signature.LoadVerifier(pub, crypto.SHA256)
}

// GenerateAge writes a new age X25519 identity and returns its recipient.
func GenerateAge(path string) (string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf("# public key: %s\n%s\n", id.Recipient(), id)
	if err := writeNew(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}

// ReadAgeIdentity returns the AGE-SECRET-KEY line of an identity file.
func ReadAgeIdentity(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			return line, nil
		}
	}
	return "", fmt.Errorf("%s: no AGE-SECRET-KEY found", path)
}

// ParseRecipients combines inline recipients with those listed in file
// (one per line, # comments allowed). A missing file is not an error.
func ParseRecipients(inline []string, file string) ([]age.Recipient, error) {
	lines := append([]string(nil), inline...)
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	}
	var out []age.Recipient
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		r, err := age.ParseX25519Recipient(l)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", l, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("no encryption recipients configured (see `ramekin keys generate`)")
	}
	return out, nil
}

// AppendRecipient adds a recipient line to file if not already present.
func AppendRecipient(file, recipient, comment string) error {
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Contains(b, []byte(recipient)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "# %s\n%s\n", comment, recipient); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeNew(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite a key", path)
		}
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
