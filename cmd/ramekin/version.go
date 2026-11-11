package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// modulePath is the module `go install` fetches ramekin from.
const modulePath = "github.com/ascension-association/ramekin"

// installPath is the package `go install` builds into the ramekin binary.
const installPath = modulePath + "/cmd/ramekin"

// updateCheckInterval is how often commands other than `version` and
// `update` look for a newer release.
const updateCheckInterval = 24 * time.Hour

// moduleProxy is where the latest version is looked up; tests replace it.
var moduleProxy = "https://proxy.golang.org"

// currentVersion is the version this binary was built from: a release such
// as v0.3.3 when installed with `go install ...@version`, or a development
// version for local builds.
func currentVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func cmdVersion(ctx context.Context, args []string) error {
	if _, err := parse(flags("version"), args); err != nil {
		return err
	}
	cur := currentVersion()
	fmt.Printf("ramekin %s\n", cur)
	latest, err := latestVersion(ctx)
	if err != nil {
		fmt.Printf("couldn't check for a newer version: %v\n", err)
		return nil
	}
	saveUpdateCheck(latest)
	switch {
	case newer(latest, cur):
		fmt.Printf("ramekin %s is available. Run `ramekin update` to install it.\n", latest)
	case isRelease(cur):
		fmt.Println("This is the latest version.")
	default:
		fmt.Printf("This is a development build; the latest release is %s.\n", latest)
	}
	return nil
}

func cmdUpdate(ctx context.Context, args []string) error {
	fl := flags("update")
	force := fl.Bool("force", false, "reinstall even if this is already the latest version")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	cur := currentVersion()
	latest, err := latestVersion(ctx)
	if err != nil {
		return fmt.Errorf("finding the latest version: %w", err)
	}
	saveUpdateCheck(latest)
	if !*force && isRelease(cur) && !newer(latest, cur) {
		fmt.Printf("ramekin %s is the latest version.\n", cur)
		return nil
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("updating needs Go (https://go.dev/dl/), which `go install` uses to build ramekin: %w", err)
	}
	fmt.Printf("updating ramekin %s to %s...\n", cur, latest)
	cmd := exec.CommandContext(ctx, goBin, "install", installPath+"@"+latest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install %s@%s: %w", installPath, latest, err)
	}
	dir, err := goInstallDir(ctx, goBin)
	if err != nil {
		return err
	}
	installed := filepath.Join(dir, "ramekin")
	fmt.Printf("installed ramekin %s at %s\n", latest, installed)
	onPath, _ := exec.LookPath("ramekin")
	if msg := pathWarning(installed, onPath, binaryVersion(onPath)); msg != "" {
		fmt.Println(msg)
	}
	return nil
}

// goInstallDir is where `go install` puts binaries: GOBIN, or the first
// GOPATH entry's bin directory.
func goInstallDir(ctx context.Context, goBin string) (string, error) {
	out, err := exec.CommandContext(ctx, goBin, "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) >= 1 && strings.TrimSpace(lines[0]) != "" {
		return strings.TrimSpace(lines[0]), nil
	}
	if len(lines) >= 2 {
		if first := filepath.SplitList(strings.TrimSpace(lines[1])); len(first) > 0 && first[0] != "" {
			return filepath.Join(first[0], "bin"), nil
		}
	}
	return "", errors.New("go env reports neither GOBIN nor GOPATH")
}

// pathWarning explains when the `ramekin` found first on PATH isn't the one
// just installed, so running `ramekin` would still start the old version.
func pathWarning(installed, onPath, onPathVersion string) string {
	same := func(a, b string) bool {
		ra, err1 := filepath.EvalSymlinks(a)
		rb, err2 := filepath.EvalSymlinks(b)
		if err1 == nil && err2 == nil {
			return ra == rb
		}
		return filepath.Clean(a) == filepath.Clean(b)
	}
	switch {
	case onPath == "":
		return fmt.Sprintf("%s isn't on your PATH: add %s to PATH to run it as `ramekin`.", installed, filepath.Dir(installed))
	case same(installed, onPath):
		return ""
	}
	v := ""
	if onPathVersion != "" {
		v = " (" + onPathVersion + ")"
	}
	return fmt.Sprintf("Running `ramekin` still starts %s%s, which comes first on your PATH. Remove it, or put %s earlier in PATH.", onPath, v, filepath.Dir(installed))
}

// binaryVersion reads the module version recorded in a Go binary, if any.
func binaryVersion(path string) string {
	if path == "" {
		return ""
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	return info.Main.Version
}

// latestVersion asks the Go module proxy (GOPROXY's first entry, if it's a
// URL) for the newest release, the one `go install ...@latest` would fetch.
func latestVersion(ctx context.Context) (string, error) {
	proxy := moduleProxy
	if first, _, _ := strings.Cut(env("GOPROXY"), ","); strings.HasPrefix(first, "https://") || strings.HasPrefix(first, "http://") {
		proxy = first
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(proxy, "/")+"/"+modulePath+"/@latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", proxy, resp.Status)
	}
	var v struct{ Version string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v); err != nil {
		return "", fmt.Errorf("%s: %w", proxy, err)
	}
	if !isRelease(v.Version) {
		return "", fmt.Errorf("%s reported %q, not a release version", proxy, v.Version)
	}
	return v.Version, nil
}

// parseRelease splits a release version such as v1.2.3 into numbers. Pre-
// releases, pseudo-versions and dirty builds aren't releases.
func parseRelease(v string) ([3]int, bool) {
	var n [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if !strings.HasPrefix(v, "v") || len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || (len(p) > 1 && p[0] == '0') {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func isRelease(v string) bool { _, ok := parseRelease(v); return ok }

// newer reports whether release a is newer than b. A development build (b
// not a release) is never reported as out of date.
func newer(a, b string) bool {
	x, ok1 := parseRelease(a)
	y, ok2 := parseRelease(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// updateCheck is the cached result of the last lookup, so commands look
// for a newer version at most once a day.
type updateCheck struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

func updateCheckPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ramekin", "update-check.json")
}

func loadUpdateCheck() (c updateCheck) {
	if p := updateCheckPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &c)
		}
	}
	return c
}

func saveUpdateCheck(latest string) {
	p := updateCheckPath()
	if p == "" {
		return
	}
	b, _ := json.Marshal(updateCheck{Checked: time.Now().UTC(), Latest: latest})
	if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
		os.WriteFile(p, b, 0o644)
	}
}

// updateNotifier tells the user about a newer release after a command runs,
// without slowing commands down or bothering scripts and CI.
type updateNotifier struct {
	current string
	cached  updateCheck
	result  chan string // the fetched latest version, if a lookup was started
}

// startUpdateCheck decides whether this run may show a notice and, if the
// cached answer is more than a day old, looks up the latest version in the
// background. It returns nil when no notice should be shown.
func startUpdateCheck(ctx context.Context, cmd string) *updateNotifier {
	if !updateNoticeWanted(cmd, currentVersion(), os.Getenv, stderrIsTerminal()) {
		return nil
	}
	n := &updateNotifier{current: currentVersion(), cached: loadUpdateCheck()}
	if time.Since(n.cached.Checked) >= updateCheckInterval {
		n.result = make(chan string, 1)
		go func() {
			latest, err := latestVersion(ctx)
			if err == nil {
				saveUpdateCheck(latest)
			} else {
				saveUpdateCheck(n.cached.Latest) // don't retry on every command while offline
			}
			n.result <- latest
		}()
	}
	return n
}

// updateNoticeWanted reports whether a command may print an update notice:
// not for version and update (they say it themselves), not in CI, not when
// turned off, not for development builds, and only when a person is likely
// to read stderr.
func updateNoticeWanted(cmd, current string, getenv func(string) string, terminal bool) bool {
	switch {
	case cmd == "version" || cmd == "update":
		return false
	case getenv("RAMEKIN_NO_UPDATE_CHECK") != "", getenv("CI") != "":
		return false
	case !isRelease(current), !terminal:
		return false
	}
	return true
}

// finish prints the notice if a newer version is known, waiting briefly for
// a background lookup that is still running.
func (n *updateNotifier) finish() {
	if n == nil {
		return
	}
	latest := n.cached.Latest
	if n.result != nil {
		select {
		case v := <-n.result:
			if v != "" {
				latest = v
			}
		case <-time.After(time.Second):
		}
	}
	if newer(latest, n.current) {
		fmt.Fprintf(os.Stderr, "\nramekin %s is available (you have %s). Run `ramekin update` to install it.\n", latest, n.current)
	}
}

func stderrIsTerminal() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
