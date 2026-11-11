package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
)

// The GK button opens a device's gokrazy web interface through the console,
// which logs in for the browser: no gokrazy password or device token reaches
// it. Each device gets its own loopback port, because gokrazy's pages use
// root paths (/status, /assets/...), so they need an origin of their own.

const gkLoginPath = "/.ramekin-login"

// gokrazyAccess is how the console reaches a device's gokrazy interface.
type gokrazyAccess struct {
	via      string // "ramekind" (its API proxies gokrazy's socket) or "direct" (port 8181)
	base     string // for direct: http://host:port
	password string // for direct: gokrazy's web password
}

// gokrazyRoute picks how to reach a device's gokrazy interface: through
// ramekind when it supports that (any gokrazy, with or without a network
// password), else directly when the gokrazy password is known here.
func gokrazyRoute(p *config.Project, d deviceRecord, features []string) (gokrazyAccess, error) {
	if slices.Contains(features, agent.FeatureGokrazy) {
		return gokrazyAccess{via: "ramekind"}, nil
	}
	if base, pw := gokrazyDirect(d); base != "" {
		// A password can outlive the image it was for: a device reflashed
		// with official firmware serves gokrazy only on itself.
		if !gokrazyAnswers(base) {
			return gokrazyAccess{}, fmt.Errorf("a gokrazy password for %s is known here, but nothing answers at %s, so it may be out of date (official firmware serves gokrazy only on the device); update the device's firmware so its ramekind can open gokrazy", d.name, base)
		}
		return gokrazyAccess{via: "direct", base: base, password: pw}, nil
	}
	return gokrazyAccess{}, errors.New("this device's ramekind is too old to open its gokrazy dashboard, and no gokrazy password for it is known here; update its firmware, or run `ramekin device update " + d.name + "`")
}

// gokrazyAnswers reports whether something listens at base (http://host:port).
var gokrazyAnswers = func(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	c, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// gokrazyDirect returns a device's gokrazy address and web password, if the
// password is known: from the device record (set when claimed) or its gokrazy
// instance config. Devices without one (official firmware) don't serve
// gokrazy on the network at all.
func gokrazyDirect(d deviceRecord) (base, password string) {
	host, _, err := d.hostPort()
	if err != nil || host == "" {
		return "", ""
	}
	inst := instanceUpdate(d.name)
	password = firstNonEmpty(d.GokrazyPassword, inst.HTTPPassword)
	if password == "" {
		return "", ""
	}
	port := dashboardPort
	if inst.HTTPPort != "" {
		port = inst.HTTPPort // an adopted instance can keep its own port
	}
	return "http://" + net.JoinHostPort(host, port), password
}

// gkSession serves one device's gokrazy interface on a loopback port.
type gkSession struct {
	c      *console
	name   string
	addr   string // host:port the browser uses
	hosts  []string
	cookie string // session cookie name: per port, since cookies ignore ports
	secret string
	prefix string // added to gokrazy's cookie names, so devices' cookies don't mix
	srv    *http.Server

	mu     sync.Mutex
	logins map[string]time.Time // one-time login keys and when they expire
}

// openGokrazy answers the GK button: it starts (or reuses) the device's
// session and returns a one-time login URL for a new browser tab.
func (c *console) openGokrazy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(deviceNames(c.p), name) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	// Say now, on the card, if there's no way in, rather than in a new tab.
	d, err := resolveDevice(c.p, name)
	if err == nil {
		_, err = gokrazyRoute(c.p, d, c.features(name))
	}
	if err != nil {
		writeConsoleJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s, err := c.gkSessionFor(name)
	if err != nil {
		writeConsoleJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	key := randomHex(16)
	s.mu.Lock()
	for k, exp := range s.logins {
		if time.Now().After(exp) {
			delete(s.logins, k)
		}
	}
	s.logins[key] = time.Now().Add(time.Minute)
	s.mu.Unlock()
	writeConsoleJSON(w, http.StatusOK, map[string]any{"ok": true, "url": "http://" + s.addr + gkLoginPath + "?k=" + key})
}

func (c *console) gkSessionFor(name string) (*gkSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.gk[name]; s != nil {
		return s, nil
	}
	host := c.listenHost
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	s := &gkSession{
		c: c, name: name, addr: addr,
		hosts:  []string{addr, "localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port},
		cookie: "ramekin_gk_" + port, secret: randomHex(24), prefix: "rk" + port + "_",
		logins: map[string]time.Time{},
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	if c.gk == nil {
		c.gk = map[string]*gkSession{}
	}
	c.gk[name] = s
	return s, nil
}

// closeGokrazy stops every device's gokrazy session.
func (c *console) closeGokrazy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, s := range c.gk {
		s.srv.Close()
		delete(c.gk, name)
	}
}

func (s *gkSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !slices.Contains(s.hosts, r.Host) {
		http.Error(w, "unexpected Host header", http.StatusForbidden) // DNS rebinding
		return
	}
	if r.URL.Path == gkLoginPath {
		s.login(w, r)
		return
	}
	if c, err := r.Cookie(s.cookie); err != nil || c.Value != s.secret {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Open %s's gokrazy dashboard with the GK button in the Ramekin dashboard.\n", s.name)
		return
	}
	d, err := resolveDevice(s.c.p, s.name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	acc, err := gokrazyRoute(s.c.p, d, s.c.features(s.name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.proxy(d, acc).ServeHTTP(w, r)
}

// login trades a one-time key from the console for the session cookie, so
// only the console's own page can open a session.
func (s *gkSession) login(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("k")
	s.mu.Lock()
	exp, ok := s.logins[key]
	delete(s.logins, key)
	s.mu.Unlock()
	if key == "" || !ok || time.Now().After(exp) {
		http.Error(w, "this link has expired; use the GK button in the Ramekin dashboard again", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: s.secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *gkSession) proxy(d deviceRecord, acc gokrazyAccess) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := url.Parse(firstNonEmpty(acc.base, d.URL))
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			if acc.via == "ramekind" {
				pr.Out.URL.Path = agent.GokrazyPrefix + pr.In.URL.Path
				pr.Out.URL.RawPath = ""
				pr.Out.Header.Set("Authorization", "Bearer "+d.Token)
			} else {
				pr.Out.SetBasicAuth("gokrazy", acc.password)
			}
			s.deviceCookies(pr.Out)
		},
		ModifyResponse: func(resp *http.Response) error {
			s.renameSetCookies(resp.Header)
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && u.IsAbs() {
					u.Scheme, u.Host = "", "" // keep redirects on this origin
					resp.Header.Set("Location", u.String())
				}
			}
			resp.Header.Set("X-Frame-Options", "DENY")
			resp.Header.Set("Referrer-Policy", "no-referrer")
			return nil
		},
		FlushInterval: -1, // gokrazy streams logs
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("%s's gokrazy dashboard isn't answering: %v", s.name, err), http.StatusBadGateway)
		},
	}
}

// deviceCookies passes on only this device's cookies, under their own names.
// Cookies ignore ports, so the browser sends every device's (and the session
// cookie) to every port.
func (s *gkSession) deviceCookies(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if name, ok := strings.CutPrefix(c.Name, s.prefix); ok {
			r.AddCookie(&http.Cookie{Name: name, Value: c.Value})
		}
	}
}

// renameSetCookies gives gokrazy's cookies (its XSRF token) per-device names.
func (s *gkSession) renameSetCookies(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, v := range vals {
		c, err := http.ParseSetCookie(v)
		if err != nil {
			continue
		}
		c.Name, c.Domain, c.Secure = s.prefix+c.Name, "", false
		if c.Path == "" {
			c.Path = "/"
		}
		h.Add("Set-Cookie", c.String())
	}
}

// features returns what a device's ramekind last said it supports.
func (c *console) features(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if row := c.devices[name]; row != nil && row.Status != nil {
		return row.Status.Features
	}
	return nil
}

// gokrazyInfo describes the GK route for the device card.
func gokrazyInfo(p *config.Project, d deviceRecord, features []string) (available bool, note string) {
	acc, err := gokrazyRoute(p, d, features)
	if err != nil {
		return false, err.Error()
	}
	if acc.via == "ramekind" {
		return true, "opens through ramekind, logged in for you"
	}
	src := "the device record"
	if d.GokrazyPassword == "" {
		src = instanceConfigPath(d.name)
	} else if rel, err := filepath.Rel(p.Dir, deviceRecordPath(p, d.name)); err == nil {
		src = rel
	}
	return true, fmt.Sprintf("opens %s/, logged in for you with the gokrazy password from %s", acc.base, src)
}
