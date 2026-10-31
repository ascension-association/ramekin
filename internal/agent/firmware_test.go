package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/ascension-association/ramekin/internal/manifest"
)

// fakeGokrazy records what a firmware install asks gokrazy to do.
type fakeGokrazy struct {
	mu      sync.Mutex
	written map[string]string
	calls   []string
}

func (g *fakeGokrazy) StreamTo(_ context.Context, dest string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.written == nil {
		g.written = map[string]string{}
	}
	g.written[dest] = string(b)
	g.calls = append(g.calls, "write "+dest)
	return nil
}
func (g *fakeGokrazy) do(c string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, c)
	return nil
}
func (g *fakeGokrazy) Testboot(context.Context) error { return g.do("testboot") }
func (g *fakeGokrazy) Switch(context.Context) error   { return g.do("switch") }
func (g *fakeGokrazy) Reboot(context.Context) error   { return g.do("reboot") }

// bootedFrom fakes /proc and /sys for a device whose boot configuration
// names root partition want while partition running is mounted on /. Like
// on real devices, the mount's source is just /dev/root.
func bootedFrom(t *testing.T, want, running string) string {
	base := t.TempDir()
	proc, sys := filepath.Join(base, "proc"), filepath.Join(base, "sys")
	os.MkdirAll(filepath.Join(proc, "self"), 0o755)
	os.WriteFile(filepath.Join(proc, "cmdline"), []byte("console=tty1 root=PARTUUID=60c24cc1-f3f9-427a-8199-4f9f2cab0001/PARTNROFF="+map[string]string{"2": "1", "3": "2"}[want]+" ro\n"), 0o644)
	os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte(
		"20 1 179:2 / / ro,relatime - squashfs /dev/root ro\n"+
			"21 20 0:5 / /dev rw - devtmpfs devtmpfs rw\n"+
			"30 1 179:"+running+" / / ro,relatime - squashfs /dev/root ro\n"), 0o644)
	disk := filepath.Join(sys, "devices", "mmc", "block", "mmcblk0")
	os.MkdirAll(filepath.Join(sys, "dev", "block"), 0o755)
	for _, n := range []string{"1", "2", "3", "4"} {
		os.MkdirAll(filepath.Join(disk, "mmcblk0p"+n), 0o755)
		os.WriteFile(filepath.Join(disk, "mmcblk0p"+n, "partition"), []byte(n+"\n"), 0o644)
		os.Symlink(filepath.Join(disk, "mmcblk0p"+n), filepath.Join(sys, "dev", "block", "179:"+n))
	}
	return base
}

func newFirmware(t *testing.T, g *fakeGokrazy, version, fake string, healthy error) *Firmware {
	dir := t.TempDir()
	os.MkdirAll(TmpDir(dir), 0o700)
	return &Firmware{
		DataDir: dir, Version: version, Channel: "stable",
		Procfs: filepath.Join(fake, "proc"), Sysfs: filepath.Join(fake, "sys"),
		Gokrazy: func(context.Context) (Gokrazy, error) { return g, nil },
		Healthy: func(context.Context) error { return healthy },
		Log:     log.New(io.Discard, "", 0),
		// A boot partition gokrazy marked for a test boot correctly.
		ReadBoot: func(int64) ([]byte, error) { return []byte(goodEntry), nil },
	}
}

// Boot entries after a test-boot request, as fixed gokrazy writes them and
// as older gokrazy wrote them (read back from a device it left unbootable).
const (
	testCmdline = "console=tty1 root=PARTUUID=60c24cc1-f3f9-427a-8199-4f9f2cab0001/PARTNROFF=1 ro init=/gokrazy/init rootwait panic=10 oops=panic"
	goodEntry   = testCmdline + " gokrazy.try_boot_inactive=1   \x00\x00title gokrazy\nlinux /vmlinuz\noptions " + testCmdline + " gokrazy.try_boot_inactive=1   "
	brokenEntry = testCmdline + "        \x00\x00title gokrazy gokrazy.try_boot_inactive=1\nlinux /vmlinuz gokrazy.try_boot_inactive=1\noptions " + testCmdline + "  "
	noFlag      = testCmdline + "        \x00\x00title gokrazy\nlinux /vmlinuz\noptions " + testCmdline + "  "
)

func TestCheckTestbootFlag(t *testing.T) {
	if err := checkTestbootFlag([]byte(goodEntry)); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{brokenEntry, noFlag} {
		if checkTestbootFlag([]byte(b)) == nil {
			t.Errorf("accepted %q", b)
		}
	}
}

func TestFirmwareCancelsBrokenTestboot(t *testing.T) {
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "1", bootedFrom(t, "2", "2"), nil)
	f.ReadBoot = func(int64) ([]byte, error) { return []byte(brokenEntry), nil }
	boot, root := filepath.Join(f.DataDir, "boot"), filepath.Join(f.DataDir, "root")
	os.WriteFile(boot, []byte("boot of 2"), 0o600)
	os.WriteFile(root, []byte("root of 2"), 0o600)
	if err := f.Install(context.Background(), "2", boot, root); err == nil {
		t.Fatal("installed despite a broken test-boot request")
	}
	// The boot partition is written again (without the request), and the
	// device isn't rebooted.
	want := "write root,write bootonly,testboot,write bootonly"
	if got := strings.Join(g.calls, ","); got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
	if f.load().Pending != nil {
		t.Fatal("left pending")
	}
}

func TestFirmwarePendingClearedWhenBootedDirectly(t *testing.T) {
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "2", bootedFrom(t, "2", "2"), nil)
	f.save(FirmwareState{Current: "1", Pending: &PendingFirmware{Version: "2"}})
	f.Resume(context.Background())
	if st := f.load(); st.Current != "2" || st.Pending != nil {
		t.Fatalf("state %+v", st)
	}
}

func TestBootPartitionName(t *testing.T) {
	fake := bootedFrom(t, "2", "3")
	if got, err := bootPartitionName(filepath.Join(fake, "sys"), "179:3"); err != nil || got != "mmcblk0p1" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRootPartitionParsing(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"root=PARTUUID=2e18c40c-02 ro", "2"}, {"root=/dev/mmcblk0p3", "3"}, {"x root=/dev/sda2 y", "2"},
		{"root=PARTUUID=60c24cc1-f3f9-427a-8199-4f9f2cab0001/PARTNROFF=1 ro", "2"},
		{"root=PARTUUID=60c24cc1-f3f9-427a-8199-4f9f2cab0001/PARTNROFF=2 ro", "3"},
	} {
		if got, ok := cmdlineRootPartition(c.in); !ok || got != c.want {
			t.Errorf("%q: got %q", c.in, got)
		}
	}
}

func TestFirmwareConfirmsHealthyTestBoot(t *testing.T) {
	g := &fakeGokrazy{}
	// Boot config still names partition 2; the test boot runs partition 3.
	f := newFirmware(t, g, "2", bootedFrom(t, "2", "3"), nil)
	f.save(FirmwareState{Current: "1", Pending: &PendingFirmware{Version: "2"}})
	f.Resume(context.Background())
	// Kept, then restarted so gokrazy takes its partition for the active one.
	if st := f.load(); st.Current != "2" || st.Pending != nil || strings.Join(g.calls, ",") != "switch,reboot" {
		t.Fatalf("state %+v, calls %v", st, g.calls)
	}
}

func TestFirmwareInstallWaitsForRestartAfterTestBoot(t *testing.T) {
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "2", bootedFrom(t, "2", "3"), nil) // still on the confirmed test boot
	boot := filepath.Join(f.DataDir, "boot")
	os.WriteFile(boot, []byte("b"), 0o600)
	if err := f.Install(context.Background(), "3", boot, boot); err == nil || len(g.calls) != 0 {
		t.Fatalf("installed over the running image: %v %v", err, g.calls)
	}
}

func TestFirmwareRevertsUnhealthyTestBoot(t *testing.T) {
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "2", bootedFrom(t, "2", "3"), errors.New("app is not running"))
	f.save(FirmwareState{Current: "1", Pending: &PendingFirmware{Version: "2"}})
	f.Resume(context.Background())
	st := f.load()
	if st.Failed["2"] == "" || st.Pending != nil || len(g.calls) != 1 || g.calls[0] != "reboot" {
		t.Fatalf("state %+v, calls %v", st, g.calls)
	}
}

func TestFirmwareRecordsTestBootThatNeverCameBack(t *testing.T) {
	g := &fakeGokrazy{}
	// Back on the old image (version 1), booted normally from partition 2.
	f := newFirmware(t, g, "1", bootedFrom(t, "2", "2"), nil)
	f.save(FirmwareState{Current: "1", Pending: &PendingFirmware{Version: "2"}})
	f.Resume(context.Background())
	if st := f.load(); st.Failed["2"] == "" || st.Current != "1" || len(g.calls) != 0 {
		t.Fatalf("state %+v, calls %v", st, g.calls)
	}
}

func TestFirmwareFromChannel(t *testing.T) {
	ctx := context.Background()
	h := newChannelHost(t)
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "1", bootedFrom(t, "2", "2"), nil)
	f.URL, f.Verifier, f.Identity = h.srv.URL+"/fw", h.signer, h.id

	publish := func(version string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		var parts []manifest.Part
		for _, name := range []string{"boot", "root"} {
			content := name + " of " + version
			var enc bytes.Buffer
			w, _ := age.Encrypt(&enc, h.id.Recipient())
			io.WriteString(w, content)
			w.Close()
			sum := sha256.Sum256([]byte(content))
			h.files["/blob/"+version+"-"+name] = enc.Bytes()
			parts = append(parts, manifest.Part{Name: name, Object: "../blob/" + version + "-" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))})
		}
		h.serials++
		env, _ := manifest.Sign(&manifest.Manifest{Name: "app-firmware", Version: version, Serial: h.serials, Arch: runtime.GOARCH, Kind: "firmware", Parts: parts}, h.signer)
		h.files["/fw/stable.json"] = env
	}

	publish("1") // what's already running: nothing to do
	if err := f.Check(ctx); err != nil || len(g.calls) != 0 {
		t.Fatalf("%v %v", err, g.calls)
	}
	publish("2")
	if err := f.Check(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"write root", "write bootonly", "testboot", "reboot"}
	if len(g.calls) != 4 || g.calls[0] != want[0] || g.calls[1] != want[1] || g.calls[2] != want[2] || g.calls[3] != want[3] {
		t.Fatalf("calls %v", g.calls)
	}
	if g.written["root"] != "root of 2" || g.written["bootonly"] != "boot of 2" {
		t.Fatalf("wrote %v", g.written)
	}
	if st := f.load(); st.Pending == nil || st.Pending.Version != "2" {
		t.Fatalf("state %+v", st)
	}
	// A version that failed its test boot isn't installed again.
	st := f.load()
	st.Pending, st.Failed = nil, map[string]string{"3": "failed its test boot"}
	f.save(st)
	g.calls = nil
	publish("3")
	if err := f.Check(ctx); err != nil || len(g.calls) != 0 {
		t.Fatalf("reinstalled a failed version: %v %v", err, g.calls)
	}
}

func TestOfficialFirmwareIsPlain(t *testing.T) {
	h := newChannelHost(t)
	g := &fakeGokrazy{}
	f := newFirmware(t, g, "1", bootedFrom(t, "2", "2"), nil)
	f.URL, f.Verifier = h.srv.URL+"/fw", h.signer // no age identity: not claimed
	var parts []manifest.Part
	for _, name := range []string{"boot", "root"} {
		content := name + " of 2"
		h.files["/blob/"+name] = []byte(content)
		sum := sha256.Sum256([]byte(content))
		parts = append(parts, manifest.Part{Name: name, Object: "../blob/" + name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))})
	}
	env, _ := manifest.Sign(&manifest.Manifest{Name: "r76s", Version: "2", Serial: 1, Arch: runtime.GOARCH, Kind: "firmware", Plain: true, Parts: parts}, h.signer)
	h.files["/fw/stable.json"] = env
	if err := f.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.written["root"] != "root of 2" || g.written["bootonly"] != "boot of 2" {
		t.Fatalf("wrote %v", g.written)
	}
}
