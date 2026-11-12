package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
)

// Claimer lets the workstation claim a device: give it a name, an API token
// and a record of its identity signed with the project key. An unclaimed
// device proves it is the board in front of you with a pairing code it
// shows on its console; a device built for one board (`ramekin device init`)
// can instead be claimed with its existing API token.
type Claimer struct {
	Identity *identity.Identity
	// Verifier checks the identity record: the project key the device's
	// image was built with. On official firmware it is nil until claimed,
	// and the claim brings the project's settings, key included.
	Verifier   signature.Verifier
	HardwareID string
	// UUID is the device's permanent UUID (identity.DeviceUUID).
	UUID        string
	LegacyToken string // accepted instead of the pairing code; "" if none
	// Problem explains why an existing identity was rejected, if it was.
	Problem string
	// GokrazyPasswordPath is where a new gokrazy password is written
	// (gokrazy prefers /perm/gokr-pw.txt over the one in the image).
	GokrazyPasswordPath string
	// SaveSettings stores the project settings a claim brings.
	SaveSettings func(config.Settings) error
	// Claimed runs after a successful claim (ramekind restarts to apply it).
	Claimed func()
	Log     *log.Logger

	mu   sync.Mutex
	code string
}

// ClaimInfo is what an unauthenticated GET /v1/claim returns.
type ClaimInfo struct {
	Claimed      bool   `json:"claimed"`
	Name         string `json:"name,omitempty"`
	PublicKey    string `json:"public_key"`
	AgeRecipient string `json:"age_recipient"`
	HardwareID   string `json:"hardware_id"`
	// UUID is the device's permanent UUID, for stickers and inventories.
	UUID string `json:"uuid,omitempty"`
	// NeedsCode is false when the device can be claimed with its API token.
	NeedsCode bool `json:"needs_code"`
	// NeedsSettings is true on official firmware: the claim must bring the
	// project's settings, including its signing key.
	NeedsSettings bool   `json:"needs_settings"`
	Problem       string `json:"problem,omitempty"`
}

// ClaimRequest is the body of POST /v1/claim.
type ClaimRequest struct {
	Record          json.RawMessage `json:"record"` // manifest.Envelope of an identity.Record
	Token           string          `json:"token"`
	Code            string          `json:"code,omitempty"`
	GokrazyPassword string          `json:"gokrazy_password,omitempty"`
	// Settings are required by official firmware, ignored otherwise.
	Settings *config.Settings `json:"settings,omitempty"`
	// FleetKey is the project's shared age key, encrypted to the device's
	// own age key, so it can install releases published before its claim.
	FleetKey []byte `json:"fleet_key,omitempty"`
}

func (c *Claimer) claimed() bool { return c.Identity.Record != nil }

// Code returns the current pairing code, creating one if needed.
func (c *Claimer) Code() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.code == "" {
		n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
		c.code = fmt.Sprintf("%06d", n.Int64())
	}
	return c.code
}

// PairingCodeFile holds the pairing code while the device is unclaimed, for
// programs that show it on screen (such as gk-lockscreen).
const PairingCodeFile = "/run/ramekin/pairing-code"

// Announce shows the pairing code on the console, in the log and in
// PairingCodeFile until the device is claimed.
func (c *Claimer) Announce(ctx context.Context) {
	defer os.Remove(PairingCodeFile)
	for !c.claimed() {
		msg := fmt.Sprintf("ramekin: this device is unclaimed. Pairing code: %s (hardware %s)", c.Code(), c.HardwareID)
		c.Log.Print(msg)
		if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "\r\n%s\r\n", msg)
			f.Close()
		}
		if err := os.MkdirAll(filepath.Dir(PairingCodeFile), 0o755); err == nil {
			os.WriteFile(PairingCodeFile, []byte(c.Code()+"\n"), 0o644)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

func (c *Claimer) info(w http.ResponseWriter, r *http.Request) {
	i := ClaimInfo{
		Claimed:       c.claimed(),
		PublicKey:     c.Identity.PublicKey(),
		AgeRecipient:  c.Identity.Age.Recipient().String(),
		HardwareID:    c.HardwareID,
		UUID:          c.UUID,
		NeedsCode:     c.LegacyToken == "",
		NeedsSettings: c.Verifier == nil,
		Problem:       c.Problem,
	}
	if i.Claimed {
		i.Name = c.Identity.Record.Name
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(i)
}

func (c *Claimer) claim(w http.ResponseWriter, r *http.Request) {
	if c.claimed() {
		http.Error(w, "this device is already claimed as "+c.Identity.Record.Name, http.StatusConflict)
		return
	}
	var req ClaimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	byToken := c.LegacyToken != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(c.LegacyToken)) == 1
	byCode := req.Code != "" && subtle.ConstantTimeCompare([]byte(req.Code), []byte(c.Code())) == 1
	if !byToken && !byCode {
		time.Sleep(2 * time.Second) // slows guessing
		c.Log.Print("claim refused: wrong pairing code")
		http.Error(w, "wrong pairing code", http.StatusForbidden)
		return
	}
	verifier := c.Verifier
	if verifier == nil {
		// Official firmware: trust the project key the claim brings, as the
		// pairing code shows the claimer has the device in hand.
		if req.Settings == nil || req.Settings.SigningPubKey == "" {
			http.Error(w, "this device runs official firmware: the claim must include the project's settings", http.StatusBadRequest)
			return
		}
		dev := config.Device{}
		if err := dev.Apply(*req.Settings); err != nil {
			http.Error(w, "settings: "+err.Error(), http.StatusBadRequest)
			return
		}
		v, err := keys.LoadVerifier([]byte(req.Settings.SigningPubKey))
		if err != nil {
			http.Error(w, "signing key: "+err.Error(), http.StatusBadRequest)
			return
		}
		verifier = v
	}
	if err := c.Identity.Claim(req.Record, req.Token, verifier, c.HardwareID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.FleetKey) > 0 {
		if err := c.Identity.SaveFleetKey(req.FleetKey); err != nil {
			c.Log.Printf("claim: %v", err)
		}
	}
	if c.Verifier == nil && c.SaveSettings != nil {
		if err := c.SaveSettings(*req.Settings); err != nil {
			c.Log.Printf("claim: saving settings: %v", err)
		}
	}
	if req.GokrazyPassword != "" {
		if err := writeAtomic(c.GokrazyPasswordPath, []byte(req.GokrazyPassword+"\n")); err != nil {
			c.Log.Printf("claim: writing the gokrazy password: %v", err)
		}
	}
	c.Log.Printf("claimed as %s", c.Identity.Record.Name)
	// Answer, signed with the device key, before restarting to apply the identity.
	b, _ := json.Marshal(map[string]string{"name": c.Identity.Record.Name, "hardware_id": c.HardwareID})
	b = append(b, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(identity.SignatureHeader, c.Identity.Sign(b))
	w.Write(b)
	if c.Claimed != nil {
		go func() {
			time.Sleep(time.Second)
			c.Claimed()
		}()
	}
}

// ReleaseRequest, signed with the project key, asks a device to forget its
// project (DELETE /v1/claim).
type ReleaseRequest struct {
	Release string    `json:"release"` // the device's name
	At      time.Time `json:"at"`
}

// Releaser factory-resets a claimed device: it forgets its identity,
// settings, apps and app data, and restarts unclaimed. Firmware stays.
type Releaser struct {
	Name     string
	Verifier signature.Verifier // the project key
	DataDir  string
	Stop     func() // stops the app before its files are removed
	Restart  func()
	Log      *log.Logger
}

func (rl *Releaser) release(w http.ResponseWriter, r *http.Request) {
	if rl == nil || rl.Verifier == nil {
		http.Error(w, "this device isn't claimed", http.StatusConflict)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req ReleaseRequest
	if err := manifest.OpenInto(b, rl.Verifier, &req); err != nil {
		http.Error(w, "release request "+err.Error(), http.StatusForbidden)
		return
	}
	if req.Release != rl.Name || time.Since(req.At).Abs() > 10*time.Minute {
		http.Error(w, "the release request is for another device, or too old", http.StatusForbidden)
		return
	}
	rl.Log.Printf("releasing %s: forgetting its identity, settings, apps and data", rl.Name)
	rl.Stop()
	ents, _ := os.ReadDir(rl.DataDir)
	for _, e := range ents {
		if e.Name() != "firmware.json" {
			os.RemoveAll(filepath.Join(rl.DataDir, e.Name()))
		}
	}
	os.Remove("/perm/gokr-pw.txt")
	if d, err := os.Open(rl.DataDir); err == nil {
		d.Sync()
		d.Close()
	}
	w.WriteHeader(http.StatusNoContent)
	go func() {
		time.Sleep(time.Second)
		rl.Restart()
	}()
}
