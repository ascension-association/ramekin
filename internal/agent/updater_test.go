package agent

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/manifest"
)

// channelHost serves channel manifests and encrypted images like a static
// host or CDN would.
type channelHost struct {
	mu      sync.Mutex
	files   map[string][]byte
	status  []DeviceStatus
	srv     *httptest.Server
	signer  signature.SignerVerifier
	id      *age.X25519Identity
	serials int64
}

func (h *channelHost) signerPublic() crypto.PublicKey {
	pk, _ := h.signer.PublicKey()
	return pk
}

func newChannelHost(t *testing.T) *channelHost {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sv, _ := signature.LoadECDSASignerVerifier(k, crypto.SHA256)
	id, _ := age.GenerateX25519Identity()
	h := &channelHost{files: map[string][]byte{}, signer: sv, id: id}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/status" {
			var s DeviceStatus
			json.NewDecoder(r.Body).Decode(&s)
			h.status = append(h.status, s)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download/") {
			// Release assets redirect to a signed URL elsewhere.
			http.Redirect(w, r, "/blob/"+strings.TrimPrefix(r.URL.Path, "/download/"), http.StatusFound)
			return
		}
		b, ok := h.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// publish puts an image on a channel, as `ramekin publish` would.
func (h *channelHost) publish(t *testing.T, channel, version, content string, corrupt bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var enc bytes.Buffer
	w, _ := age.Encrypt(&enc, h.id.Recipient())
	io.WriteString(w, content)
	w.Close()
	sum := sha256.Sum256([]byte(content))
	if corrupt {
		sum[0] ^= 1
	}
	h.files["/blob/"+version+".sif.age"] = enc.Bytes()
	h.serials++
	m := &manifest.Manifest{Name: "app", Version: version, Channel: channel, Serial: h.serials, Arch: runtime.GOARCH,
		Object: "../download/" + version + ".sif.age", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
	env, err := manifest.Sign(m, h.signer)
	if err != nil {
		t.Fatal(err)
	}
	h.files["/channels/"+channel+".json"] = env
}

func TestUpdaterHTTPS(t *testing.T) {
	ctx := context.Background()
	h := newChannelHost(t)
	rt := &fakeRuntime{}
	sup := newSup(t, t.TempDir(), rt)
	u := &Updater{Sup: sup, URL: h.srv.URL + "/channels/", Channel: "stable", Verifier: h.signer, Identity: h.id,
		Interval: time.Hour, Device: "dev1", StatusURL: h.srv.URL + "/status", Log: sup.o.Log}

	// Nothing published yet is not an error.
	if err := u.Check(ctx); err != nil {
		t.Fatalf("empty channel: %v", err)
	}
	h.publish(t, "stable", "1.0", "v1", false)
	if err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if v := sup.State().Current.Version; v != "1.0" || rt.last() != "v1" {
		t.Fatalf("running %s (%s)", v, rt.last())
	}
	// Polling again changes nothing.
	if err := u.Check(ctx); err != nil || len(rt.started) != 1 {
		t.Fatalf("re-poll: %v, %d starts", err, len(rt.started))
	}

	// A manifest that isn't signed by the project key is refused.
	h.mu.Lock()
	h.files["/channels/stable.json"] = []byte(`{"payload":"e30=","signature":"AAAA"}`)
	h.mu.Unlock()
	if err := u.Check(ctx); err == nil {
		t.Fatal("forged manifest accepted")
	}

	// An image that doesn't match its manifest's checksum is refused.
	h.publish(t, "stable", "2.0", "v2", true)
	if err := u.Check(ctx); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("corrupt image: %v", err)
	}
	if v := sup.State().Current.Version; v != "1.0" {
		t.Fatalf("corrupt image installed: %s", v)
	}

	h.publish(t, "stable", "3.0", "v3", false)
	if err := u.Check(ctx); err != nil || sup.State().Current.Version != "3.0" {
		t.Fatalf("3.0: %v", err)
	}

	u.ReportStatus(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.status) != 1 || h.status[0].Device != "dev1" || h.status[0].Current.Version != "3.0" {
		t.Fatalf("status reports: %+v", h.status)
	}
}
