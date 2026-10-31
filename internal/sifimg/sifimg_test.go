package sifimg

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sylabs/sif/v2/pkg/sif"
)

func newSIF(t *testing.T, fs sif.FSType) string {
	t.Helper()
	di, err := sif.NewDescriptorInput(sif.DataPartition, bytes.NewReader(bytes.Repeat([]byte("squash"), 1000)),
		sif.OptPartitionMetadata(fs, sif.PartPrimSys, "arm64"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.sif")
	f, err := sif.CreateContainerAtPath(path, sif.OptCreateWithDescriptors(di))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.UnloadContainer(); err != nil {
		t.Fatal(err)
	}
	return path
}

func newKey(t *testing.T) signature.SignerVerifier {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := signature.LoadECDSASignerVerifier(k, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestSignVerify(t *testing.T) {
	ctx := context.Background()
	path := newSIF(t, sif.FsSquash)
	good, other := newKey(t), newKey(t)

	if err := Verify(ctx, path, good); err == nil {
		t.Fatal("unsigned image verified")
	}
	if err := Sign(ctx, path, good); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, path, good); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := Verify(ctx, path, other); err == nil {
		t.Fatal("verified with the wrong key")
	}

	// Flip a byte inside the partition: the signature must no longer hold.
	rfs, err := PrimaryRootFS(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("X"), rfs.Offset+10); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Verify(ctx, path, good); err == nil {
		t.Fatal("tampered image verified")
	}
}

func TestPrimaryRootFS(t *testing.T) {
	rfs, err := PrimaryRootFS(newSIF(t, sif.FsSquash))
	if err != nil {
		t.Fatal(err)
	}
	if rfs.Size != 6000 || rfs.Offset <= 0 || rfs.Arch != "arm64" {
		t.Fatalf("unexpected rootfs %+v", rfs)
	}
	if _, err := PrimaryRootFS(newSIF(t, sif.FsEncryptedSquashfs)); err == nil {
		t.Fatal("encrypted rootfs accepted")
	}
}
