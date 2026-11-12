package agent

import (
	"archive/tar"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/manifest"
)

// API is the device's authenticated HTTP API, used by `ramekin push`,
// `ramekin logs` and `ramekin status`.
type API struct {
	Sup     *Supervisor
	Upd     *Updater // nil when channel updates are disabled
	Logs    *LogBuffer
	Metrics *Sampler // optional
	Token   string
	Device  string
	// UUID is the device's permanent UUID, reported in its status.
	UUID string

	// Sign, once the device is claimed, signs response bodies with its
	// identity key, so the workstation knows which device answered.
	Sign func([]byte) string
	// Claim serves /v1/claim (unauthenticated); nil disables claiming.
	Claim *Claimer
	// Firmware accepts firmware pushes (checked against Verifier); nil
	// disables them.
	Firmware *Firmware
	Verifier signature.Verifier
	// Settings serves the project settings; nil disables /v1/settings.
	Settings *SettingsStore
	// Release serves DELETE /v1/claim; nil disables it.
	Release *Releaser
	// Install serves POST /v1/install; nil disables it.
	Install *Installer
	// Gokrazy serves gokrazy's web interface under /v1/gokrazy/; nil
	// disables it.
	Gokrazy http.Handler
}

// SettingsStore holds a device's project settings. On official firmware they
// live on the device and can be changed over the API; otherwise they come
// from the image and change with it.
type SettingsStore struct {
	Current    config.Settings
	FromDevice bool
	// Verifier checks new settings: the project's key, not the firmware's.
	Verifier signature.Verifier
	Save     func(config.Settings) error
	Restart  func()
}

// SettingsInfo is the answer to GET /v1/settings.
type SettingsInfo struct {
	Settings config.Settings `json:"settings"`
	// Source is "device" (changeable with PUT /v1/settings) or "image".
	Source string `json:"source"`
}

type PushResult struct {
	OK         bool   `json:"ok"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	Error      string `json:"error,omitempty"`
	State      State  `json:"state"`
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/firmware", a.pushFirmware)
	mux.HandleFunc("GET /v1/settings", a.getSettings)
	mux.HandleFunc("DELETE /v1/claim", func(w http.ResponseWriter, r *http.Request) { a.Release.release(w, r) })
	mux.HandleFunc("POST /v1/install", func(w http.ResponseWriter, r *http.Request) { a.Install.install(w, r) })
	mux.HandleFunc("PUT /v1/settings", a.putSettings)
	mux.HandleFunc("GET /v1/status", a.status)
	mux.HandleFunc("PUT /v1/image", a.push)
	mux.HandleFunc("GET /v1/logs", a.logs)
	mux.HandleFunc("GET /v1/processes", a.processes)
	mux.HandleFunc("POST /v1/rollback", a.rollback)
	mux.HandleFunc("POST /v1/switch", a.rollback)
	mux.HandleFunc("POST /v1/restart", func(w http.ResponseWriter, r *http.Request) {
		a.Sup.Restart()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/check", func(w http.ResponseWriter, r *http.Request) {
		if a.Upd == nil {
			http.Error(w, "channel updates are disabled on this device", http.StatusConflict)
			return
		}
		a.Upd.Trigger()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("DELETE /v1/bad", func(w http.ResponseWriter, r *http.Request) {
		a.Sup.ClearBad()
		w.WriteHeader(http.StatusNoContent)
	})
	if a.Gokrazy != nil {
		mux.Handle(GokrazyPrefix+"/", a.Gokrazy)
	}
	authed := a.auth(mux)
	if a.Claim == nil {
		return authed
	}
	top := http.NewServeMux()
	top.HandleFunc("GET /v1/claim", a.Claim.info)
	top.HandleFunc("POST /v1/claim", a.Claim.claim)
	top.Handle("/", authed)
	return top
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if a.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// status answers with the device's status. The workstation passes a random
// nonce, which the (signed) answer echoes, so an old answer can't be replayed.
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	var s DeviceStatus
	if a.Upd != nil {
		s = a.Upd.Status()
	} else {
		s = NewStatus(a.Device, a.Sup, a.Metrics)
		if a.Firmware != nil {
			fw := a.Firmware.Status()
			s.Firmware = &fw
		}
	}
	s.Nonce = r.URL.Query().Get("nonce")
	s.UUID, s.ShortID = a.UUID, identity.ShortID(a.UUID)
	if a.Gokrazy != nil {
		s.Features = append(s.Features, FeatureGokrazy)
	}
	a.writeJSON(w, http.StatusOK, s)
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	if a.Settings == nil {
		http.Error(w, "not supported", http.StatusNotImplemented)
		return
	}
	src := "image"
	if a.Settings.FromDevice {
		src = "device"
	}
	a.writeJSON(w, http.StatusOK, SettingsInfo{Settings: a.Settings.Current, Source: src})
}

// putSettings replaces the project settings of a device on official
// firmware. Besides the API token, the new settings must be signed with the
// project's current key (they may name a new one), then ramekind restarts.
func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	if a.Settings == nil || !a.Settings.FromDevice {
		http.Error(w, "this device's settings come from its image; change them with `ramekin device set` and a firmware update", http.StatusConflict)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var s config.Settings
	if a.Settings.Verifier == nil {
		http.Error(w, "this device isn't claimed", http.StatusConflict)
		return
	}
	if err := manifest.OpenInto(b, a.Settings.Verifier, &s); err != nil {
		http.Error(w, "settings "+err.Error(), http.StatusBadRequest)
		return
	}
	dev := config.Device{}
	if err := dev.Apply(s); err != nil {
		http.Error(w, "settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.Settings.Save(s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, http.StatusOK, SettingsInfo{Settings: s, Source: "device"})
	go func() {
		time.Sleep(time.Second)
		a.Settings.Restart()
	}()
}

// pushFirmware installs a firmware image sent as a tar of manifest.json (a
// signed firmware manifest), boot.img and root.img, then reboots into it for
// a test boot.
func (a *API) pushFirmware(w http.ResponseWriter, r *http.Request) {
	if a.Firmware == nil {
		http.Error(w, "this device's ramekind can't install firmware", http.StatusNotImplemented)
		return
	}
	dir, err := os.MkdirTemp(TmpDir(a.Sup.o.DataDir), "firmware-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	files := map[string]string{}
	var m *manifest.Manifest
	tr := tar.NewReader(r.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "read upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch h.Name {
		case "manifest.json":
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err == nil {
				m, err = manifest.Open(b, a.Verifier)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		case "boot.img", "root.img":
			p := filepath.Join(dir, h.Name)
			f, err := os.Create(p)
			if err == nil {
				_, err = io.Copy(f, tr)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				http.Error(w, "receive "+h.Name+": "+err.Error(), http.StatusInternalServerError)
				return
			}
			files[strings.TrimSuffix(h.Name, ".img")] = p
		}
	}
	if m == nil {
		http.Error(w, "no signed manifest", http.StatusBadRequest)
		return
	}
	if err := VerifyParts(m, files); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Answer before the reboot: once it starts, the device may go down before
	// a later answer reaches the workstation.
	answered := false
	ready := func() {
		answered = true
		a.writeJSON(w, http.StatusOK, map[string]string{"version": m.Version, "status": "rebooting for a test boot"})
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	if err := a.Firmware.Install(r.Context(), m.Version, files["boot"], files["root"], ready); err != nil {
		if answered {
			a.Firmware.Log.Printf("firmware: reboot into %s: %v", m.Version, err)
			return
		}
		http.Error(w, fmt.Sprintf("install firmware %s: %v", m.Version, err), http.StatusInternalServerError)
	}
}

// rollback runs an earlier release on trial: the previous one, or for
// /v1/switch the one given by ?sha= (or the newest kept with ?version=).
// It answers like push.
func (a *API) rollback(w http.ResponseWriter, r *http.Request) {
	var err error
	if r.URL.Path == "/v1/switch" {
		err = a.Sup.SwitchTo(r.Context(), a.findKept(r.URL.Query().Get("sha"), r.URL.Query().Get("version")))
	} else {
		err = a.Sup.Rollback(r.Context())
	}
	res := PushResult{OK: err == nil, RolledBack: errors.Is(err, ErrRolledBack), State: a.Sup.State()}
	code := http.StatusOK
	switch {
	case errors.Is(err, ErrNoPrevious), errors.Is(err, ErrNotKept):
		code = http.StatusConflict
	case err != nil:
		code = http.StatusUnprocessableEntity
	}
	if err != nil {
		res.Error = err.Error()
	}
	a.writeJSON(w, code, res)
}

func (a *API) push(w http.ResponseWriter, r *http.Request) {
	version := r.URL.Query().Get("version")
	if version == "" {
		version = "dev"
	}
	path, sum, _, err := receive(r.Body, TmpDir(a.Sup.o.DataDir), 0)
	if err != nil {
		http.Error(w, "receive image: "+err.Error(), http.StatusBadRequest)
		return
	}
	err = a.Sup.Install(r.Context(), path, Release{Version: version, SHA256: sum, Source: "push"}, r.URL.Query().Get("force") == "1")
	res := PushResult{OK: err == nil, RolledBack: errors.Is(err, ErrRolledBack), State: a.Sup.State()}
	code := http.StatusOK
	if err != nil {
		res.Error = err.Error()
		code = http.StatusUnprocessableEntity
	}
	a.writeJSON(w, code, res)
}

// processes lists what runs on the device (kernel threads with ?kernel=1).
// Like status, the signed answer echoes the caller's nonce.
func (a *API) processes(w http.ResponseWriter, r *http.Request) {
	ps, err := Processes("/proc", r.URL.Query().Get("kernel") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	l := ProcessList{Device: a.Device, Nonce: r.URL.Query().Get("nonce"), Processes: ps}
	st := a.Sup.State()
	if rel := st.Trial; rel != nil {
		l.App = rel.Version
	} else if rel := st.Current; rel != nil {
		l.App = rel.Version
	}
	a.writeJSON(w, http.StatusOK, l)
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	backlog, ch, cancel := a.Logs.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, l := range backlog {
		w.Write(l)
	}
	if r.URL.Query().Get("follow") != "1" {
		return
	}
	fl, _ := w.(http.Flusher)
	for {
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case l := <-ch:
			if _, err := w.Write(l); err != nil {
				return
			}
		}
	}
}

// findKept returns the SHA-256 of the kept release matching sha or version.
func (a *API) findKept(sha, version string) string {
	for _, r := range a.Sup.State().History {
		if r.SHA256 == sha || (sha == "" && version != "" && r.Version == version) {
			return r.SHA256
		}
	}
	return ""
}

// writeJSON answers with v, signed with the device's key once it is claimed.
func (a *API) writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b = append(b, '\n')
	w.Header().Set("Content-Type", "application/json")
	// With a length, a client has the whole answer once the body arrives,
	// even if the device then goes down (a firmware push reboots it).
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if a.Sign != nil {
		w.Header().Set(identity.SignatureHeader, a.Sign(b))
	}
	w.WriteHeader(code)
	w.Write(b)
}
