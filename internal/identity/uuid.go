package identity

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
