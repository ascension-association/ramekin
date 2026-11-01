//go:build linux

package agent

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"
	"golang.org/x/sys/unix"

	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
)

// Installer copies a device that booted from an SD card onto its internal
// storage (eMMC or NVMe): the firmware's disk image, then its /perm data
// (identity, settings, apps). The device then powers off, so the card can
// be removed. The card is left as it was, as a fallback.
type Installer struct {
	Name        string
	Verifier    signature.Verifier // the project key: signs install requests
	FwVerifier  signature.Verifier // the firmware key: signs its manifests
	FirmwareURL string
	Channel     string
	DataDir     string
	Stop        func() // stops the app before its data is copied
	Log         *log.Logger
}

func (in *Installer) install(w http.ResponseWriter, r *http.Request) {
	if in == nil || in.Verifier == nil {
		http.Error(w, "this device isn't claimed", http.StatusConflict)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req InstallRequest
	if err := manifest.OpenInto(b, in.Verifier, &req); err != nil {
		http.Error(w, "install request "+err.Error(), http.StatusForbidden)
		return
	}
	if req.Device != in.Name || time.Since(req.At).Abs() > 10*time.Minute {
		http.Error(w, "the install request is for another device, or too old", http.StatusForbidden)
		return
	}
	res, code, err := in.run(context.WithoutCancel(r.Context()), req)
	if err != nil {
		in.Log.Printf("install: %v", err)
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
	go func() {
		time.Sleep(2 * time.Second)
		if err := gokrazyPost("/poweroff"); err != nil {
			in.Log.Printf("install: power off: %v", err)
		}
	}()
}

func (in *Installer) run(ctx context.Context, req InstallRequest) (*InstallResult, int, error) {
	boot := bootDiskName()
	var target *Disk
	for _, d := range Disks("/sys", boot) {
		if d.Name == req.Target {
			target = &d
		}
	}
	switch {
	case target == nil:
		return nil, http.StatusBadRequest, fmt.Errorf("no disk %s", req.Target)
	case target.Boot:
		return nil, http.StatusBadRequest, fmt.Errorf("%s is the disk the device is running from", req.Target)
	case target.Kind != "emmc" && target.Kind != "nvme":
		return nil, http.StatusBadRequest, fmt.Errorf("%s is %s storage; install to emmc or nvme", req.Target, target.Kind)
	}
	parts := diskPartitions(req.Target)
	if len(parts) > 0 && !req.Overwrite {
		return nil, http.StatusConflict, fmt.Errorf("%s has partitions (%s); erasing them needs --overwrite", req.Target, strings.Join(parts, ", "))
	}

	// The firmware's disk image, as signed by the firmware key.
	chURL := strings.TrimSuffix(in.FirmwareURL, "/") + "/" + in.Channel + ".json"
	mb, err := release.FetchBytes(ctx, chURL, 1<<20)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("firmware channel: %w", err)
	}
	m, err := manifest.Open(mb, in.FwVerifier)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if m.Image == nil {
		return nil, http.StatusConflict, fmt.Errorf("firmware %s has no disk image to install (official firmware has one)", m.Version)
	}
	in.Log.Printf("install: downloading firmware %s's disk image", m.Version)
	img, err := in.download(ctx, chURL, m.Image)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer os.Remove(img)

	in.Log.Printf("install: writing it to %s", req.Target)
	dev := "/dev/" + req.Target
	if err := writeImage(img, dev); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := in.reidentify(dev); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := rereadPartitions(dev); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("re-reading %s's partitions: %w", req.Target, err)
	}

	in.Log.Print("install: stopping the app and copying /perm data")
	in.Stop()
	if err := copyPerm(partitionDev(req.Target, 4), in.DataDir); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	in.Log.Printf("install: done; powering off. Remove the SD card, then power on to boot from %s", req.Target)
	return &InstallResult{Target: req.Target, Firmware: m.Version, Erased: parts}, http.StatusOK, nil
}

func (in *Installer) download(ctx context.Context, chURL string, part *manifest.Part) (string, error) {
	obj, err := release.Resolve(chURL, part.Object)
	if err != nil {
		return "", err
	}
	rc, err := release.Fetch(ctx, obj)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	path, sum, _, err := receive(rc, TmpDir(in.DataDir), part.Size)
	if err != nil {
		return "", err
	}
	if sum != part.SHA256 {
		os.Remove(path)
		return "", fmt.Errorf("disk image: sha256 %s does not match the signed manifest", sum)
	}
	return path, nil
}

// writeImage decompresses a gzipped disk image onto dev.
func writeImage(gz, dev string) error {
	f, err := os.Open(gz)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(out, zr, make([]byte, 4<<20)); err != nil {
		out.Close()
		return fmt.Errorf("writing %s: %w", dev, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// reidentify gives the newly written disk its own partition IDs, so they
// don't clash with the card the device booted from.
func (in *Installer) reidentify(dev string) error {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	var id [4]byte
	rand.Read(id[:])
	oldHex, newHex, err := reidentify(f, id)
	if err != nil {
		return fmt.Errorf("partition IDs: %w", err)
	}
	first, last, err := GPTPartition(f, 1)
	if err != nil {
		return err
	}
	b := make([]byte, (last-first+1)*sector)
	if _, err := f.ReadAt(b, int64(first)*sector); err != nil {
		return err
	}
	if n := replaceBootID(b, oldHex, newHex); n == 0 {
		return fmt.Errorf("the boot partition doesn't name the root partition by %s", oldHex)
	}
	if _, err := f.WriteAt(b, int64(first)*sector); err != nil {
		return err
	}
	in.Log.Printf("install: partition IDs %s → %s", oldHex, newHex)
	return f.Sync()
}

func rereadPartitions(dev string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKRRPART, 0); errno != 0 {
		return errno
	}
	return nil
}

// copyPerm copies the device's data directory onto the new disk's /perm.
func copyPerm(permDev, dataDir string) error {
	var err error
	for i := 0; i < 50; i++ { // the partition device appears shortly after BLKRRPART
		if _, err = os.Stat(permDev); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%s didn't appear: %w", permDev, err)
	}
	mnt, err := os.MkdirTemp("", "ramekin-install-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if err := unix.Mount(permDev, mnt, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mounting the new /perm: %w", err)
	}
	defer unix.Unmount(mnt, 0)
	dst := filepath.Join(mnt, filepath.Base(dataDir))
	if err := copyTree(dataDir, dst); err != nil {
		return fmt.Errorf("copying %s: %w", dataDir, err)
	}
	unix.Sync()
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "tmp" {
			return filepath.SkipDir // downloads in progress, including the image
		}
		to := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(to, fi.Mode().Perm())
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		case fi.Mode().IsRegular():
			return copyFile(p, to, fi.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// diskPartitions lists a disk's partitions (from sysfs).
func diskPartitions(disk string) []string {
	ents, _ := os.ReadDir(filepath.Join("/sys/block", disk))
	var out []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join("/sys/block", disk, e.Name(), "partition")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// partitionDev names partition n of disk: mmcblk0 → /dev/mmcblk0p4, sda → /dev/sda4.
func partitionDev(disk string, n int) string {
	if c := disk[len(disk)-1]; c >= '0' && c <= '9' {
		return fmt.Sprintf("/dev/%sp%d", disk, n)
	}
	return fmt.Sprintf("/dev/%s%d", disk, n)
}

// gokrazyPost sends a POST to the device's own gokrazy API.
func gokrazyPost(path string) error {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", gokrazySocket)
		},
	}}
	req, err := http.NewRequest(http.MethodPost, "http://gokrazy"+path, nil)
	if err != nil {
		return err
	}
	resp, err := basicAuth{hc, gokrazyPassword()}.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errors.New(resp.Status)
	}
	return nil
}
