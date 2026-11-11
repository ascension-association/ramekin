package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGokrazyRoute opens gokrazy through ramekind when it can, else directly
// with a known password, and never puts a password in the card's note.
func TestGokrazyRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &config.Project{Name: "demo", Dir: t.TempDir()}
	writeFile(t, instanceConfigPath("dev"), `{"Update": {"HTTPPassword": "instpw", "HTTPPort": "9000"}}`)
	writeFile(t, instanceConfigPath("reflashed"), `{"Update": {"HTTPPassword": "stalepw"}}`)
	old := gokrazyAnswers
	t.Cleanup(func() { gokrazyAnswers = old })
	gokrazyAnswers = func(base string) bool { return !strings.Contains(base, "192.168.1.40") }
	gk := []string{agent.FeatureGokrazy}
	for _, c := range []struct {
		name      string
		d         deviceRecord
		features  []string
		via, base string
	}{
		{"new ramekind, official firmware", deviceRecord{name: "official", URL: "http://192.168.1.30:8095"}, gk, "ramekind", ""},
		{"new ramekind wins over a password", deviceRecord{name: "fleet", URL: "http://192.168.1.20:8095", GokrazyPassword: "recpw"}, gk, "ramekind", ""},
		{"old ramekind, claimed", deviceRecord{name: "fleet", URL: "http://192.168.1.20:8095", GokrazyPassword: "recpw"}, nil, "direct", "http://192.168.1.20:8181"},
		{"old ramekind, gok instance", deviceRecord{name: "dev", URL: "http://[fd00::5]:8095"}, nil, "direct", "http://[fd00::5]:9000"},
		{"old ramekind, official firmware", deviceRecord{name: "official", URL: "http://192.168.1.30:8095"}, nil, "", ""},
		// A gok password from before the device was reflashed: nothing answers on 8181.
		{"old ramekind, stale password", deviceRecord{name: "reflashed", URL: "http://192.168.1.40:8095"}, nil, "", ""},
	} {
		acc, err := gokrazyRoute(p, c.d, c.features)
		if acc.via != c.via || acc.base != c.base || (c.via == "") != (err != nil) {
			t.Errorf("%s: got %+v, %v; want via %q base %q", c.name, acc, err, c.via, c.base)
		}
		ok, note := gokrazyInfo(p, c.d, c.features)
		if ok != (c.via != "") || note == "" {
			t.Errorf("%s: info %v %q", c.name, ok, note)
		}
		for _, secret := range []string{"recpw", "instpw", "stalepw"} {
			if strings.Contains(note, secret) {
				t.Errorf("%s: note reveals the password: %q", c.name, note)
			}
		}
	}
}

// fakeGokrazy plays gokrazy behind a prefix ("/v1/gokrazy" for ramekind),
// accepting only wantAuth. It records what it saw.
type gkSeen struct{ path, auth, cookie string }

func fakeGokrazy(t *testing.T, prefix, wantAuth string, seen chan<- gkSeen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		seen <- gkSeen{path, r.Header.Get("Authorization"), r.Header.Get("Cookie")}
		switch path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "gokrazy_xsrf", Value: "123"})
			io.WriteString(w, "overview")
		case "/restart":
			http.Redirect(w, r, "/status?path=x", http.StatusSeeOther)
		case "/absolute":
			w.Header().Set("Location", "http://gokrazy/status?path=y")
			w.WriteHeader(http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGokrazySession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seen := make(chan gkSeen, 1)
	viaRamekind := fakeGokrazy(t, agent.GokrazyPrefix, "Bearer devtoken", seen)
	direct := fakeGokrazy(t, "", "Basic Z29rcmF6eTpneHB3", seen) // gokrazy:gxpw

	p := &config.Project{Name: "demo", Dir: t.TempDir()}
	writeFile(t, deviceRecordPath(p, "new"), `{"url": "`+viaRamekind.URL+`", "token": "devtoken"}`)
	directURL, _ := url.Parse(direct.URL)
	writeFile(t, deviceRecordPath(p, "old"), `{"url": "http://127.0.0.1:1", "token": "x", "gokrazy_password": "gxpw"}`)
	writeFile(t, instanceConfigPath("old"), `{"Update": {"HTTPPort": "`+directURL.Port()+`"}}`)
	writeFile(t, deviceRecordPath(p, "none"), `{"url": "http://127.0.0.1:1", "token": "x"}`)

	c := &console{p: p, key: "k", listenHost: "127.0.0.1", searched: map[string]time.Time{}, devices: map[string]*fleetDevice{
		"new": {Name: "new", Status: &agent.DeviceStatus{Features: []string{agent.FeatureGokrazy}}},
		"old": {Name: "old", Status: &agent.DeviceStatus{}},
	}}
	defer c.closeGokrazy()
	h := c.handler("127.0.0.1:8099")
	open := func(name, key string) (int, map[string]any) {
		r := httptest.NewRequest("POST", "/api/devices/"+name+"/gokrazy", nil)
		r.Host = "127.0.0.1:8099"
		if key != "" {
			r.Header.Set("X-Ramekin-Console", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var res map[string]any
		json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res
	}
	if code, _ := open("new", ""); code != http.StatusForbidden {
		t.Fatalf("GK without console key: %d", code)
	}
	if code, _ := open("nope", "k"); code != http.StatusNotFound {
		t.Fatalf("GK for unknown device: %d", code)
	}
	if _, res := open("none", "k"); res["ok"] != false || !strings.Contains(res["error"].(string), "too old") {
		t.Fatalf("GK for a device with no way in: %v", res)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(method, rawURL string, cookies ...*http.Cookie) *http.Response {
		req, _ := http.NewRequest(method, rawURL, nil)
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	for _, tc := range []struct {
		device, wantPath, wantAuth string
	}{
		{"new", "/", "Bearer devtoken"},        // through ramekind, with the device token
		{"old", "/", "Basic Z29rcmF6eTpneHB3"}, // straight to gokrazy, with its password
	} {
		_, res := open(tc.device, "k")
		login, _ := res["url"].(string)
		if res["ok"] != true || !strings.HasPrefix(login, "http://127.0.0.1:") || strings.Contains(login, "devtoken") || strings.Contains(login, "gxpw") {
			t.Fatalf("%s: open: %v", tc.device, res)
		}
		u, _ := url.Parse(login)
		origin := "http://" + u.Host
		port := u.Port()

		if resp := get("GET", origin+"/"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: no session cookie: %d", tc.device, resp.StatusCode)
		}
		if resp := get("GET", origin+gkLoginPath+"?k=wrong"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: wrong login key: %d", tc.device, resp.StatusCode)
		}
		resp := get("GET", login)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Fatalf("%s: login: %d %q", tc.device, resp.StatusCode, resp.Header.Get("Location"))
		}
		var session *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == "ramekin_gk_"+port {
				session = ck
			}
		}
		if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%s: session cookie %+v", tc.device, session)
		}
		if resp := get("GET", login); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: login key reused: %d", tc.device, resp.StatusCode)
		}

		// Another device's cookies and the session cookie stay out of the request.
		resp = get("GET", origin+"/", session, &http.Cookie{Name: "rk1_gokrazy_xsrf", Value: "other"})
		s := <-seen
		if resp.StatusCode != http.StatusOK || s.path != tc.wantPath || s.auth != tc.wantAuth || s.cookie != "" {
			t.Fatalf("%s: overview: %d, gokrazy saw %+v", tc.device, resp.StatusCode, s)
		}
		var xsrf *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == "rk"+port+"_gokrazy_xsrf" {
				xsrf = ck
			}
		}
		if xsrf == nil || xsrf.Value != "123" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: gokrazy's cookie wasn't renamed for this device: %v", tc.device, resp.Header)
		}
		get("GET", origin+"/", session, xsrf)
		if s := <-seen; s.cookie != "gokrazy_xsrf=123" {
			t.Fatalf("%s: gokrazy got cookies %q", tc.device, s.cookie)
		}

		resp = get("POST", origin+"/restart", session)
		<-seen
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/status?path=x" {
			t.Fatalf("%s: redirect: %d %q", tc.device, resp.StatusCode, loc)
		}
		resp = get("GET", origin+"/absolute", session)
		<-seen
		if loc := resp.Header.Get("Location"); loc != "/status?path=y" {
			t.Fatalf("%s: absolute redirect not kept on this origin: %q", tc.device, loc)
		}

		req, _ := http.NewRequest("GET", origin+"/", nil)
		req.Host = "evil.example:" + port
		req.AddCookie(session)
		if resp, err := noRedirect.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: rebound Host allowed: %v %v", tc.device, resp, err)
		}
	}
}
