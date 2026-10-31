package manifest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
)

func key(t *testing.T) signature.SignerVerifier {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sv, err := signature.LoadECDSASignerVerifier(k, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestSignOpen(t *testing.T) {
	sv := key(t)
	m := &Manifest{Name: "app", Version: "1.0.0", Serial: 42, SHA256: "abc"}
	env, err := Sign(m, sv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(env, sv)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.0.0" || got.Serial != 42 {
		t.Fatalf("got %+v", got)
	}
	if _, err := Open(env, key(t)); err == nil {
		t.Fatal("opened with wrong key")
	}

	// Tamper with the payload, keeping the signature.
	var e Envelope
	json.Unmarshal(env, &e)
	e.Payload = bytes.Replace(e.Payload, []byte(`"serial":42`), []byte(`"serial":43`), 1)
	tampered, _ := json.Marshal(e)
	if _, err := Open(tampered, sv); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestNextSerial(t *testing.T) {
	now := time.UnixMilli(1000)
	if s := NextSerial(0, now); s != 1000 {
		t.Fatalf("got %d", s)
	}
	if s := NextSerial(5000, now); s != 5001 {
		t.Fatalf("serial must exceed previous even with a slow clock, got %d", s)
	}
}
