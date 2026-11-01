//go:build linux

package agent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A flashed card's /perm partition is only as large as the image it came
// from. GrowPerm extends it to the end of the card, once: it rewrites the
// GPT (both copies), tells the kernel the partition's new size, and grows
// the mounted ext4 file system online. Devices whose /perm already fills the
// disk are left alone.
func GrowPerm(logger *log.Logger) error {
	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	dev, ok := mountDevice(string(mi), "/perm")
	if !ok {
		return nil // not mounted (yet): gokrazy/mkfs creates it first
	}
	part, err := filepath.EvalSymlinks(filepath.Join("/sys/dev/block", dev))
	if err != nil {
		return err
	}
	disk := filepath.Dir(part)
	start, err := readSysInt(filepath.Join(part, "start"))
	if err != nil {
		return err
	}
	size, err := readSysInt(filepath.Join(part, "size"))
	if err != nil {
		return err
	}
	pno, err := readSysInt(filepath.Join(part, "partition"))
	if err != nil {
		return err
	}
	diskSectors, err := readSysInt(filepath.Join(disk, "size"))
	if err != nil {
		return err
	}
	newLast := lastUsableAligned(uint64(diskSectors))
	if uint64(start+size-1) >= newLast {
		return nil // already fills the disk
	}

	// Growing small-block ext4 online is a rarely used kernel path (and has
	// been buggy); only grow file systems with 4 KiB blocks, as images get.
	var st unix.Statfs_t
	if err := unix.Statfs("/perm", &st); err != nil {
		return err
	}
	if st.Bsize < 4096 {
		return fmt.Errorf("/perm has %d-byte blocks; not growing it online (only 4 KiB blocks are)", st.Bsize)
	}
	devPath := "/dev/" + filepath.Base(disk)
	logger.Printf("growing /perm (%s partition %d) from %d MiB to the end of the card (%d MiB)",
		devPath, pno, size>>11, (int64(newLast)-start+1)>>11)
	f, err := os.OpenFile(devPath, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := growGPT(f, uint64(diskSectors), uint64(start), newLast); err != nil {
		return fmt.Errorf("partition table: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	newSize := int64(newLast) - start + 1
	if err := blkpgResize(f, int32(pno), start*512, newSize*512); err != nil {
		return fmt.Errorf("telling the kernel the new partition size: %w", err)
	}
	if err := resizeExt4("/perm", newSize*512); err != nil {
		return fmt.Errorf("growing the file system: %w", err)
	}
	logger.Printf("/perm now fills the card")
	return nil
}

func readSysInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// mountDevice returns the device number of the last mount on mountpoint.
func mountDevice(mountinfo, mountpoint string) (string, bool) {
	var dev string
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == mountpoint {
			dev = fields[2]
		}
	}
	return dev, dev != ""
}

// blkpgResize tells the kernel a partition's new extent (BLKPG_RESIZE_PARTITION),
// which works while it's mounted.
func blkpgResize(disk *os.File, pno int32, start, length int64) error {
	type blkpgPartition struct {
		Start, Length int64
		Pno           int32
		Devname       [64]byte
		Volname       [64]byte
	}
	type blkpgIoctlArg struct {
		Op, Flags, Datalen int32
		Data               uintptr
	}
	p := blkpgPartition{Start: start, Length: length, Pno: pno}
	arg := blkpgIoctlArg{Op: 3 /* BLKPG_RESIZE_PARTITION */, Datalen: int32(unsafe.Sizeof(p)), Data: uintptr(unsafe.Pointer(&p))}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, disk.Fd(), 0x1269 /* BLKPG */, uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return errno
	}
	return nil
}

// resizeExt4 grows the mounted ext4 file system at dir to size bytes
// (EXT4_IOC_RESIZE_FS).
func resizeExt4(dir string, size int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return err
	}
	blocks := uint64(size) / uint64(st.Bsize)
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x40086610 /* EXT4_IOC_RESIZE_FS */, uintptr(unsafe.Pointer(&blocks)))
	if errno != 0 {
		return errno
	}
	return nil
}
