package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
)

// Gokrazy is the slice of gokrazy's own update API that firmware installs
// use. gokrazy keeps two root partitions; the boot partition (kernel, device
// tree, firmware blobs) has only one copy.
type Gokrazy interface {
	// StreamTo writes "root" (the inactive root partition) or "bootonly"
	// (the boot partition, keeping the active root).
	StreamTo(ctx context.Context, dest string, r io.Reader) error
	// Testboot makes the next boot use the inactive root once; booting
	// again returns to the active one unless Switch is called.
	Testboot(ctx context.Context) error
	// Switch makes the running (test-booted) root permanent.
	Switch(ctx context.Context) error
	Reboot(ctx context.Context) error
}

// FirmwareState is persisted to <data_dir>/firmware.json.
type FirmwareState struct {
	Current string `json:"current,omitempty"` // last confirmed version
	// Pending is set between writing a new image and confirming it.
	Pending *PendingFirmware `json:"pending,omitempty"`
	// SeenSerial is the highest firmware channel serial acted upon.
	SeenSerial int64 `json:"seen_serial,omitempty"`
	// Failed lists versions that didn't survive their test boot, by version,
	// so the channel doesn't install them again.
	Failed map[string]string `json:"failed,omitempty"`
}

type PendingFirmware struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// FirmwareStatus is the firmware part of a device's status.
type FirmwareStatus struct {
	Version   string            `json:"version"` // running
	Pending   string            `json:"pending,omitempty"`
	Failed    map[string]string `json:"failed,omitempty"`
	LastCheck time.Time         `json:"last_check,omitzero"`
	LastError string            `json:"last_error,omitempty"`
}

// Firmware installs OS images (from a channel or a push) with gokrazy's
// test boot, and after a test boot confirms the new image only once the
// device is healthy.
type Firmware struct {
	DataDir  string
	Version  string // the version this image was built as
	URL      string // firmware channel directory; "" disables channel updates
	Channel  string
	Verifier signature.Verifier
	Identity age.Identity // decrypts channel images; nil disables channel updates
	Gokrazy  func(ctx context.Context) (Gokrazy, error)
	// Healthy decides whether a test-booted image is kept.
	Healthy func(ctx context.Context) error
	Log     *log.Logger

	// Procfs and Sysfs are where /proc and /sys are read from (tests
	// override them).
	Procfs, Sysfs string
	// ReadBoot reads the first n bytes of the boot partition; nil reads the
	// device's (tests override it).
	ReadBoot func(n int64) ([]byte, error)

	mu sync.Mutex // serializes installs and checks

	smu     sync.Mutex // guards the fields below, so status never waits on an install
	lastErr string
	checked time.Time
}

func (f *Firmware) statePath() string { return filepath.Join(f.DataDir, "firmware.json") }

func (f *Firmware) load() FirmwareState {
	var st FirmwareState
	b, err := os.ReadFile(f.statePath())
	if err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (f *Firmware) save(st FirmwareState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writeAtomic(f.statePath(), b); err != nil {
		f.Log.Printf("firmware: saving state: %v", err)
	}
}

func (f *Firmware) Status() FirmwareStatus {
	st := f.load()
	f.smu.Lock()
	defer f.smu.Unlock()
	s := FirmwareStatus{Version: f.Version, Failed: st.Failed, LastCheck: f.checked, LastError: f.lastErr}
	if st.Pending != nil {
		s.Pending = st.Pending.Version
	}
	return s
}

// Resume runs at startup. After a test boot it keeps the new image if the
// device is healthy and reboots into the previous one otherwise; after a
// test boot that didn't come back (the device is on its old image again) it
// records the failure.
func (f *Firmware) Resume(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.load()
	testBoot, err := f.testBooted()
	if err != nil {
		f.Log.Printf("firmware: %v", err)
	}
	switch {
	case testBoot:
		f.Log.Printf("firmware: %s is on its test boot; confirming once the device is healthy", f.Version)
		hctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := f.Healthy(hctx)
		cancel()
		if err == nil {
			err = f.gokrazyDo(ctx, func(g Gokrazy) error { return g.Switch(ctx) })
		}
		if err != nil {
			f.Log.Printf("firmware: %s failed its test boot (%v); rebooting into the previous image", f.Version, err)
			if st.Failed == nil {
				st.Failed = map[string]string{}
			}
			st.Failed[f.Version] = "failed its test boot: " + err.Error()
			st.Pending = nil
			f.save(st)
			if err := f.gokrazyDo(ctx, func(g Gokrazy) error { return g.Reboot(ctx) }); err != nil {
				f.Log.Printf("firmware: reboot: %v", err)
			}
			return
		}
		f.Log.Printf("firmware: %s confirmed; restarting once so gokrazy treats it as the active image", f.Version)
		st.Current, st.Pending = f.Version, nil
		f.save(st)
		// Until it restarts, gokrazy still takes the previous image's
		// partition for active (it reads the boot command line), so the next
		// install would overwrite the running image.
		if err := f.gokrazyDo(ctx, func(g Gokrazy) error { return g.Reboot(ctx) }); err != nil {
			f.Log.Printf("firmware: reboot: %v", err)
		}
		return
	case st.Pending != nil && st.Pending.Version != f.Version:
		cur := f.Version
		if cur == "" {
			cur = "the current image"
		}
		f.Log.Printf("firmware: %s never confirmed its test boot; staying on %s", st.Pending.Version, cur)
		if st.Failed == nil {
			st.Failed = map[string]string{}
		}
		st.Failed[st.Pending.Version] = "didn't come up on its test boot"
		st.Pending = nil
		st.Current = f.Version
	default:
		// Includes booting the pending image without a test boot (e.g. after
		// writing it to the card by hand): it's running, so it's no longer pending.
		st.Current, st.Pending = f.Version, nil
	}
	f.save(st)
}

// TestBooted reports whether this is a firmware test boot.
func (f *Firmware) TestBooted() bool {
	tb, err := f.testBooted()
	return err == nil && tb
}

// testBooted reports whether the running root partition differs from the
// one the boot configuration names, which is what a gokrazy test boot does.
func (f *Firmware) testBooted() (bool, error) {
	procfs := f.Procfs
	if procfs == "" {
		procfs = "/proc"
	}
	cmdline, err := os.ReadFile(procfs + "/cmdline")
	if err != nil {
		return false, err
	}
	mountinfo, err := os.ReadFile(procfs + "/self/mountinfo")
	if err != nil {
		return false, err
	}
	want, ok := cmdlineRootPartition(string(cmdline))
	if !ok {
		return false, nil
	}
	dev, ok := mountedRootDevice(string(mountinfo))
	if !ok {
		return false, nil
	}
	got, err := os.ReadFile(f.sysfs() + "/dev/block/" + dev + "/partition")
	if err != nil {
		return false, fmt.Errorf("root partition number: %w", err)
	}
	return strings.TrimSpace(string(got)) != want, nil
}

func (f *Firmware) sysfs() string {
	if f.Sysfs != "" {
		return f.Sysfs
	}
	return "/sys"
}

var trailingNum = regexp.MustCompile(`(\d+)$`)

// partitionNumber returns the trailing partition number of a device name or
// PARTUUID ("/dev/mmcblk0p3", "/dev/sda2", "PARTUUID=2e18c40c-02").
func partitionNumber(s string) (string, bool) {
	m := trailingNum.FindString(s)
	if m == "" {
		return "", false
	}
	return strings.TrimLeft(m, "0"), true
}

// cmdlineRootPartition returns the number of the root partition named on
// the kernel command line: root=/dev/mmcblk0p2, root=PARTUUID=…-02, or
// gokrazy's root=PARTUUID=<boot partition>/PARTNROFF=<offset from it>.
func cmdlineRootPartition(cmdline string) (string, bool) {
	for _, f := range strings.Fields(cmdline) {
		v, ok := strings.CutPrefix(f, "root=")
		if !ok {
			continue
		}
		if base, off, ok := strings.Cut(v, "/PARTNROFF="); ok {
			b, bok := partitionNumber(base)
			o, err := strconv.Atoi(off)
			n, err2 := strconv.Atoi(b)
			if !bok || err != nil || err2 != nil {
				return "", false
			}
			return strconv.Itoa(n + o), true
		}
		return partitionNumber(v)
	}
	return "", false
}

// mountedRootDevice returns the device number ("179:2") of the last mount
// on / (a test boot moves the inactive partition over the original root).
// The mount's source is often just "/dev/root", so the number is what
// identifies the partition.
func mountedRootDevice(mountinfo string) (string, bool) {
	var dev string
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == "/" {
			dev = fields[2]
		}
	}
	return dev, dev != ""
}

func (f *Firmware) gokrazyDo(ctx context.Context, fn func(Gokrazy) error) error {
	g, err := f.Gokrazy(ctx)
	if err != nil {
		return fmt.Errorf("gokrazy update API: %w", err)
	}
	return fn(g)
}

// Install writes a firmware image (decrypted boot and root partition files)
// and reboots into it for a test boot. On success it doesn't return until
// the reboot is underway.
func (f *Firmware) Install(ctx context.Context, version, boot, root string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.install(ctx, version, boot, root)
}

func (f *Firmware) install(ctx context.Context, version, boot, root string) error {
	st := f.load()
	if st.Pending != nil {
		return fmt.Errorf("firmware %s is already waiting for its test boot", st.Pending.Version)
	}
	if tb, _ := f.testBooted(); tb {
		return errors.New("the device hasn't restarted since its last firmware update; it must before installing more")
	}
	g, err := f.Gokrazy(ctx)
	if err != nil {
		return fmt.Errorf("gokrazy update API: %w", err)
	}
	f.Log.Printf("firmware: writing %s (root, then boot)", version)
	for _, part := range []struct{ dest, path string }{{"root", root}, {"bootonly", boot}} {
		r, err := os.Open(part.path)
		if err != nil {
			return err
		}
		err = g.StreamTo(ctx, part.dest, r)
		r.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w", part.dest, err)
		}
	}
	if err := g.Testboot(ctx); err != nil {
		return fmt.Errorf("enable test boot: %w", err)
	}
	if err := f.checkTestboot(boot); err != nil {
		// Put back a boot partition without the request, so the device
		// keeps booting its current image.
		f.Log.Printf("firmware: %v; cancelling the install", err)
		if r, oerr := os.Open(boot); oerr == nil {
			werr := g.StreamTo(ctx, "bootonly", r)
			r.Close()
			if werr != nil {
				return fmt.Errorf("%v, and restoring the boot partition failed: %v (don't reboot this device)", err, werr)
			}
		}
		return fmt.Errorf("%w; the install was cancelled and the device still boots its current image", err)
	}
	st.Pending = &PendingFirmware{Version: version, At: time.Now().UTC()}
	f.save(st)
	f.Log.Printf("firmware: rebooting into %s for a test boot", version)
	return g.Reboot(ctx)
}

// checkTestboot reads back the boot partition after gokrazy marked it for a
// test boot. Some gokrazy versions put the flag in the wrong place, which
// leaves systemd-boot devices unbootable (gokrazy issue #395), so the flag
// must be on the kernel command line and the entry's other lines intact.
// Only the length of the image just written is read: anything after it is
// stale data from older images.
func (f *Firmware) checkTestboot(boot string) error {
	fi, err := os.Stat(boot)
	if err != nil {
		return err
	}
	read := f.ReadBoot
	if read == nil {
		read = f.readBootPartition
	}
	b, err := read(fi.Size())
	if err != nil {
		return fmt.Errorf("reading back the boot partition: %w", err)
	}
	return checkTestbootFlag(b)
}

const testbootFlag = "gokrazy.try_boot_inactive=1"

var optionsWithFlag = regexp.MustCompile(`\noptions [^\n\x00]*` + regexp.QuoteMeta(testbootFlag))

func checkTestbootFlag(b []byte) error {
	for _, bad := range []string{"title gokrazy " + testbootFlag, "linux /vmlinuz " + testbootFlag} {
		if bytes.Contains(b, []byte(bad)) {
			return errors.New("gokrazy wrote the test-boot request into the boot entry's title or kernel line (gokrazy issue #395)")
		}
	}
	if !bytes.Contains(b, []byte(testbootFlag)) {
		return errors.New("gokrazy didn't add the test-boot request to the kernel command line (gokrazy issue #395)")
	}
	// A systemd-boot entry takes the kernel command line from its options line.
	if bytes.Contains(b, []byte("\nlinux /vmlinuz\noptions ")) && !optionsWithFlag.Match(b) {
		return errors.New("gokrazy didn't add the test-boot request to the boot entry's options line (gokrazy issue #395)")
	}
	return nil
}

// readBootPartition reads the start of the boot partition: partition 1 of
// the disk holding the running root.
func (f *Firmware) readBootPartition(n int64) ([]byte, error) {
	procfs := f.Procfs
	if procfs == "" {
		procfs = "/proc"
	}
	mi, err := os.ReadFile(procfs + "/self/mountinfo")
	if err != nil {
		return nil, err
	}
	root, ok := mountedRootDevice(string(mi))
	if !ok {
		return nil, errors.New("can't find the root device")
	}
	name, err := bootPartitionName(f.sysfs(), root)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open("/dev/" + name)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b := make([]byte, n)
	_, err = io.ReadFull(fh, b)
	return b, err
}

// bootPartitionName returns the name ("mmcblk0p1") of partition 1 on the
// disk holding the partition with device number root. /sys/dev/block/<root>
// links into the disk's directory, which has a directory per partition.
func bootPartitionName(sysfs, root string) (string, error) {
	part, err := filepath.EvalSymlinks(filepath.Join(sysfs, "dev", "block", root))
	if err != nil {
		return "", err
	}
	disk := filepath.Dir(part)
	parts, _ := filepath.Glob(filepath.Join(disk, "*", "partition"))
	for _, p := range parts {
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == "1" {
			return filepath.Base(filepath.Dir(p)), nil
		}
	}
	return "", fmt.Errorf("no partition 1 on %s", filepath.Base(disk))
}

// Check polls the firmware channel and installs a newer image.
func (f *Firmware) Check(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.check(ctx)
	f.smu.Lock()
	f.checked, f.lastErr = time.Now().UTC(), ""
	if err != nil {
		f.lastErr = err.Error()
	}
	f.smu.Unlock()
	return err
}

func (f *Firmware) check(ctx context.Context) error {
	if f.URL == "" {
		return nil
	}
	chURL := strings.TrimSuffix(f.URL, "/") + "/" + f.Channel + ".json"
	b, err := release.FetchBytes(ctx, chURL, 1<<20)
	if errors.Is(err, release.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch firmware channel %s: %w", f.Channel, err)
	}
	m, err := manifest.Open(b, f.Verifier)
	if err != nil {
		return err
	}
	st := f.load()
	if m.Kind != "firmware" || m.Serial <= st.SeenSerial {
		return nil
	}
	skip := func(why string) error {
		st.SeenSerial = m.Serial
		f.save(st)
		if why != "" {
			f.Log.Printf("firmware: skipping %s: %s", m.Version, why)
		}
		return nil
	}
	switch {
	case m.Arch != runtime.GOARCH:
		return fmt.Errorf("firmware channel %s has %s for %s; this device is %s", f.Channel, m.Version, m.Arch, runtime.GOARCH)
	case m.Version == f.Version:
		return skip("")
	case st.Failed[m.Version] != "":
		return skip(st.Failed[m.Version])
	case !m.Plain && f.Identity == nil:
		return fmt.Errorf("firmware %s is encrypted, and this device has no key yet (claim it)", m.Version)
	}
	f.Log.Printf("firmware: channel %s has %s (serial %d), downloading", f.Channel, m.Version, m.Serial)
	paths := map[string]string{}
	defer func() {
		for _, p := range paths {
			os.Remove(p)
		}
	}()
	for _, part := range m.Parts {
		p, err := f.fetchPart(ctx, chURL, part, m.Plain)
		if err != nil {
			return err
		}
		paths[part.Name] = p
	}
	if paths["boot"] == "" || paths["root"] == "" {
		return fmt.Errorf("firmware %s lacks a boot or root partition", m.Version)
	}
	st.SeenSerial = m.Serial
	f.save(st)
	return f.install(ctx, m.Version, paths["boot"], paths["root"])
}

func (f *Firmware) fetchPart(ctx context.Context, chURL string, part manifest.Part, plain bool) (string, error) {
	obj, err := release.Resolve(chURL, part.Object)
	if err != nil {
		return "", err
	}
	rc, err := release.Fetch(ctx, obj)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", part.Object, err)
	}
	defer rc.Close()
	var r io.Reader = rc
	if !plain {
		if r, err = age.Decrypt(rc, f.Identity); err != nil {
			return "", fmt.Errorf("decrypt %s: %w", part.Object, err)
		}
	}
	path, sum, _, err := receive(r, TmpDir(f.DataDir), part.Size)
	if err != nil {
		return "", err
	}
	if sum != part.SHA256 {
		os.Remove(path)
		return "", fmt.Errorf("%s: sha256 %s does not match manifest %s", part.Object, sum, part.SHA256)
	}
	return path, nil
}

// VerifyParts checks pushed partition files against a signed firmware
// manifest.
func VerifyParts(m *manifest.Manifest, files map[string]string) error {
	if m.Kind != "firmware" {
		return errors.New("not a firmware manifest")
	}
	if m.Arch != runtime.GOARCH {
		return fmt.Errorf("firmware %s is for %s; this device is %s", m.Version, m.Arch, runtime.GOARCH)
	}
	for _, part := range m.Parts {
		p := files[part.Name]
		if p == "" {
			return fmt.Errorf("missing %s partition", part.Name)
		}
		fh, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, fh)
		fh.Close()
		if err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != part.SHA256 {
			return fmt.Errorf("%s partition: sha256 %s does not match the signed manifest", part.Name, got)
		}
	}
	return nil
}

// writeAtomic replaces path with b, durably.
func writeAtomic(path string, b []byte) error {
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
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable, e.g. before a reboot.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
