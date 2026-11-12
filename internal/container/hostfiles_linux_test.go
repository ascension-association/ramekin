package container

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ascension-association/ramekin/internal/config"
	"golang.org/x/sys/unix"
)

func TestHostMirrorSync(t *testing.T) {
	src, dir := t.TempDir(), filepath.Join(t.TempDir(), "mirror")
	m := &hostMirror{src: src, dir: dir}
	read := func(name string) (string, bool) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b), true
	}

	// At boot on gokrazy, resolv.conf has no nameservers yet.
	os.WriteFile(filepath.Join(src, "resolv.conf"), nil, 0o644)
	os.WriteFile(filepath.Join(src, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644)
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	if got, ok := read("resolv.conf"); !ok || got != "" {
		t.Errorf("resolv.conf = %q, %v; want an empty file", got, ok)
	}
	if got, _ := read("hosts"); got != "127.0.0.1 localhost\n" {
		t.Errorf("hosts = %q", got)
	}

	// The DHCP client renames a new resolv.conf into place.
	tmp := filepath.Join(src, ".resolv.conf.new")
	os.WriteFile(tmp, []byte("nameserver 8.8.8.8\n"), 0o644)
	if err := os.Rename(tmp, filepath.Join(src, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	if got, _ := read("resolv.conf"); got != "nameserver 8.8.8.8\n" {
		t.Errorf("after the lease, resolv.conf = %q", got)
	}

	// A file removed on the host is removed from the mirror.
	os.Remove(filepath.Join(src, "hosts"))
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	if _, ok := read("hosts"); ok {
		t.Error("hosts still mirrored after the host removed it")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("mirror holds %d entries, want only resolv.conf (no temporary files)", len(ents))
	}
}

func TestBoundOver(t *testing.T) {
	binds := []config.Bind{{Src: "data", Dst: "/data"}, {Src: "/x", Dst: "/etc/hosts"}}
	for path, want := range map[string]bool{
		"/etc/hosts": true, "/etc/resolv.conf": false, "/data/x": true, "/datax": false,
	} {
		if got := boundOver(binds, path); got != want {
			t.Errorf("boundOver(%s) = %v, want %v", path, got, want)
		}
	}
	if !boundOver([]config.Bind{{Src: "/e", Dst: "/etc"}}, "/etc/resolv.conf") {
		t.Error("a bind of /etc should cover /etc/resolv.conf")
	}
}

// TestLinkHostFilesFollowsRenames needs to mount; run it in a throwaway
// mount namespace:
//
//	RAMEKIN_MOUNT_TEST=1 unshare -Urm go test -run LinkHostFiles ./internal/container
func TestLinkHostFilesFollowsRenames(t *testing.T) {
	if os.Getenv("RAMEKIN_MOUNT_TEST") == "" {
		t.Skip("set RAMEKIN_MOUNT_TEST=1 and run in a mount namespace (unshare -Urm)")
	}
	src, mirror, root := t.TempDir(), filepath.Join(t.TempDir(), "mirror"), t.TempDir()
	os.WriteFile(filepath.Join(src, "resolv.conf"), nil, 0o644)
	os.WriteFile(filepath.Join(src, "hosts"), []byte("127.0.0.1 localhost\n"), 0o644)
	// The image's own files, which the links replace.
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "resolv.conf"), []byte("nameserver 9.9.9.9\n"), 0o644)

	m := &hostMirror{src: src, dir: mirror}
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	// The user binds their own hosts file, which must be left alone.
	userHosts := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(userHosts, []byte("10.0.0.1 mine\n"), 0o644)
	binds := []config.Bind{{Src: userHosts, Dst: "/etc/hosts", ReadOnly: true}}
	t.Cleanup(func() { // before TempDir's cleanup, which can't remove mounts
		unix.Unmount(filepath.Join(root, hostFilesMount), unix.MNT_DETACH)
		unix.Unmount(filepath.Join(root, "etc", "hosts"), unix.MNT_DETACH)
	})
	for _, b := range binds {
		if err := bindMount(root, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := linkHostFiles(root, mirror, binds); err != nil {
		t.Fatal(err)
	}

	// Read the container's files as the container would: through its
	// symlinks, resolved inside root.
	read := func(name string) string {
		t.Helper()
		p := filepath.Join(root, "etc", name)
		if l, err := os.Readlink(p); err == nil {
			p = filepath.Join(root, l)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("resolv.conf"); got != "" {
		t.Errorf("before the lease, the container's resolv.conf = %q, want the host's empty file", got)
	}
	if got := read("hosts"); got != "10.0.0.1 mine\n" {
		t.Errorf("hosts = %q, want the user's bind", got)
	}

	// The DHCP client renames a new file into place on the host.
	tmp := filepath.Join(src, ".new")
	os.WriteFile(tmp, []byte("nameserver 8.8.8.8\n"), 0o644)
	os.Rename(tmp, filepath.Join(src, "resolv.conf"))
	if err := m.sync(); err != nil {
		t.Fatal(err)
	}
	if got := read("resolv.conf"); got != "nameserver 8.8.8.8\n" {
		t.Errorf("after the lease, the container's resolv.conf = %q, want the new nameserver", got)
	}

	// The mirror is read-only in the container.
	if err := os.WriteFile(filepath.Join(root, hostFilesMount, "resolv.conf"), []byte("x"), 0o644); err == nil {
		t.Error("the container could write the mirrored resolv.conf")
	}
}
