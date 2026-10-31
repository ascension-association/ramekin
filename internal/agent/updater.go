package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/store"
)

type Updater struct {
	Sup      *Supervisor
	Store    *store.Store
	Channel  string
	Verifier signature.Verifier
	Identity age.Identity
	Interval time.Duration
	Device   string
	Report   bool
	Log      *log.Logger

	mu      sync.Mutex
	lastErr string
	checked time.Time
	kick    chan struct{}
}

func (u *Updater) Run(ctx context.Context) {
	kick := u.kickCh()
	// Spread a fleet's first poll so a power restoration doesn't stampede.
	wait := time.Duration(rand.Int64N(int64(30 * time.Second)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-kick:
		}
		err := u.Check(ctx)
		u.mu.Lock()
		u.checked = time.Now().UTC()
		u.lastErr = ""
		if err != nil {
			u.lastErr = err.Error()
			u.Log.Printf("update check: %v", err)
		}
		u.mu.Unlock()
		u.ReportStatus(ctx)
		jitter := time.Duration(rand.Int64N(int64(u.Interval)/5+1)) - u.Interval/10
		wait = u.Interval + jitter
	}
}

// Trigger requests an immediate check.
func (u *Updater) Trigger() {
	select {
	case u.kickCh() <- struct{}{}:
	default:
	}
}

func (u *Updater) kickCh() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.kick == nil {
		u.kick = make(chan struct{}, 1)
	}
	return u.kick
}

// Check fetches the channel manifest and installs it if it is new.
func (u *Updater) Check(ctx context.Context) error {
	b, err := u.Store.GetBytes(ctx, u.Store.Key(store.Channel(u.Channel)...))
	if err != nil {
		if store.IsNotFound(err) {
			return nil // nothing published yet
		}
		return fmt.Errorf("fetch channel %s: %w", u.Channel, err)
	}
	m, err := manifest.Open(b, u.Verifier)
	if err != nil {
		return err
	}
	st := u.Sup.State()
	if m.Serial <= st.SeenSerial {
		return nil
	}
	if m.Arch != "" && m.Arch != runtime.GOARCH {
		return fmt.Errorf("channel %s has %s %s for %s; this device is %s", u.Channel, m.Name, m.Version, m.Arch, runtime.GOARCH)
	}
	if cur := st.Current; cur != nil && cur.SHA256 == m.SHA256 {
		// Already running it (e.g. promoted from another channel); just record the serial.
		u.Sup.MarkSeen(m.Serial)
		return nil
	}
	u.Log.Printf("channel %s: new release %s (serial %d), downloading", u.Channel, m.Version, m.Serial)
	path, err := u.download(ctx, m)
	if err != nil {
		return err
	}
	return u.Sup.Install(ctx, path, Release{
		Version: m.Version,
		Serial:  m.Serial,
		SHA256:  m.SHA256,
		Source:  "channel:" + u.Channel,
	}, false)
}

func (u *Updater) download(ctx context.Context, m *manifest.Manifest) (string, error) {
	rc, err := u.Store.Get(ctx, m.Object)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", m.Object, err)
	}
	defer rc.Close()
	plain, err := age.Decrypt(rc, u.Identity)
	if err != nil {
		return "", fmt.Errorf("decrypt %s: %w", m.Object, err)
	}
	path, sum, _, err := receive(plain, TmpDir(u.Sup.o.DataDir), m.Size)
	if err != nil {
		return "", err
	}
	if sum != m.SHA256 {
		os.Remove(path)
		return "", fmt.Errorf("%s: sha256 %s does not match manifest %s", m.Object, sum, m.SHA256)
	}
	return path, nil
}

// receive streams r into a temp file in dir, hashing as it goes. If limit is
// positive, reading more than limit bytes is an error.
func receive(r io.Reader, dir string, limit int64) (path, sum string, n int64, err error) {
	f, err := os.CreateTemp(dir, "incoming-*.sif")
	if err != nil {
		return "", "", 0, err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	if limit > 0 {
		r = io.LimitReader(r, limit+1)
	}
	n, err = io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		return "", "", n, err
	}
	if limit > 0 && n > limit {
		return "", "", n, fmt.Errorf("image larger than the expected %d bytes", limit)
	}
	if err = f.Sync(); err != nil {
		return "", "", n, err
	}
	if err = f.Close(); err != nil {
		return "", "", n, err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), n, nil
}

// DeviceStatus is what a device publishes to status/<device>.json.
type DeviceStatus struct {
	Device    string    `json:"device"`
	Arch      string    `json:"arch"`
	Channel   string    `json:"channel"`
	Current   *Release  `json:"current,omitempty"`
	Previous  *Release  `json:"previous,omitempty"`
	BadCount  int       `json:"bad_count"`
	LastCheck time.Time `json:"last_check,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	Reported  time.Time `json:"reported"`
}

func (u *Updater) Status() DeviceStatus {
	st := u.Sup.State()
	u.mu.Lock()
	defer u.mu.Unlock()
	return DeviceStatus{
		Device: u.Device, Arch: runtime.GOARCH, Channel: u.Channel,
		Current: st.Current, Previous: st.Previous, BadCount: len(st.Bad),
		LastCheck: u.checked, LastError: u.lastErr, Reported: time.Now().UTC(),
	}
}

func (u *Updater) ReportStatus(ctx context.Context) {
	if !u.Report {
		return
	}
	b, _ := json.MarshalIndent(u.Status(), "", "  ")
	key := u.Store.Key(store.Status(u.Device)...)
	if err := u.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), store.PutOpts{ContentType: "application/json"}); err != nil {
		u.Log.Printf("report status: %v", err)
	}
}
