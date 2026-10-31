package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

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
