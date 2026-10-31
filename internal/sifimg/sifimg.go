// Package sifimg signs, verifies and inspects SIF images without needing the
// apptainer binary. Signatures are DSSE envelopes, interoperable with
// `apptainer sign --key` and `apptainer verify --key`.
package sifimg

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sylabs/sif/v2/pkg/integrity"
	"github.com/sylabs/sif/v2/pkg/sif"
)

// Sign adds a signature over all object groups in the SIF at path.
func Sign(ctx context.Context, path string, s signature.Signer) error {
	f, err := sif.LoadContainerFromPath(path, sif.OptLoadWithFlag(os.O_RDWR))
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	defer f.UnloadContainer()
	is, err := integrity.NewSigner(f, integrity.OptSignWithSigner(s), integrity.OptSignWithContext(ctx))
	if err != nil {
		return err
	}
	return is.Sign()
}

// Verify checks that every object group in the SIF is signed by v.
func Verify(ctx context.Context, path string, v signature.Verifier) error {
	f, err := sif.LoadContainerFromPath(path, sif.OptLoadWithFlag(os.O_RDONLY))
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	defer f.UnloadContainer()
	iv, err := integrity.NewVerifier(f, integrity.OptVerifyWithVerifier(v), integrity.OptVerifyWithContext(ctx))
	if err != nil {
		return err
	}
	if err := iv.Verify(); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}

// RootFS describes the primary squashfs partition of a SIF.
type RootFS struct {
	Offset, Size int64
	Arch         string // GOARCH naming, e.g. arm64
}

// PrimaryRootFS locates the squashfs root filesystem inside the SIF.
func PrimaryRootFS(path string) (RootFS, error) {
	f, err := sif.LoadContainerFromPath(path, sif.OptLoadWithFlag(os.O_RDONLY))
	if err != nil {
		return RootFS{}, fmt.Errorf("load %s: %w", path, err)
	}
	defer f.UnloadContainer()
	d, err := f.GetDescriptor(sif.WithPartitionType(sif.PartPrimSys))
	if err != nil {
		return RootFS{}, fmt.Errorf("%s: no primary partition: %w", path, err)
	}
	fs, _, arch, err := d.PartitionMetadata()
	if err != nil {
		return RootFS{}, err
	}
	if fs != sif.FsSquash {
		// Encrypted (LUKS) or ext3 roots are not supported by the native runtime.
		return RootFS{}, errors.New("primary partition is not squashfs (apptainer-encrypted SIFs are not supported; ramekin encrypts with age instead)")
	}
	return RootFS{Offset: d.Offset(), Size: d.Size(), Arch: arch}, nil
}
