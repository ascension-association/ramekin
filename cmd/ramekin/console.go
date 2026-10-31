package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
)

//go:embed console
var consoleFiles embed.FS

// pollInterval is how often the console asks every device for its status.
const pollInterval = 5 * time.Second

// fleetDevice is one row of the console: a device record and what it last
// reported. Tokens stay in the console process; the page never sees them.
type fleetDevice struct {
	Name   string              `json:"name"`
	URL    string              `json:"url"`
	Health string              `json:"health"` // healthy, warning, unhealthy, updating, idle or offline
	Detail string              `json:"detail,omitempty"`
	Status *agent.DeviceStatus `json:"status,omitempty"`
	Polled time.Time           `json:"polled"`
	// Seen is when the device last answered, so an offline row can say how long ago.
	Seen time.Time `json:"seen,omitzero"`
	Busy string    `json:"busy,omitempty"` // an action in progress, e.g. "switching to 1.2.0"
}

type console struct {
	p   *config.Project
	key string // sent by the page with every API call; see guard

	mu       sync.Mutex
	devices  map[string]*fleetDevice
	searched map[string]time.Time // last MAC search per device
}

func cmdDashboard(ctx context.Context, args []string) error {
	fl := flags("--dashboard")
	listen := fl.String("listen", "127.0.0.1:8099", "address for the dashboard (loopback only)")
	noOpen := fl.Bool("no-open", false, "don't open a browser")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if err := loopbackOnly(*listen); err != nil {
		return err
	}
	c := &console{p: p, key: randomHex(16), devices: map[string]*fleetDevice{}, searched: map[string]time.Time{}}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: c.handler(ln.Addr().String()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go c.pollLoop(ctx)

	u := "http://" + ln.Addr().String() + "/"
	fmt.Printf("Ramekin dashboard for %s: %s\n", p.Name, u)
	fmt.Printf("watching the devices in %s; press Ctrl-C to stop\n", p.Path(filepath.Join(".ramekin", "devices")))
	if !*noOpen {
		openBrowser(u)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopbackOnly refuses other addresses: the console has no login, and it
// holds the device tokens. Use SSH port forwarding to reach it remotely.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--listen %s: the dashboard has no login, so it only listens on a loopback address (e.g. 127.0.0.1:8099); to reach it from another machine, use ssh -L", addr)
}

func openBrowser(u string) {
	name := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name = "explorer"
	}
	if exec.Command(name, u).Start() != nil {
		fmt.Println("open that address in a browser")
	}
}

func (c *console) handler(addr string) http.Handler {
	static, _ := fs.Sub(consoleFiles, "console")
	page := template.Must(template.ParseFS(static, "index.html"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, map[string]string{"Project": c.p.Name, "Key": c.key})
	})
	mux.Handle("GET /app.js", http.FileServerFS(static))
	mux.Handle("GET /app.css", http.FileServerFS(static))
	mux.HandleFunc("GET /api/fleet", func(w http.ResponseWriter, r *http.Request) {
		writeConsoleJSON(w, http.StatusOK, map[string]any{"project": c.p.Name, "interval_seconds": pollInterval.Seconds(), "devices": c.snapshot()})
	})
	mux.HandleFunc("POST /api/devices/{name}/switch", c.switchVersion)
	return c.guard(addr, mux)
}

// guard protects the console from other web pages open in the same browser.
// Checking Host defeats DNS rebinding. A key that only the console's own page
// knows blocks cross-site requests, since other sites can't read the page or
// set custom headers.
func (c *console) guard(addr string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	hosts := []string{addr, "localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(hosts, r.Host) {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-Ramekin-Console") != c.key {
			http.Error(w, "missing console key; reload the page", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (c *console) snapshot() []fleetDevice {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]fleetDevice, 0, len(c.devices))
	for _, d := range c.devices {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b fleetDevice) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// deviceNames lists the project's device records.
func deviceNames(p *config.Project) []string {
	ents, _ := os.ReadDir(p.Path(filepath.Join(".ramekin", "devices")))
	var names []string
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			names = append(names, name)
		}
	}
	return names
}

func (c *console) pollLoop(ctx context.Context) {
	for {
		c.pollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

func (c *console) pollAll(ctx context.Context) {
	names := deviceNames(c.p)
	c.mu.Lock()
	for name := range c.devices {
		if !slices.Contains(names, name) {
			delete(c.devices, name) // its record was removed
		}
	}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.poll(ctx, name)
		}()
	}
	wg.Wait()
}

func (c *console) poll(ctx context.Context, name string) {
	row := fleetDevice{Name: name, Polled: time.Now().UTC()}
	d, err := resolveDevice(c.p, name)
	row.URL = d.URL
	if err == nil {
		var st agent.DeviceStatus
		if err = c.fetchStatus(ctx, &d, &st); err == nil {
			row.Status = &st
			row.Seen = row.Polled
			row.Health, row.Detail = classify(&st)
			row.URL = d.URL // locate may have found it at a new address
		}
	}
	if err != nil {
		row.Health, row.Detail = "offline", err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.devices[name]; old != nil {
		row.Busy = old.Busy
		if row.Status == nil {
			row.Status, row.Seen = old.Status, old.Seen // keep the last report on screen
		}
	}
	c.devices[name] = &row
}

// fetchStatus asks a device for its status. If it doesn't answer and its MAC
// is known, the local network is searched for it, at most every 2 minutes.
func (c *console) fetchStatus(ctx context.Context, d *deviceRecord, st *agent.DeviceStatus) error {
	host, port, err := d.hostPort()
	if err != nil {
		return err
	}
	if !reachable(host, port) {
		c.mu.Lock()
		search := d.MAC != "" && time.Since(c.searched[d.name]) > 2*time.Minute
		if search {
			c.searched[d.name] = time.Now()
		}
		c.mu.Unlock()
		if !search {
			return fmt.Errorf("not reachable at %s", net.JoinHostPort(host, port))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := d.do(ctx, http.MethodGet, "/v1/status", nil, 10*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(st)
}

// classify sums up a device's status as one health word and a reason.
func classify(st *agent.DeviceStatus) (health, detail string) {
	switch {
	case st.Trial != nil:
		return "updating", "health-checking " + st.Trial.Version
	case st.Current == nil:
		return "idle", "no release installed yet"
	case st.Metrics == nil:
		return "warning", "this device's ramekind is older than the console, so it doesn't report app health or resource use; run `ramekin device update " + st.Device + "`"
	case !st.App.Running:
		detail = "app is not running"
		if st.App.LastExit != "" {
			detail += " (last exit: " + st.App.LastExit + ")"
		}
		return "unhealthy", detail
	case st.LastError != "":
		return "warning", "update check failed: " + st.LastError
	case st.App.Restarts > 0:
		return "warning", fmt.Sprintf("app restarted %d times (last exit: %s)", st.App.Restarts, st.App.LastExit)
	}
	return "healthy", ""
}

// switchVersion moves a device to one of its kept releases (?sha=, with
// ?version= for messages).
func (c *console) switchVersion(w http.ResponseWriter, r *http.Request) {
	name, sha, version := r.PathValue("name"), r.URL.Query().Get("sha"), r.URL.Query().Get("version")
	if !slices.Contains(deviceNames(c.p), name) {
		http.Error(w, "unknown device", http.StatusNotFound)
		return
	}
	if sha == "" {
		http.Error(w, "sha is required", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	row := c.devices[name]
	if row != nil && row.Busy != "" {
		c.mu.Unlock()
		http.Error(w, name+" is busy: "+row.Busy, http.StatusConflict)
		return
	}
	if row != nil {
		row.Busy = "switching to " + version
	}
	c.mu.Unlock()
	fmt.Printf("switching %s to %s\n", name, version)
	res, err := switchDevice(context.WithoutCancel(r.Context()), c.p, name, url.Values{"sha": {sha}}, version)
	c.mu.Lock()
	if row := c.devices[name]; row != nil {
		row.Busy = ""
	}
	c.mu.Unlock()
	go c.poll(context.Background(), name)
	if err != nil {
		fmt.Printf("switching %s failed: %v\n", name, err)
		writeConsoleJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	fmt.Printf("%s is running %s\n", name, res.State.Current.Version)
	writeConsoleJSON(w, http.StatusOK, map[string]any{"ok": true, "version": res.State.Current.Version})
}

// switchDevice asks a device to run one of its earlier releases: q names it
// (sha= or version=), or is empty for the previous one. The device
// health-checks it first and keeps the current release if it fails.
func switchDevice(ctx context.Context, p *config.Project, name string, q url.Values, label string) (*agent.PushResult, error) {
	d, err := resolveDevice(p, name)
	if err != nil {
		return nil, err
	}
	path := "/v1/rollback"
	if len(q) > 0 {
		path = "/v1/switch?" + q.Encode()
	}
	resp, err := d.do(ctx, http.MethodPost, path, nil, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res agent.PushResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("device returned %s (is its ramekind older than this ramekin? run `ramekin device update %s`)", resp.Status, name)
	}
	if res.OK {
		return &res, nil
	}
	if res.RolledBack && res.State.Current != nil {
		// "new release failed and was rolled back: <version>: <cause>"
		v, cause, _ := strings.Cut(strings.TrimPrefix(res.Error, agent.ErrRolledBack.Error()+": "), ": ")
		return &res, fmt.Errorf("%s failed its health check (%s), so %s is still running %s", v, cause, name, res.State.Current.Version)
	}
	if strings.Contains(res.Error, agent.ErrNotKept.Error()) {
		return &res, fmt.Errorf("%s doesn't have %s any more; it keeps only its %d most recent successful releases", name, label, agent.KeepReleases+1)
	}
	return &res, errors.New(res.Error)
}

func cmdRollback(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: ramekin rollback <device> [version]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	q, label := url.Values{}, "its previous release"
	if len(args) == 2 {
		q.Set("version", args[1])
		label = args[1]
	}
	fmt.Printf("switching %s to %s; waiting for health check...\n", args[0], label)
	res, err := switchDevice(ctx, p, args[0], q, label)
	if err != nil {
		return err
	}
	fmt.Printf("OK: %s is running %s (previous: %s)\n", args[0], res.State.Current.Version, res.State.Previous.Version)
	return nil
}

func writeConsoleJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
