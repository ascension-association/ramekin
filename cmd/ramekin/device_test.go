package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ascension-association/ramekin/internal/config"
)

func mergeInto(t *testing.T, update map[string]any) map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(map[string]any{
		"Hostname":    "dev",
		"Environment": []string{"GOARCH=arm64"},
		"Packages":    []string{"github.com/gokrazy/breakglass"},
		"Update":      update,
	})
	os.WriteFile(path, b, 0o600)
	if err := mergeGokrazy(path, []byte(`{}`), "arm64"); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	b, _ = os.ReadFile(path)
	json.Unmarshal(b, &cfg)
	return cfg
}

func TestMergeMovesDashboard(t *testing.T) {
	cfg := mergeInto(t, map[string]any{"HTTPPassword": "pw"})
	upd := cfg["Update"].(map[string]any)
	if upd["HTTPPort"] != dashboardPort || upd["HTTPPassword"] != "pw" {
		t.Fatalf("Update = %v", upd)
	}
	if pkgs := cfg["Packages"].([]any); len(pkgs) != 2 || pkgs[1] != ramekindPkg {
		t.Fatalf("Packages = %v", pkgs)
	}
	// A deliberately chosen port is respected.
	if p := mergeInto(t, map[string]any{"HTTPPort": "1080"})["Update"].(map[string]any)["HTTPPort"]; p != "1080" {
		t.Fatalf("custom port overwritten: %v", p)
	}
}

func TestBridgeFallsThroughPorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dashboard") }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Port 1 is closed (the old dashboard after reboot); the bridge must fall
	// through to the next port.
	go bridge(ln, "127.0.0.1", []string{"1", port})
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "dashboard" {
		t.Fatalf("got %q", b)
	}
}

func readDevice(t *testing.T, cfgPath string) config.Device {
	t.Helper()
	b, _ := os.ReadFile(cfgPath)
	var cfg map[string]any
	json.Unmarshal(b, &cfg)
	files, err := deviceFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var d config.Device
	json.Unmarshal([]byte(files[config.DefaultDevicePath].(string)), &d)
	return d
}

func TestDeviceSet(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	cfgPath := filepath.Join(parent, "dev", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	b, _ := json.Marshal(map[string]any{"Hostname": "dev", "Environment": []string{"GOARCH=arm64"}})
	os.WriteFile(cfgPath, b, 0o600)
	if err := mergeGokrazy(cfgPath, []byte(`{"name":"dev","api_token":"secret","signing_pubkey":"PEM"}`), "arm64"); err != nil {
		t.Fatal(err)
	}
	set := func(args ...string) error {
		return deviceSet(ctx, append([]string{"dev", "--gokrazy-dir", parent}, args...))
	}

	if err := set("--health-http", "http://127.0.0.1/", "--health-grace", "5s",
		"--env", "A=1", "--env", "B=2", "--bind", "/dev/ttyUSB0:/dev/ttyUSB0"); err != nil {
		t.Fatal(err)
	}
	d := readDevice(t, cfgPath)
	if d.Health.HTTP != "http://127.0.0.1/" || time.Duration(d.Health.Grace) != 5*time.Second ||
		d.Container.Env["B"] != "2" || len(d.Container.Binds) != 3 || d.APIToken != "secret" {
		t.Fatalf("after set: %+v", d)
	}

	// Clearing, unsetting and unbinding.
	if err := set("--health-http", "", "--unset-env", "A", "--unbind", "/dev"); err != nil {
		t.Fatal(err)
	}
	d = readDevice(t, cfgPath)
	if d.Health.HTTP != "" || d.Container.Env["A"] != "" || len(d.Container.Binds) != 2 || time.Duration(d.Health.Grace) != 5*time.Second {
		t.Fatalf("after clear: %+v", d)
	}

	// Invalid input leaves the config untouched.
	before, _ := os.ReadFile(cfgPath)
	if err := set("--bind", "nodst"); err == nil {
		t.Fatal("bad bind accepted")
	}
	if err := set("--env", "novalue"); err == nil {
		t.Fatal("bad env accepted")
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Fatal("config changed by a rejected set")
	}

	// No flags: show, without exposing secrets.
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := set()
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if err != nil || strings.Contains(string(out), "secret") || !strings.Contains(string(out), "<hidden>") {
		t.Fatalf("show: err=%v\n%s", err, out)
	}
}

func TestSplitArgs(t *testing.T) {
	for in, want := range map[string][]string{
		`pg_isready -h 127.0.0.1`:               {"pg_isready", "-h", "127.0.0.1"},
		`sqlite3 /data/app.db 'select 1'`:       {"sqlite3", "/data/app.db", "select 1"},
		`sh -c "test -f /tmp/ready && echo ok"`: {"sh", "-c", "test -f /tmp/ready && echo ok"},
		`a\ b "it's" ''`:                        {"a b", "it's", ""},
		`   `:                                   nil,
	} {
		got, err := splitArgs(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("splitArgs(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`'open`, `"open`, `trailing\`} {
		if _, err := splitArgs(bad); err == nil {
			t.Errorf("splitArgs(%q): want error", bad)
		}
	}
}

func TestDeviceSetHealthExec(t *testing.T) {
	parent := t.TempDir()
	cfgPath := filepath.Join(parent, "dev", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	b, _ := json.Marshal(map[string]any{"Hostname": "dev"})
	os.WriteFile(cfgPath, b, 0o600)
	mergeGokrazy(cfgPath, []byte(`{"name":"dev","signing_pubkey":"PEM"}`), "arm64")
	set := func(args ...string) error {
		return deviceSet(context.Background(), append([]string{"dev", "--gokrazy-dir", parent}, args...))
	}
	if err := set("--health-exec", `sqlite3 /data/app.db 'select 1'`); err != nil {
		t.Fatal(err)
	}
	if got := readDevice(t, cfgPath).Health.Exec; strings.Join(got, "|") != "sqlite3|/data/app.db|select 1" {
		t.Fatalf("exec = %q", got)
	}
	if err := set("--health-exec", `''`); err == nil {
		t.Fatal("empty command accepted")
	}
	if err := set("--health-exec", ""); err != nil || len(readDevice(t, cfgPath).Health.Exec) != 0 {
		t.Fatalf("clear: %v", err)
	}
}
