package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
)

// Updater polls a channel over HTTPS. Everything it downloads is verified
// (signed manifest, image checksum and signature), so the host serving it
// needs no special trust and the device needs no credentials.
type Updater struct {
	Sup *Supervisor
	// URL is the directory of channel manifests: <URL>/<Channel>.json.
	URL       string
	Channel   string
	Verifier  signature.Verifier
	Identity  age.Identity
	Interval  time.Duration
	Device    string
	StatusURL string              // optional: POSTed a DeviceStatus after every check
	Metrics   *Sampler            // optional: resource use included in status reports
	Firmware  *Firmware           // optional: firmware status, checked after each app check
	Sign      func([]byte) string // optional: signs status reports (see API.Sign)
	Log       *log.Logger

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
		if u.Firmware != nil {
			if ferr := u.Firmware.Check(ctx); ferr != nil {
				u.Log.Printf("firmware check: %v", ferr)
			}
		}
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
	chURL := u.channelURL()
	b, err := release.FetchBytes(ctx, chURL, 1<<20)
	if err != nil {
		if errors.Is(err, release.ErrNotFound) {
			return nil // nothing published yet
		}
		return fmt.Errorf("fetch channel %s: %w", u.Channel, err)
	}
	m, err := manifest.Open(b, u.Verifier)
	if err != nil {
		return err
	}
	st := u.Sup.State()
	if m.Serial <= st.SeenSerials[u.Channel] {
		return nil
	}
	if m.Arch != "" && m.Arch != runtime.GOARCH {
		return fmt.Errorf("channel %s has %s %s for %s; this device is %s", u.Channel, m.Name, m.Version, m.Arch, runtime.GOARCH)
	}
	if cur := st.Current; cur != nil && cur.SHA256 == m.SHA256 {
		// Already running it (e.g. promoted from another channel); just record the serial.
		u.Sup.MarkSeen(u.Channel, m.Serial)
		return nil
	}
	u.Log.Printf("channel %s: new release %s (serial %d), downloading", u.Channel, m.Version, m.Serial)
	path, err := u.download(ctx, chURL, m)
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

func (u *Updater) channelURL() string {
	return strings.TrimSuffix(u.URL, "/") + "/" + u.Channel + ".json"
}

func (u *Updater) download(ctx context.Context, chURL string, m *manifest.Manifest) (string, error) {
	obj, err := release.Resolve(chURL, m.Object)
	if err != nil {
		return "", err
	}
	rc, err := release.Fetch(ctx, obj)
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

// DeviceStatus is what a device reports (API /v1/status and update.status_url).
type DeviceStatus struct {
	Device    string          `json:"device"`
	Arch      string          `json:"arch"`
	Channel   string          `json:"channel"`
	Current   *Release        `json:"current,omitempty"`
	Previous  *Release        `json:"previous,omitempty"`
	History   []*Release      `json:"history,omitempty"` // earlier releases it can switch to, newest first
	Trial     *Release        `json:"trial,omitempty"`   // being health-checked right now
	App       AppStatus       `json:"app"`
	BadCount  int             `json:"bad_count"`
	LastCheck time.Time       `json:"last_check,omitzero"`
	LastError string          `json:"last_error,omitempty"`
	Metrics   *Metrics        `json:"metrics,omitempty"`
	Firmware  *FirmwareStatus `json:"firmware,omitempty"`
	Disks     []Disk          `json:"disks,omitempty"`
	Reported  time.Time       `json:"reported"`
	// Features lists optional API endpoints this ramekind serves, so the
	// workstation knows what an older device can't do.
	Features []string `json:"features,omitempty"`
	// Nonce echoes the workstation's nonce, binding a signed answer to its request.
	Nonce string `json:"nonce,omitempty"`
}

// NewStatus reports a supervisor's releases, app and resource use.
func NewStatus(device string, sup *Supervisor, m *Sampler) DeviceStatus {
	st := sup.State()
	return DeviceStatus{
		Device: device, Arch: runtime.GOARCH,
		Current: st.Current, Previous: st.Previous, History: st.History, Trial: st.Trial, App: sup.App(),
		BadCount: len(st.Bad), Metrics: m.Snapshot(), Reported: time.Now().UTC(),
		Disks: Disks("/sys", bootDiskName()),
	}
}

func (u *Updater) Status() DeviceStatus {
	s := NewStatus(u.Device, u.Sup, u.Metrics)
	u.mu.Lock()
	defer u.mu.Unlock()
	s.Channel, s.LastCheck, s.LastError = u.Channel, u.checked, u.lastErr
	if u.Firmware != nil {
		fw := u.Firmware.Status()
		s.Firmware = &fw
	}
	return s
}

// ReportStatus POSTs the device's status to StatusURL, if configured.
func (u *Updater) ReportStatus(ctx context.Context) {
	if u.StatusURL == "" {
		return
	}
	b, _ := json.Marshal(u.Status())
	sig := ""
	if u.Sign != nil {
		sig = u.Sign(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.StatusURL, bytes.NewReader(b))
	if err != nil {
		u.Log.Printf("report status: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set(identity.SignatureHeader, sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		u.Log.Printf("report status: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode > 299 {
		u.Log.Printf("report status: %s returned %s", u.StatusURL, resp.Status)
	}
}
