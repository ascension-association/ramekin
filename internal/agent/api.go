package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// API is the device's authenticated HTTP API, used by `ramekin push`,
// `ramekin logs` and `ramekin status`.
type API struct {
	Sup    *Supervisor
	Upd    *Updater // nil when channel updates are disabled
	Logs   *LogBuffer
	Token  string
	Device string
}

type PushResult struct {
	OK         bool   `json:"ok"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	Error      string `json:"error,omitempty"`
	State      State  `json:"state"`
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", a.status)
	mux.HandleFunc("PUT /v1/image", a.push)
	mux.HandleFunc("GET /v1/logs", a.logs)
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
	return a.auth(mux)
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

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	var v any
	if a.Upd != nil {
		v = a.Upd.Status()
	} else {
		st := a.Sup.State()
		v = DeviceStatus{Device: a.Device, Arch: runtime.GOARCH, Current: st.Current, Previous: st.Previous, BadCount: len(st.Bad), Reported: time.Now().UTC()}
	}
	writeJSON(w, http.StatusOK, v)
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
	writeJSON(w, code, res)
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
