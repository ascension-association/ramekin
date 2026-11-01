package agent

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// memDisk is a disk image in memory.
type memDisk []byte

func (d memDisk) ReadAt(p []byte, off int64) (int, error)  { return copy(p, d[off:]), nil }
func (d memDisk) WriteAt(p []byte, off int64) (int, error) { return copy(d[off:], p), nil }

// newGPTDisk lays out a GPT for an image of imageSectors on a disk of
// diskSectors, with one partition from start to the end of the image.
func newGPTDisk(diskSectors, imageSectors, start uint64) memDisk {
	d := make(memDisk, diskSectors*sector)
	le := binary.LittleEndian
	entries := make([]byte, 128*128)
	le.PutUint64(entries[32:], start)
	le.PutUint64(entries[40:], imageSectors-34)
	h := make([]byte, gptHeaderSize)
	copy(h, "EFI PART")
	le.PutUint32(h[8:], 0x00010000)
	le.PutUint32(h[12:], gptHeaderSize)
	le.PutUint64(h[24:], 1)
	le.PutUint64(h[32:], imageSectors-1)
	le.PutUint64(h[40:], 34)
	le.PutUint64(h[48:], imageSectors-34)
	le.PutUint64(h[72:], 2)
	le.PutUint32(h[80:], 128)
	le.PutUint32(h[84:], 128)
	le.PutUint32(h[88:], crc32.ChecksumIEEE(entries))
	le.PutUint32(h[16:], crc32.ChecksumIEEE(h))
	copy(d[sector:], h)
	copy(d[2*sector:], entries)
	return d
}

func TestGrowGPT(t *testing.T) {
	const disk, image, start = 1 << 16, 1 << 14, 2048
	d := newGPTDisk(disk, image, start)
	newLast := lastUsableAligned(disk)
	if err := growGPT(d, disk, start, newLast); err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	for _, at := range []uint64{1, disk - 1} { // primary and backup headers
		h := d[at*sector : at*sector+gptHeaderSize]
		if !bytes.Equal(h[:8], []byte("EFI PART")) || crc32.ChecksumIEEE(withZeroCRC(h)) != le.Uint32(h[16:]) {
			t.Fatalf("header at %d invalid", at)
		}
		if le.Uint64(h[24:]) != at {
			t.Fatalf("header at %d says it's at %d", at, le.Uint64(h[24:]))
		}
		el := le.Uint64(h[72:])
		entries := d[el*sector : el*sector+128*128]
		if crc32.ChecksumIEEE(entries) != le.Uint32(h[88:]) || le.Uint64(entries[40:]) != newLast {
			t.Fatalf("entries for header at %d: last LBA %d, want %d", at, le.Uint64(entries[40:]), newLast)
		}
	}
	if (newLast+1)%2048 != 0 || newLast > disk-34 {
		t.Fatalf("bad end %d", newLast)
	}
	// Growing again is harmless; shrinking is refused.
	if err := growGPT(d, disk, start, newLast); err != nil {
		t.Fatal(err)
	}
	if growGPT(d, disk, start, newLast-2048) == nil {
		t.Fatal("shrank the partition")
	}
}

func TestReidentify(t *testing.T) {
	d := newGPTDisk(1<<14, 1<<14, 2048)
	le := binary.LittleEndian
	// Give the disk and partition gokrazy-style GUIDs: …8199-AA4A34D9000N.
	guid := func(n byte) []byte {
		return []byte{0xc1, 0x4c, 0xc2, 0x60, 0xf9, 0xf3, 0x7a, 0x42, 0x81, 0x99, 0xaa, 0x4a, 0x34, 0xd9, 0x00, n}
	}
	hdr := d[sector : sector+gptHeaderSize]
	copy(hdr[56:], guid(0))
	copy(d[2*sector+16:], guid(1))
	le.PutUint32(hdr[88:], crc32.ChecksumIEEE(d[2*sector:2*sector+128*128]))
	le.PutUint32(hdr[16:], crc32.ChecksumIEEE(withZeroCRC(hdr)))

	oldHex, newHex, err := reidentify(d, [4]byte{0x12, 0x34, 0x56, 0x78})
	if err != nil || oldHex != "aa4a34d9" || newHex != "12345678" {
		t.Fatalf("%s %s %v", oldHex, newHex, err)
	}
	hdr = d[sector : sector+gptHeaderSize]
	if crc32.ChecksumIEEE(withZeroCRC(hdr)) != le.Uint32(hdr[16:]) || crc32.ChecksumIEEE(d[2*sector:2*sector+128*128]) != le.Uint32(hdr[88:]) {
		t.Fatal("checksums not updated")
	}
	if g := d[2*sector+16 : 2*sector+32]; !bytes.Equal(g[10:16], []byte{0x12, 0x34, 0x56, 0x78, 0x00, 0x01}) {
		t.Fatalf("partition GUID %x", g)
	}
	boot := []byte("console=tty1 root=PARTUUID=60c24cc1-f3f9-427a-8199-aa4a34d90001/PARTNROFF=1 ro\x00options root=PARTUUID=60c24cc1-f3f9-427a-8199-aa4a34d90001/PARTNROFF=1")
	if n := replaceBootID(boot, oldHex, newHex); n != 2 || bytes.Contains(boot, []byte("aa4a34d9")) {
		t.Fatalf("replaced %d: %s", n, boot)
	}
}
