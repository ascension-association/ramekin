package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/manifest"
)

func TestClaim(t *testing.T) {
	h := newChannelHost(t) // for its project signing key
	dir := t.TempDir()
	id, err := identity.Ensure(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	claimed := make(chan struct{}, 1)
	c := &Claimer{Identity: id, Verifier: h.signer, HardwareID: "dt:A", GokrazyPasswordPath: filepath.Join(dir, "gokr-pw.txt"),
		Claimed: func() { claimed <- struct{}{} }, Log: log.New(io.Discard, "", 0)}
	api := &API{Sup: newSup(t, dir, &fakeRuntime{}), Claim: c}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	var info ClaimInfo
	resp, err := http.Get(srv.URL + "/v1/claim")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&info)
	if info.Claimed || !info.NeedsCode || info.PublicKey != id.PublicKey() || info.HardwareID != "dt:A" {
		t.Fatalf("info %+v", info)
	}
	// Everything else needs a token, which an unclaimed device doesn't have.
	if resp, _ := http.Get(srv.URL + "/v1/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without a token: %d", resp.StatusCode)
	}

	rec, _ := manifest.Seal(identity.Record{Name: "kitchen", PublicKey: info.PublicKey, AgeRecipient: info.AgeRecipient, HardwareID: info.HardwareID, ClaimedAt: time.Now()}, h.signer)
	post := func(code string) *http.Response {
		b, _ := json.Marshal(ClaimRequest{Record: rec, Token: "0123456789abcdef0123", Code: code, GokrazyPassword: "pw"})
		resp, err := http.Post(srv.URL+"/v1/claim", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("000000x"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	resp = post(c.Code())
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || identity.Verify(id.PublicKey(), body, resp.Header.Get(identity.SignatureHeader)) != nil {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	select {
	case <-claimed:
	case <-time.After(3 * time.Second):
		t.Fatal("ramekind not asked to restart")
	}
	if resp := post(c.Code()); resp.StatusCode != http.StatusConflict {
		t.Fatalf("claimed twice: %d", resp.StatusCode)
	}
}

func TestClaimOfficialFirmware(t *testing.T) {
	h := newChannelHost(t) // its key plays the project's
	dir := t.TempDir()
	id, _ := identity.Ensure(filepath.Join(dir, "identity"))
	var saved *config.Settings
	c := &Claimer{Identity: id, HardwareID: "dt:A", GokrazyPasswordPath: filepath.Join(dir, "pw"), Log: log.New(io.Discard, "", 0),
		SaveSettings: func(s config.Settings) error { saved = &s; return nil }}
	srv := httptest.NewServer((&API{Sup: newSup(t, dir, &fakeRuntime{}), Claim: c}).Handler())
	defer srv.Close()

	var info ClaimInfo
	resp, _ := http.Get(srv.URL + "/v1/claim")
	json.NewDecoder(resp.Body).Decode(&info)
	if !info.NeedsSettings {
		t.Fatal("official firmware didn't ask for settings")
	}
	pub, _ := cryptoutils.MarshalPublicKeyToPEM(h.signerPublic())
	rec, _ := manifest.Seal(identity.Record{Name: "kitchen", PublicKey: info.PublicKey, AgeRecipient: info.AgeRecipient, HardwareID: "dt:A"}, h.signer)
	// The project's fleet key, encrypted to the device's own age key.
	fleet, _ := age.GenerateX25519Identity()
	devRcpt, _ := age.ParseX25519Recipient(info.AgeRecipient)
	var encFleet bytes.Buffer
	w, _ := age.Encrypt(&encFleet, devRcpt)
	io.WriteString(w, fleet.String())
	w.Close()
	post := func(s *config.Settings) int {
		b, _ := json.Marshal(ClaimRequest{Record: rec, Token: "0123456789abcdef0123", Code: c.Code(), Settings: s, FleetKey: encFleet.Bytes()})
		resp, err := http.Post(srv.URL+"/v1/claim", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if code := post(nil); code != http.StatusBadRequest {
		t.Fatalf("claim without settings: %d", code)
	}
	s := &config.Settings{SigningPubKey: string(pub), Update: config.Update{Enabled: true, URL: "https://example/app"}}
	if code := post(s); code != http.StatusOK || saved == nil || saved.SigningPubKey != string(pub) {
		t.Fatalf("claim: %d, saved %+v", code, saved)
	}
	// The fleet key is kept, and survives a restart.
	again, _ := identity.Ensure(filepath.Join(dir, "identity"))
	if again.Fleet == nil || again.Fleet.String() != fleet.String() {
		t.Fatal("fleet key not kept")
	}
}

func TestPutSettings(t *testing.T) {
	h := newChannelHost(t)
	dir := t.TempDir()
	var saved *config.Settings
	store := &SettingsStore{FromDevice: true, Verifier: h.signer, Save: func(s config.Settings) error { saved = &s; return nil }, Restart: func() {}}
	// The API's own Verifier is the firmware key; settings use the project's.
	fwKey := newChannelHost(t)
	srv := httptest.NewServer((&API{Sup: newSup(t, dir, &fakeRuntime{}), Token: "tok", Verifier: fwKey.signer, Settings: store}).Handler())
	defer srv.Close()
	put := func(body []byte) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/settings", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	pub, _ := cryptoutils.MarshalPublicKeyToPEM(h.signerPublic())
	s := config.Settings{SigningPubKey: string(pub), Health: config.Health{HTTP: "http://127.0.0.1/"}}
	other := newChannelHost(t)
	forged, _ := manifest.Seal(s, other.signer)
	if code := put(forged); code != http.StatusBadRequest || saved != nil {
		t.Fatalf("settings signed by another key: %d", code)
	}
	good, _ := manifest.Seal(s, h.signer)
	if code := put(good); code != http.StatusOK || saved == nil || saved.Health.HTTP != "http://127.0.0.1/" {
		t.Fatalf("signed settings: %d %+v", code, saved)
	}
	store.FromDevice = false
	if code := put(good); code != http.StatusConflict {
		t.Fatalf("changed settings that come from the image: %d", code)
	}
}

func TestRelease(t *testing.T) {
	h := newChannelHost(t)
	dir := t.TempDir()
	for _, f := range []string{"state.json", "settings.json", "firmware.json", "identity/record.json", "images/x.sif", "data/app.db"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o700)
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	var restarted atomic.Bool
	rl := &Releaser{Name: "kitchen", Verifier: h.signer, DataDir: dir, Stop: func() {}, Restart: func() { restarted.Store(true) }, Log: log.New(io.Discard, "", 0)}
	srv := httptest.NewServer((&API{Sup: newSup(t, t.TempDir(), &fakeRuntime{}), Token: "tok", Release: rl}).Handler())
	defer srv.Close()
	del := func(req ReleaseRequest, s *channelHost) int {
		body, _ := manifest.Seal(req, s.signer)
		r, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/claim", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	now := time.Now()
	for _, bad := range []struct {
		req ReleaseRequest
		s   *channelHost
	}{
		{ReleaseRequest{Release: "garage", At: now}, h},                  // another device
		{ReleaseRequest{Release: "kitchen", At: now.Add(-time.Hour)}, h}, // replayed
		{ReleaseRequest{Release: "kitchen", At: now}, newChannelHost(t)}, // another project
	} {
		if code := del(bad.req, bad.s); code != http.StatusForbidden {
			t.Fatalf("%+v: %d", bad.req, code)
		}
	}
	if code := del(ReleaseRequest{Release: "kitchen", At: now}, h); code != http.StatusNoContent {
		t.Fatalf("release: %d", code)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "firmware.json" {
		t.Fatalf("left %v", ents)
	}
	waitFor(t, restarted.Load)
}
