package identity

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// uuidNamespace is Ramekin's UUIDv5 namespace for device UUIDs. It must
// never change: every device's UUID, and every sticker printed with it,
// depends on it.
var uuidNamespace = [16]byte{0x4e, 0x67, 0x28, 0xa8, 0x5c, 0x45, 0x47, 0x7d, 0xb0, 0x1a, 0x3b, 0x94, 0xed, 0x71, 0xb7, 0x77} // 4e6728a8-5c45-477d-b01a-3b94ed71b777

// uuidFile holds the random UUID of a board with no hardware ID.
const uuidFile = "uuid"

// DeviceUUID is a board's permanent UUID, derived from its hardware ID
// (UUIDv5), so it survives firmware updates, reflashing and moving to
// another project. It is "" for a board with no hardware ID.
func DeviceUUID(hardwareID string) string {
	if hardwareID == "" {
		return ""
	}
	h := sha1.New()
	h.Write(uuidNamespace[:])
	h.Write([]byte(hardwareID))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50 // version 5
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return formatUUID(u)
}

// UUID returns the device's permanent UUID: derived from its hardware ID,
// or, for a board with none, a random one kept in the identity directory
// (on /perm, so it survives firmware updates, but not a reflash).
func (i *Identity) UUID(hardwareID string) (string, error) {
	if u := DeviceUUID(hardwareID); u != "" {
		return u, nil
	}
	path := filepath.Join(i.Dir, uuidFile)
	if b, err := os.ReadFile(path); err == nil {
		if u := strings.TrimSpace(string(b)); ValidUUID(u) {
			return u, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80
	s := formatUUID(u)
	if err := writeFileAtomic(path, []byte(s+"\n")); err != nil {
		return "", fmt.Errorf("saving the device UUID: %w", err)
	}
	return s, nil
}

// crockford is Crockford's base32 alphabet: digits and capitals without
// I, L, O and U, so it's easy to read back from a label.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ShortID is a 10-character form of a device UUID for labels, such as
// Q5RNK-2MXD7: 50 bits of it in Crockford base32. It's short enough to fit
// the smallest QR code (alphanumeric mode) and to print legibly on a small
// sticker. It is "" if u isn't a valid UUID.
//
// The bits come from bytes 0-5 and the top of byte 7, skipping the version
// in byte 6, so they're all hash output (UUIDv5) or random (UUIDv4). With 50
// bits, two devices of a 50,000-device fleet share one with a probability
// of about 1 in 2 million; ShortIDClashes finds them.
func ShortID(u string) string {
	if !ValidUUID(u) {
		return ""
	}
	b, _ := hex.DecodeString(strings.ReplaceAll(u, "-", ""))
	var n uint64
	for _, x := range b[:6] {
		n = n<<8 | uint64(x)
	}
	n = n<<2 | uint64(b[7]>>6)
	var s [10]byte
	for i := 9; i >= 0; i-- {
		s[i] = crockford[n&31]
		n >>= 5
	}
	return string(s[:5]) + "-" + string(s[5:])
}

// ShortIDClashes groups names (to UUIDs) whose UUIDs differ but share a
// short ID. It returns each clashing short ID with its names, sorted.
func ShortIDClashes(uuids map[string]string) map[string][]string {
	byID := map[string]map[string]string{} // short ID -> name -> UUID
	for name, u := range uuids {
		if id := ShortID(u); id != "" {
			if byID[id] == nil {
				byID[id] = map[string]string{}
			}
			byID[id][name] = u
		}
	}
	out := map[string][]string{}
	for id, names := range byID {
		distinct := map[string]bool{}
		for _, u := range names {
			distinct[u] = true
		}
		if len(distinct) < 2 {
			continue // the same device under two names isn't a clash
		}
		for name := range names {
			out[id] = append(out[id], name)
		}
		sort.Strings(out[id])
	}
	return out
}

func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ValidUUID reports whether s is a UUID in its canonical lowercase form.
func ValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
