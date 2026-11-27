//go:build linux

package netbridge

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEnsureNetns builds a real bridge. It changes the host's interfaces, so
// it runs only in a throwaway network namespace with its own sysfs:
//
//	unshare -rnm sh -c 'mount -t sysfs sysfs /sys && RAMEKIN_NETNS_TEST=1 go test -run Netns ./internal/netbridge'
func TestEnsureNetns(t *testing.T) {
	if os.Getenv("RAMEKIN_NETNS_TEST") != "1" {
		t.Skip("set RAMEKIN_NETNS_TEST=1 inside unshare -rnm (see the comment)")
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(sysfs, "class", "net", p))
		return strings.TrimSpace(string(b))
	}
	s := Spec{Name: "br0", Ports: []string{"a0", "b0"}}

	if _, err := Ensure(s); err == nil {
		t.Fatal("Ensure succeeded with no ports")
	}
	ip("link", "add", "a0", "type", "veth", "peer", "name", "a1")
	missing, err := Ensure(s)
	if err != nil || !slices.Equal(missing, []string{"b0"}) {
		t.Fatalf("Ensure = %v, %v; want b0 missing", missing, err)
	}
	ip("link", "add", "b0", "type", "veth", "peer", "name", "b1")
	if missing, err := Ensure(s); err != nil || len(missing) != 0 {
		t.Fatalf("Ensure = %v, %v", missing, err)
	}
	// Taken out again: Ensure puts it back.
	ip("link", "set", "b0", "nomaster")
	if _, err := Ensure(s); err != nil {
		t.Fatal(err)
	}

	a0, _ := net.InterfaceByName("a0")
	br, _ := net.InterfaceByName("br0")
	if br.HardwareAddr.String() != a0.HardwareAddr.String() {
		t.Errorf("br0 MAC %s, want a0's %s", br.HardwareAddr, a0.HardwareAddr)
	}
	for _, p := range s.Ports {
		if m := masterOf(p); m != "br0" {
			t.Errorf("%s master = %q, want br0", p, m)
		}
		if ifc, _ := net.InterfaceByName(p); ifc.Flags&net.FlagUp == 0 {
			t.Errorf("%s is down", p)
		}
	}
	if br.Flags&net.FlagUp == 0 {
		t.Error("br0 is down")
	}
	if v := read("br0/bridge/stp_state"); v != "1" {
		t.Errorf("stp_state = %q, want 1", v)
	}
	if v := read("br0/bridge/forward_delay"); v != "200" {
		t.Errorf("forward_delay = %q, want 200", v)
	}
}
