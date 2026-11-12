package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.3.3", "v0.3.2", true},
		{"v0.10.0", "v0.9.9", true}, // numeric, not string, order
		{"v1.0.0", "v0.99.99", true},
		{"v0.3.2", "v0.3.2", false},
		{"v0.3.1", "v0.3.2", false},
		{"v0.3.3", "(devel)", false},                              // development builds never nag
		{"v0.3.3", "v0.3.4-0.20261111100000-abcdef123456", false}, // pseudo-version from a checkout
		{"v0.3.3", "v0.3.2+dirty", false},
		{"v0.4.0-rc.1", "v0.3.3", false}, // only releases are offered
		{"v01.2.3", "v0.3.3", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdateNoticeWanted(t *testing.T) {
	envs := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, c := range []struct {
		name, cmd, cur string
		env            func(string) string
		terminal, want bool
	}{
		{"normal command", "build", "v0.3.2", envs(), true, true},
		{"version says it itself", "version", "v0.3.2", envs(), true, false},
		{"update says it itself", "update", "v0.3.2", envs(), true, false},
		{"turned off", "build", "v0.3.2", envs("RAMEKIN_NO_UPDATE_CHECK", "1"), true, false},
		{"CI", "watch", "v0.3.2", envs("CI", "true"), true, false},
		{"script or pipe", "build", "v0.3.2", envs(), false, false},
		{"development build", "build", "(devel)", envs(), true, false},
	} {
		if got := updateNoticeWanted(c.cmd, c.cur, c.env, c.terminal); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeProxy answers @latest for ramekin's module with version.
func fakeProxy(t *testing.T, version string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+modulePath+"/@latest" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		io.WriteString(w, `{"Version":"`+version+`","Time":"2026-11-11T16:41:36Z"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestVersion(t *testing.T) {
	oldGH := githubReleasesLatest
	t.Cleanup(func() { githubReleasesLatest = oldGH })
	githubReleasesLatest = fakeGitHub(t, "", http.StatusNotFound) // proxy only
	t.Setenv("GOPROXY", "")
	old := moduleProxy
	t.Cleanup(func() { moduleProxy = old })

	moduleProxy = fakeProxy(t, "v0.3.3", http.StatusOK).URL
	if v, err := latestVersion(context.Background()); err != nil || v != "v0.3.3" {
		t.Fatalf("got %q, %v", v, err)
	}
	moduleProxy = fakeProxy(t, "v0.4.0-0.20261111-abc", http.StatusOK).URL
	if _, err := latestVersion(context.Background()); err == nil {
		t.Error("accepted a pseudo-version as the latest release")
	}
	moduleProxy = fakeProxy(t, "", http.StatusNotFound).URL
	if _, err := latestVersion(context.Background()); err == nil {
		t.Error("accepted an error answer")
	}

	// GOPROXY's first entry wins over the default, as with `go install`.
	moduleProxy = "http://127.0.0.1:1"
	t.Setenv("GOPROXY", fakeProxy(t, "v0.5.0", http.StatusOK).URL+",direct")
	if v, err := latestVersion(context.Background()); err != nil || v != "v0.5.0" {
		t.Fatalf("with GOPROXY: got %q, %v", v, err)
	}
}

func TestPathWarning(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "gobin", "ramekin")
	other := filepath.Join(dir, "usr-local-bin", "ramekin")
	link := filepath.Join(dir, "link", "ramekin")
	for _, p := range []string{installed, other} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o755)
	}
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(installed, link); err != nil {
		t.Fatal(err)
	}
	if msg := pathWarning(installed, installed, "v0.3.3"); msg != "" {
		t.Errorf("same binary: %q", msg)
	}
	if msg := pathWarning(installed, link, "v0.3.3"); msg != "" {
		t.Errorf("symlink to the same binary: %q", msg)
	}
	msg := pathWarning(installed, other, "v0.3.2")
	if !strings.Contains(msg, other) || !strings.Contains(msg, "v0.3.2") || !strings.Contains(msg, filepath.Dir(installed)) {
		t.Errorf("shadowed binary: %q", msg)
	}
	if msg := pathWarning(installed, "", ""); !strings.Contains(msg, "isn't on your PATH") {
		t.Errorf("not on PATH: %q", msg)
	}
}

// captureStderr returns what f writes to os.Stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	f()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestUpdateNotifier(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fresh := updateCheck{Checked: time.Now(), Latest: "v0.3.3"}

	// A cached answer is enough; nothing is fetched.
	out := captureStderr(t, func() { (&updateNotifier{current: "v0.3.2", cached: fresh}).finish() })
	if !strings.Contains(out, "ramekin v0.3.3 is available (you have v0.3.2)") || !strings.Contains(out, "ramekin update") {
		t.Errorf("notice: %q", out)
	}
	if out := captureStderr(t, func() { (&updateNotifier{current: "v0.3.3", cached: fresh}).finish() }); out != "" {
		t.Errorf("up to date, but printed %q", out)
	}
	// A background lookup's answer replaces an older cached one.
	res := make(chan string, 1)
	res <- "v0.4.0"
	out = captureStderr(t, func() { (&updateNotifier{current: "v0.3.3", cached: fresh, result: res}).finish() })
	if !strings.Contains(out, "v0.4.0 is available") {
		t.Errorf("fetched notice: %q", out)
	}
	// A lookup that hasn't finished doesn't hold the command up for long.
	start := time.Now()
	captureStderr(t, func() { (&updateNotifier{current: "v0.3.3", result: make(chan string)}).finish() })
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("waited %v for a slow lookup", d)
	}
	var none *updateNotifier
	none.finish() // no notice wanted: nothing happens
}

func TestUpdateCheckCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if c := loadUpdateCheck(); !c.Checked.IsZero() {
		t.Fatalf("empty cache: %+v", c)
	}
	saveUpdateCheck("v0.3.3")
	c := loadUpdateCheck()
	if c.Latest != "v0.3.3" || time.Since(c.Checked) > time.Minute {
		t.Fatalf("saved cache: %+v", c)
	}
}

// fakeProxyList serves a version list and @latest; an empty value is a 404.
func fakeProxyList(t *testing.T, list, latest string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+modulePath+"/@v/list" && list != "":
			io.WriteString(w, list)
		case r.URL.Path == "/"+modulePath+"/@latest" && latest != "":
			io.WriteString(w, `{"Version":"`+latest+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestLatestVersionLaggingProxy: right after a release, the proxy's version
// list has it but @latest still names the previous one.
func TestLatestVersionLaggingProxy(t *testing.T) {
	oldGH := githubReleasesLatest
	t.Cleanup(func() { githubReleasesLatest = oldGH })
	githubReleasesLatest = fakeGitHub(t, "", http.StatusNotFound) // proxy only
	t.Setenv("GOPROXY", "")
	old := moduleProxy
	t.Cleanup(func() { moduleProxy = old })
	for _, c := range []struct {
		name, list, latest, want string
	}{
		{"@latest lags", "v0.3.5\nv0.3.6\nv0.3.7\n", "v0.3.6", "v0.3.7"},
		{"list order and junk", "v0.3.10\nv0.4.0-rc.1\nv0.3.9\nv0.3.11-0.20261112-abcdef\n", "v0.3.9", "v0.3.10"},
		{"no list", "", "v0.3.7", "v0.3.7"},
		{"no @latest", "v0.3.7\n", "", "v0.3.7"},
		{"neither", "", "", ""},
	} {
		moduleProxy = fakeProxyList(t, c.list, c.latest)
		got, err := latestVersion(context.Background())
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestUpdateArgs(t *testing.T) {
	for _, args := range [][]string{{"banana"}, {"v0.3"}, {"v0.4.0-rc.1"}} {
		if err := cmdUpdate(context.Background(), args); err == nil || !strings.Contains(err.Error(), "isn't a release version") {
			t.Errorf("update %v: %v", args, err)
		}
	}
	if err := cmdUpdate(context.Background(), []string{"v0.3.7", "v0.3.8"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("two versions: %v", err)
	}
}

// fakeGitHub answers GitHub's latest-release API with tag, or with status
// when it isn't 200.
func fakeGitHub(t *testing.T, tag string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "rate limited", status)
			return
		}
		io.WriteString(w, `{"tag_name":"`+tag+`","draft":false,"prerelease":false}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestLatestVersionGitHub: the proxy only learns of a release once someone
// fetches it, so right after a release it can name the previous one in both
// its list and @latest, while GitHub already has it.
func TestLatestVersionGitHub(t *testing.T) {
	t.Setenv("GOPROXY", "")
	oldProxy, oldGH := moduleProxy, githubReleasesLatest
	t.Cleanup(func() { moduleProxy, githubReleasesLatest = oldProxy, oldGH })
	for _, c := range []struct {
		name, list, latest, gh string
		ghStatus               int
		want                   string
	}{
		{"GitHub ahead of the proxy", "v0.3.7\nv0.3.8\n", "v0.3.7", "v0.3.9", 200, "v0.3.9"},
		{"GitHub rate limited", "v0.3.7\nv0.3.8\n", "v0.3.7", "", 403, "v0.3.8"},
		{"GitHub behind a newer proxy", "v0.3.10\n", "v0.3.10", "v0.3.9", 200, "v0.3.10"},
		{"only GitHub", "", "", "v0.3.9", 200, "v0.3.9"},
		{"GitHub tag isn't a release", "v0.3.8\n", "", "nightly", 200, "v0.3.8"},
	} {
		moduleProxy = fakeProxyList(t, c.list, c.latest)
		githubReleasesLatest = fakeGitHub(t, c.gh, c.ghStatus)
		got, err := latestVersion(context.Background())
		if got != c.want || err != nil {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}
