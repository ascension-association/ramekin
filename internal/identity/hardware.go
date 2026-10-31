package identity

import (
	"bufio"
	"bytes"
	"os"
	"strings"
)

// HardwareID returns a stable identifier of the board, from the first
// source that has one: the device tree serial number (Raspberry Pi, most
// Rockchip boards), the DMI product UUID (PCs and VMs), or the CPU serial
// in /proc/cpuinfo. Each is prefixed with its source. It returns "" if the
// board exposes none.
func HardwareID() string { return hardwareID("/") }

func hardwareID(root string) string {
	read := func(p string) string {
		b, _ := os.ReadFile(root + p)
		return strings.TrimSpace(string(bytes.Trim(b, "\x00")))
	}
	if s := read("sys/firmware/devicetree/base/serial-number"); s != "" {
		return "dt:" + s
	}
	if s := read("sys/class/dmi/id/product_uuid"); s != "" && s != "00000000-0000-0000-0000-000000000000" {
		return "dmi:" + strings.ToLower(s)
	}
	f, err := os.Open(root + "proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "Serial" {
			if v = strings.TrimSpace(v); strings.Trim(v, "0") != "" {
				return "cpu:" + v
			}
		}
	}
	return ""
}
