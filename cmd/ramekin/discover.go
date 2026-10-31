package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Devices get their address from DHCP, so it can change across reboots. The
// workstation remembers each device's MAC address (learned from its neighbor
// table after a successful connection) and, when the device stops answering,
// finds the MAC's new IP on the local network.

// neighbors returns the workstation's IPv4 neighbor table as IP -> MAC
// (lowercase), with only resolved entries.
func neighbors() map[string]string {
	if b, err := os.ReadFile("/proc/net/arp"); err == nil {
		return parseProcARP(b)
	}
	// macOS and BSDs.
	b, err := exec.Command("arp", "-an").Output()
	if err != nil {
		return map[string]string{}
	}
	return parseArpAn(b)
}

// parseProcARP parses Linux's /proc/net/arp:
// IP address, HW type, Flags, HW address, Mask, Device.
// Flags 0x0 marks unresolved (incomplete) entries.
func parseProcARP(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 4 && f[2] != "0x0" && f[3] != "00:00:00:00:00:00" {
			out[f[0]] = strings.ToLower(f[3])
		}
	}
	return out
}

var arpAnLine = regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\) at ([0-9a-fA-F:]+)`)

// parseArpAn parses `arp -an` output: "? (192.168.1.5) at aa:bb:cc:dd:ee:ff on en0 ...".
// Unresolved entries print "(incomplete)" and don't match.
func parseArpAn(b []byte) map[string]string {
	out := map[string]string{}
	for _, m := range arpAnLine.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = normalizeMAC(m[2])
	}
	return out
}

// normalizeMAC zero-pads octets (macOS prints "a:b:..." for "0a:0b:...").
func normalizeMAC(s string) string {
	parts := strings.Split(strings.ToLower(s), ":")
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// macFor returns the MAC address the workstation has for host, if any.
func macFor(host string) string {
	ips, err := net.LookupHost(host)
	if err != nil {
		return ""
	}
	tbl := neighbors()
	for _, ip := range ips {
		if mac := tbl[ip]; mac != "" {
			return mac
		}
	}
	return ""
}

// localSubnets lists the IPv4 networks of the workstation's physical
// interfaces, clamped to the /24 around the workstation's address.
func localSubnets() []netip.Prefix {
	ifaces, _ := net.Interfaces()
	var out []netip.Prefix
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualInterface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP.To4())
			if !ok || !addr.IsPrivate() {
				continue
			}
			bits, _ := ipn.Mask.Size()
			out = append(out, netip.PrefixFrom(addr, max(bits, 24)).Masked())
		}
	}
	return out
}

func virtualInterface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "podman", "tailscale", "wg", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// sweep touches every address in the subnets so the kernel resolves them,
// filling the neighbor table. The connection results themselves are ignored.
func sweep(ctx context.Context, subnets []netip.Prefix, port string) {
	sem := make(chan struct{}, 256)
	var wg sync.WaitGroup
	for _, p := range subnets {
		for a := p.Addr().Next(); p.Contains(a) && a.Next().IsValid() && p.Contains(a.Next()); a = a.Next() {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(a netip.Addr) {
				defer func() { <-sem; wg.Done() }()
				d := net.Dialer{Timeout: time.Second}
				if c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port)); err == nil {
					c.Close()
				}
			}(a)
		}
	}
	wg.Wait()
}

// findByMAC returns the IP currently holding mac that accepts connections on
// port. The neighbor table can keep stale entries for a device's previous
// addresses, so every candidate is checked.
func findByMAC(ctx context.Context, mac, port string) (string, error) {
	mac = normalizeMAC(mac)
	try := func() string {
		for ip, m := range neighbors() {
			if m == mac && reachable(ip, port) {
				return ip
			}
		}
		return ""
	}
	if ip := try(); ip != "" {
		return ip, nil
	}
	subnets := localSubnets()
	if len(subnets) == 0 {
		return "", fmt.Errorf("no local IPv4 network to search for %s", mac)
	}
	sweep(ctx, subnets, port)
	if ip := try(); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no device with MAC %s answering on port %s in %v", mac, port, subnets)
}
