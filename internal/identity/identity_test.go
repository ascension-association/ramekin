package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/manifest"
)

func projectKey(t *testing.T) (signature.Signer, signature.Verifier) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := signature.LoadECDSASignerVerifier(k, crypto.SHA256)
	return s, s
}

func record(t *testing.T, s signature.Signer, id *Identity, name, hw string) []byte {
	b, err := manifest.Seal(Record{Name: name, PublicKey: id.PublicKey(), AgeRecipient: id.Age.Recipient().String(), HardwareID: hw, ClaimedAt: time.Now()}, s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaimAndReload(t *testing.T) {
	signer, verifier := projectKey(t)
	dir := t.TempDir()
	id, err := Ensure(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := id.Load(verifier, "dt:A"); err != nil || id.Record != nil {
		t.Fatalf("fresh device should be unclaimed: %v %+v", err, id.Record)
	}
	if err := id.Claim(record(t, signer, id, "kitchen", "dt:A"), "0123456789abcdef0123", verifier, "dt:A"); err != nil {
		t.Fatal(err)
	}
	// After a restart the same keys and record load.
	again, err := Ensure(dir)
	if err != nil || again.PublicKey() != id.PublicKey() {
		t.Fatalf("keys not persisted: %v", err)
	}
	if err := again.Load(verifier, "dt:A"); err != nil || again.Record.Name != "kitchen" || again.Token != "0123456789abcdef0123" {
		t.Fatalf("reload: %v %+v", err, again.Record)
	}
	sig := again.Sign([]byte("status"))
	if Verify(id.PublicKey(), []byte("status"), sig) != nil || Verify(id.PublicKey(), []byte("tampered"), sig) == nil {
		t.Fatal("device signatures don't verify correctly")
	}
}

func TestTamperedOrCopiedIdentityIsRefused(t *testing.T) {
	signer, verifier := projectKey(t)
	a, _ := Ensure(t.TempDir())
	b, _ := Ensure(t.TempDir())
	recA := record(t, signer, a, "kitchen", "dt:A")

	// Another device's record: its keys don't match.
	if err := b.Claim(recA, "0123456789abcdef0123", verifier, "dt:B"); err == nil {
		t.Fatal("accepted another device's record")
	}
	// The whole identity directory copied to other hardware.
	if err := a.Claim(recA, "0123456789abcdef0123", verifier, "dt:A"); err != nil {
		t.Fatal(err)
	}
	copied, _ := Ensure(a.Dir)
	if err := copied.Load(verifier, "dt:B"); err == nil || !strings.Contains(err.Error(), "hardware") {
		t.Fatalf("copied identity accepted on other hardware: %v", err)
	}
	// An edited record (renamed) breaks the project signature.
	p := filepath.Join(a.Dir, recordFile)
	raw, _ := os.ReadFile(p)
	edited := strings.Replace(string(raw), `"payload": "`, `"payload": "e`, 1)
	os.WriteFile(p, []byte(edited), 0o600)
	if err := copied.Load(verifier, "dt:A"); err == nil {
		t.Fatal("edited record accepted")
	}
	// A record signed by a different project key.
	other, _ := projectKey(t)
	if err := b.Claim(record(t, other, b, "x", "dt:B"), "0123456789abcdef0123", verifier, "dt:B"); err == nil {
		t.Fatal("accepted a record signed by another key")
	}
}

func TestHardwareID(t *testing.T) {
	root := t.TempDir() + "/"
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(root+p), 0o755)
		os.WriteFile(root+p, []byte(s), 0o644)
	}
	if hardwareID(root) != "" {
		t.Fatal("expected no ID")
	}
	write("proc/cpuinfo", "processor\t: 0\nSerial\t\t: 10000000abcdef01\n")
	if got := hardwareID(root); got != "cpu:10000000abcdef01" {
		t.Fatal(got)
	}
	write("sys/class/dmi/id/product_uuid", "4C4C4544-0042\n")
	if got := hardwareID(root); got != "dmi:4c4c4544-0042" {
		t.Fatal(got)
	}
	write("sys/firmware/devicetree/base/serial-number", "c1a2b3\x00")
	if got := hardwareID(root); got != "dt:c1a2b3" {
		t.Fatal(got)
	}
}
