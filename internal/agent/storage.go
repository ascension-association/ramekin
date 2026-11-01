package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Disk is a block device the device has, as reported in its status.
type Disk struct {
	Name      string `json:"name"` // e.g. mmcblk0, nvme0n1
	Kind      string `json:"kind"` // sd, emmc, nvme, usb or other
	SizeBytes int64  `json:"size_bytes"`
	Model     string `json:"model,omitempty"`
	Boot      bool   `json:"boot,omitempty"` // holds the running system
}

// Disks lists the device's disks (not partitions, RAM or loop devices).
func Disks(sysfs, bootDisk string) []Disk {
	ents, _ := os.ReadDir(filepath.Join(sysfs, "block"))
	var out []Disk
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
			strings.HasPrefix(name, "mtdblock") || strings.Contains(name, "boot") || strings.HasSuffix(name, "rpmb") {
			continue
		}
		dir := filepath.Join(sysfs, "block", name)
		read := func(p string) string {
			b, _ := os.ReadFile(filepath.Join(dir, p))
			return strings.TrimSpace(string(b))
		}
		sectors, _ := strconv.ParseInt(read("size"), 10, 64)
		if sectors == 0 {
			continue
		}
		d := Disk{Name: name, SizeBytes: sectors * 512, Model: read("device/model"), Boot: name == bootDisk}
		if d.Model == "" {
			d.Model = read("device/name") // MMC cards and eMMC
		}
		switch {
		case strings.HasPrefix(name, "nvme"):
			d.Kind = "nvme"
		case strings.HasPrefix(name, "mmcblk"):
			d.Kind = "sd"
			if read("device/type") == "MMC" {
				d.Kind = "emmc"
			}
		case strings.HasPrefix(name, "sd"):
			d.Kind = "usb"
		default:
			d.Kind = "other"
		}
		out = append(out, d)
	}
	return out
}

// bootDiskName returns the disk holding the running root ("" if unknown).
func bootDiskName() string {
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	dev, ok := mountedRootDevice(string(mi))
	if !ok {
		return ""
	}
	part, err := filepath.EvalSymlinks(filepath.Join("/sys/dev/block", dev))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(part))
}
