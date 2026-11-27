//go:build linux

package netbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// sysfs is where port state is read from; tests may point it elsewhere.
var sysfs = "/sys"

// forwardDelay is the STP forward delay in clock ticks (USER_HZ, 1/100 s):
// the kernel's minimum, 2 s, so a port that links up forwards after about
// 4 s instead of 30. STP is on so that two cables into the same switch don't
// make a loop that floods the LAN.
const forwardDelay = 200

// Keep makes sure the bridge exists and every port that exists is in it and
// up, now and every couple of seconds until ctx ends: ports can appear late
// at boot (PCIe NICs), and something could take one out. Changes in what it
// finds are logged.
func Keep(ctx context.Context, s Spec, logf func(format string, args ...any)) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	last := ""
	for {
		msg := fmt.Sprintf("bridge %s: up with %s", s.Name, strings.Join(s.Ports, ", "))
		if missing, err := Ensure(s); err != nil {
			msg = fmt.Sprintf("bridge %s: %v", s.Name, err)
		} else if len(missing) > 0 {
			msg = fmt.Sprintf("bridge %s: up; waiting for %s to appear", s.Name, strings.Join(missing, ", "))
		}
		if msg != last {
			logf("%s", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Ensure creates the bridge if it's missing (once its first port exists, for
// the MAC address), puts every existing port in it, and brings all of them
// up. It returns the ports that don't exist yet.
func Ensure(s Spec) (missing []string, err error) {
	br, err := net.InterfaceByName(s.Name)
	if err != nil {
		first, err := net.InterfaceByName(s.Ports[0])
		if err != nil {
			return s.Ports, fmt.Errorf("waiting for %s to appear, to create the bridge", s.Ports[0])
		}
		if err := createBridge(s.Name, first.HardwareAddr); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("creating the bridge: %w", err)
		}
		if br, err = net.InterfaceByName(s.Name); err != nil {
			return nil, err
		}
	}
	if !isBridge(s.Name) {
		return nil, fmt.Errorf("%s exists but isn't a bridge", s.Name)
	}
	for _, p := range s.Ports {
		ifc, err := net.InterfaceByName(p)
		if err != nil {
			missing = append(missing, p)
			continue
		}
		if masterOf(p) != s.Name || ifc.Flags&net.FlagUp == 0 {
			if err := setLink(ifc.Index, br.Index); err != nil {
				return nil, fmt.Errorf("adding %s: %w", p, err)
			}
		}
	}
	if br.Flags&net.FlagUp == 0 {
		if err := setLink(br.Index, 0); err != nil {
			return nil, fmt.Errorf("bringing it up: %w", err)
		}
	}
	return missing, nil
}

func isBridge(name string) bool {
	_, err := os.Stat(filepath.Join(sysfs, "class", "net", name, "bridge"))
	return err == nil
}

// masterOf returns the bridge (or other master) port is in, if any.
func masterOf(port string) string {
	l, err := os.Readlink(filepath.Join(sysfs, "class", "net", port, "master"))
	if err != nil {
		return ""
	}
	return filepath.Base(l)
}

// createBridge adds a bridge with STP on and the given MAC address.
func createBridge(name string, mac net.HardwareAddr) error {
	brData := append(attr(unix.IFLA_BR_FORWARD_DELAY, u32(forwardDelay)),
		attr(unix.IFLA_BR_STP_STATE, u32(1))...)
	linkInfo := append(attr(unix.IFLA_INFO_KIND, []byte("bridge")),
		attr(unix.IFLA_INFO_DATA, brData)...)
	attrs := attr(unix.IFLA_IFNAME, append([]byte(name), 0))
	if len(mac) > 0 {
		attrs = append(attrs, attr(unix.IFLA_ADDRESS, mac)...)
	}
	attrs = append(attrs, attr(unix.IFLA_LINKINFO, linkInfo)...)
	return request(unix.NLM_F_CREATE|unix.NLM_F_EXCL, ifinfo(0), attrs)
}

// setLink brings interface index up and, if master isn't 0, puts it in
// that bridge.
func setLink(index, master int) error {
	var attrs []byte
	if master != 0 {
		attrs = attr(unix.IFLA_MASTER, u32(uint32(master)))
	}
	return request(0, ifinfo(index), attrs)
}

// ifinfo encodes a struct ifinfomsg that brings the interface up.
func ifinfo(index int) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	b[0] = unix.AF_UNSPEC
	binary.NativeEndian.PutUint32(b[4:], uint32(int32(index)))
	binary.NativeEndian.PutUint32(b[8:], unix.IFF_UP)  // flags
	binary.NativeEndian.PutUint32(b[12:], unix.IFF_UP) // change
	return b
}

func u32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }

// attr encodes one rtnetlink attribute, padded to 4 bytes.
func attr(typ uint16, data []byte) []byte {
	n := unix.SizeofRtAttr + len(data)
	b := make([]byte, (n+3)&^3)
	binary.NativeEndian.PutUint16(b, uint16(n))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[unix.SizeofRtAttr:], data)
	return b
}

// request sends one RTM_NEWLINK request and waits for its acknowledgment.
func request(flags uint16, ifi, attrs []byte) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, sa); err != nil {
		return err
	}
	body := append(ifi, attrs...)
	msg := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(body))
	binary.NativeEndian.PutUint32(msg, uint32(unix.NLMSG_HDRLEN+len(body)))
	binary.NativeEndian.PutUint16(msg[4:], unix.RTM_NEWLINK)
	binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK|flags)
	binary.NativeEndian.PutUint32(msg[8:], 1) // sequence number
	msg = append(msg, body...)
	if err := unix.Sendto(fd, msg, 0, sa); err != nil {
		return err
	}
	buf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(m.Data) < 4 {
				return errors.New("short netlink error message")
			}
			if errno := int32(binary.NativeEndian.Uint32(m.Data)); errno != 0 {
				return syscall.Errno(-errno)
			}
			return nil
		}
	}
}
