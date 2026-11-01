package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// lastUsableAligned is the last sector a partition may use: before the
// backup GPT (33 sectors at the end of the disk), rounded down so the
// partition ends on a 1 MiB boundary.
func lastUsableAligned(diskSectors uint64) uint64 {
	lastUsable := diskSectors - 34
	return (lastUsable+1)/2048*2048 - 1
}

const (
	gptHeaderSize = 92
	sector        = 512
)

type readWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// growGPT moves the backup GPT to the end of the disk and extends the
// partition starting at partStart to newLast. rw is the whole disk.
func growGPT(rw readWriterAt, diskSectors, partStart, newLast uint64) error {
	hdr := make([]byte, sector)
	if _, err := rw.ReadAt(hdr, sector); err != nil {
		return err
	}
	if !bytes.Equal(hdr[:8], []byte("EFI PART")) {
		return errors.New("no GPT")
	}
	if binary.LittleEndian.Uint32(hdr[12:]) != gptHeaderSize {
		return errors.New("unexpected GPT header size")
	}
	if got, want := crc32.ChecksumIEEE(withZeroCRC(hdr[:gptHeaderSize])), binary.LittleEndian.Uint32(hdr[16:]); got != want {
		return errors.New("GPT header checksum mismatch")
	}
	le := binary.LittleEndian
	entriesLBA := le.Uint64(hdr[72:])
	numEntries := le.Uint32(hdr[80:])
	entrySize := le.Uint32(hdr[84:])
	entries := make([]byte, int(numEntries)*int(entrySize))
	if _, err := rw.ReadAt(entries, int64(entriesLBA)*sector); err != nil {
		return err
	}
	if crc32.ChecksumIEEE(entries) != le.Uint32(hdr[88:]) {
		return errors.New("GPT entries checksum mismatch")
	}
	found := false
	for i := 0; i < int(numEntries); i++ {
		e := entries[i*int(entrySize):]
		if le.Uint64(e[32:]) == partStart {
			if le.Uint64(e[40:]) > newLast {
				return errors.New("the partition would shrink")
			}
			le.PutUint64(e[40:], newLast)
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no GPT entry starts at sector %d", partStart)
	}
	entriesSectors := (uint64(len(entries)) + sector - 1) / sector
	backupHdrLBA := diskSectors - 1
	backupEntriesLBA := backupHdrLBA - entriesSectors

	primary := append([]byte(nil), hdr[:gptHeaderSize]...)
	le.PutUint64(primary[32:], backupHdrLBA)       // AlternateLBA
	le.PutUint64(primary[48:], backupEntriesLBA-1) // LastUsableLBA
	le.PutUint32(primary[88:], crc32.ChecksumIEEE(entries))
	backup := append([]byte(nil), primary...)
	le.PutUint64(backup[24:], backupHdrLBA) // MyLBA
	le.PutUint64(backup[32:], 1)            // AlternateLBA
	le.PutUint64(backup[72:], backupEntriesLBA)
	for _, h := range [][]byte{primary, backup} {
		le.PutUint32(h[16:], crc32.ChecksumIEEE(withZeroCRC(h)))
	}

	// Backup first: until the primary changes, the old layout stays valid.
	if _, err := rw.WriteAt(entries, int64(backupEntriesLBA)*sector); err != nil {
		return err
	}
	if _, err := rw.WriteAt(pad(backup), int64(backupHdrLBA)*sector); err != nil {
		return err
	}
	if _, err := rw.WriteAt(entries, int64(entriesLBA)*sector); err != nil {
		return err
	}
	_, err := rw.WriteAt(pad(primary), sector)
	return err
}

func withZeroCRC(h []byte) []byte {
	c := append([]byte(nil), h[:gptHeaderSize]...)
	binary.LittleEndian.PutUint32(c[16:], 0)
	return c
}

func pad(h []byte) []byte {
	b := make([]byte, sector)
	copy(b, h)
	return b
}

// GPTPartition returns the first and last sector of partition n (1-based)
// in the GPT of the disk or image r.
func GPTPartition(r io.ReaderAt, n int) (first, last uint64, err error) {
	hdr := make([]byte, sector)
	if _, err := r.ReadAt(hdr, sector); err != nil {
		return 0, 0, err
	}
	if !bytes.Equal(hdr[:8], []byte("EFI PART")) {
		return 0, 0, errors.New("no GPT")
	}
	le := binary.LittleEndian
	entrySize := int64(le.Uint32(hdr[84:]))
	if n < 1 || uint32(n) > le.Uint32(hdr[80:]) {
		return 0, 0, fmt.Errorf("no partition %d", n)
	}
	e := make([]byte, entrySize)
	if _, err := r.ReadAt(e, int64(le.Uint64(hdr[72:]))*sector+int64(n-1)*entrySize); err != nil {
		return 0, 0, err
	}
	return le.Uint64(e[32:]), le.Uint64(e[40:]), nil
}

// reidentify gives the disk (written from a firmware image) its own ID:
// gokrazy derives the partition GUIDs from the image's hostname, so every
// device flashed from an image shares them, and the kernel finds the root
// partition by them. It replaces the 4 bytes that follow "8199-" in the
// disk and partition GUIDs with id, and returns the old and new IDs as the
// hex used on the kernel command line.
func reidentify(rw readWriterAt, id [4]byte) (oldHex, newHex string, err error) {
	hdr := make([]byte, sector)
	if _, err := rw.ReadAt(hdr, sector); err != nil {
		return "", "", err
	}
	if !bytes.Equal(hdr[:8], []byte("EFI PART")) {
		return "", "", errors.New("no GPT")
	}
	// In a GUID's on-disk form, bytes 10..13 are the first four of its last
	// group (…-AA4A34D90001 → AA 4A 34 D9); bytes 14..15 number the partition.
	old := append([]byte(nil), hdr[56+10:56+14]...)
	backupLBA := binary.LittleEndian.Uint64(hdr[32:])
	// Primary, then the backup copy (which a disk tool would compare).
	for _, lba := range []uint64{1, backupLBA} {
		err := reidentifyHeader(rw, lba, old, id)
		if errors.Is(err, errNotGPT) && lba != 1 {
			continue // no backup copy to keep in step
		}
		if err != nil {
			return "", "", fmt.Errorf("GPT header at %d: %w", lba, err)
		}
	}
	return fmt.Sprintf("%x", old), fmt.Sprintf("%x", id[:]), nil
}

var errNotGPT = errors.New("not a GPT header")

func reidentifyHeader(rw readWriterAt, lba uint64, old []byte, id [4]byte) error {
	hdr := make([]byte, sector)
	if _, err := rw.ReadAt(hdr, int64(lba)*sector); err != nil {
		return err
	}
	if !bytes.Equal(hdr[:8], []byte("EFI PART")) {
		return errNotGPT
	}
	le := binary.LittleEndian
	entriesLBA := le.Uint64(hdr[72:])
	entrySize := int(le.Uint32(hdr[84:]))
	entries := make([]byte, int(le.Uint32(hdr[80:]))*entrySize)
	if _, err := rw.ReadAt(entries, int64(entriesLBA)*sector); err != nil {
		return err
	}
	if bytes.Equal(hdr[56+10:56+14], old) {
		copy(hdr[56+10:], id[:])
	}
	for i := 0; i+entrySize <= len(entries); i += entrySize {
		g := entries[i+16 : i+32]
		if bytes.Equal(g[10:14], old) {
			copy(g[10:], id[:])
		}
	}
	le.PutUint32(hdr[88:], crc32.ChecksumIEEE(entries))
	le.PutUint32(hdr[16:], crc32.ChecksumIEEE(withZeroCRC(hdr[:gptHeaderSize])))
	if _, err := rw.WriteAt(entries, int64(entriesLBA)*sector); err != nil {
		return err
	}
	_, err := rw.WriteAt(hdr, int64(lba)*sector)
	return err
}

// replaceBootID rewrites "8199-<oldHex>" to "8199-<newHex>" (equal lengths)
// in a boot partition's bytes: its cmdline.txt and systemd-boot entry name
// the root partition by GUID.
func replaceBootID(b []byte, oldHex, newHex string) int {
	from, to := []byte("8199-"+oldHex), []byte("8199-"+newHex)
	n := bytes.Count(b, from)
	copy(b, bytes.ReplaceAll(b, from, to))
	return n
}
