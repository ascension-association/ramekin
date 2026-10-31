package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/identity"
)

// Claimer lets the workstation claim a device: give it a name, an API token
// and a record of its identity signed with the project key. An unclaimed
// device proves it is the board in front of you with a pairing code it
// shows on its console; a device built for one board (`ramekin device init`)
// can instead be claimed with its existing API token.
type Claimer struct {
	Identity    *identity.Identity
	Verifier    signature.Verifier
	HardwareID  string
	LegacyToken string // accepted instead of the pairing code; "" if none
	// Problem explains why an existing identity was rejected, if it was.
	Problem string
	// GokrazyPasswordPath is where a new gokrazy password is written
	// (gokrazy prefers /perm/gokr-pw.txt over the one in the image).
	GokrazyPasswordPath string
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
	// NeedsCode is false when the device can be claimed with its API token.
	NeedsCode bool   `json:"needs_code"`
	Problem   string `json:"problem,omitempty"`
}

// ClaimRequest is the body of POST /v1/claim.
type ClaimRequest struct {
	Record          json.RawMessage `json:"record"` // manifest.Envelope of an identity.Record
	Token           string          `json:"token"`
	Code            string          `json:"code,omitempty"`
	GokrazyPassword string          `json:"gokrazy_password,omitempty"`
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

// Announce shows the pairing code on the console and in the log until the
// device is claimed.
func (c *Claimer) Announce(ctx context.Context) {
	for !c.claimed() {
		msg := fmt.Sprintf("ramekin: this device is unclaimed. Pairing code: %s (hardware %s)", c.Code(), c.HardwareID)
		c.Log.Print(msg)
		if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
			fmt.Fprintf(f, "\r\n%s\r\n", msg)
			f.Close()
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
		Claimed:      c.claimed(),
		PublicKey:    c.Identity.PublicKey(),
		AgeRecipient: c.Identity.Age.Recipient().String(),
		HardwareID:   c.HardwareID,
		NeedsCode:    c.LegacyToken == "",
		Problem:      c.Problem,
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
	if err := c.Identity.Claim(req.Record, req.Token, c.Verifier, c.HardwareID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
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
