package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

const ramekindPkg = "github.com/ascension-association/ramekin/cmd/ramekind"

const lockscreenPkg = "github.com/ascension-association/gk-lockscreen"

// Ramekin devices run ramekind, and optionally gk-lockscreen, as gokrazy
// Packages. Devices you develop with and push to locally may also run
// gokrazy's development tools (devPackages); the fleet image may not. The
// kernel comes from KernelPackage, which must be a repository named "kernel"
// or "kernel.<something>". gokrazy's own system services are configured
// separately (GokrazyPackages).

// devPackages are allowed on local development devices only.
var devPackages = []string{
	"github.com/gokrazy/fbstatus",
	"github.com/gokrazy/hello",
	"github.com/gokrazy/serial-busybox",
	"github.com/gokrazy/breakglass",
}

func isKernelPackage(pkg string) bool {
	parts := strings.Split(pkg, "/")
	if len(parts) < 3 {
		return false
	}
	repo := parts[2] // host/owner/repo[/...]
	return repo == "kernel" || strings.HasPrefix(repo, "kernel.")
}

func allowedPackage(pkg string, fleet bool) bool {
	return pkg == ramekindPkg || pkg == lockscreenPkg || (!fleet && slices.Contains(devPackages, pkg))
}

// enforcePackages makes a gokrazy config's Packages exactly the allowed set
// (for the fleet image, or for a local device): it removes anything else
// (and its PackageConfig) and adds missing required packages. It fails if
// KernelPackage isn't a kernel repository.
func enforcePackages(cfg map[string]any, fleet bool) (removed, added []string, err error) {
	pkgs, _ := cfg["Packages"].([]any)
	// gok fetches KernelPackage without building it (it only builds main
	// packages listed in Packages); unset means its default,
	// github.com/gokrazy/kernel.rpi.
	if k, set := cfg["KernelPackage"]; set {
		if ks, _ := k.(string); !isKernelPackage(ks) {
			return nil, nil, fmt.Errorf("KernelPackage %q must be a repository named kernel or kernel.<name>", k)
		}
	}
	var keep []any
	have := map[string]bool{}
	for _, p := range pkgs {
		s, _ := p.(string)
		switch {
		case have[s]:
		case allowedPackage(s, fleet):
			keep = append(keep, s)
			have[s] = true
		default:
			removed = append(removed, fmt.Sprint(p))
		}
	}
	if !have[ramekindPkg] {
		keep = append(keep, ramekindPkg)
		added = append(added, ramekindPkg)
	}
	cfg["Packages"] = keep
	if pc, ok := cfg["PackageConfig"].(map[string]any); ok {
		for _, r := range removed {
			delete(pc, r)
		}
	}
	return removed, added, nil
}

func reportPackages(cfgPath string, removed, added []string, fleet bool) {
	if len(removed)+len(added) == 0 {
		return
	}
	if fleet {
		fmt.Fprintf(os.Stderr, "%s: the fleet image runs only ramekind and, optionally, gk-lockscreen (plus the KernelPackage)\n", cfgPath)
	} else {
		fmt.Fprintf(os.Stderr, "%s: Ramekin devices run only ramekind, optionally gk-lockscreen, and gokrazy's development tools (%s) (plus the KernelPackage)\n", cfgPath, strings.Join(devPackages, ", "))
	}
	for _, r := range removed {
		fmt.Fprintf(os.Stderr, "  removed %s\n", r)
	}
	for _, a := range added {
		fmt.Fprintf(os.Stderr, "  added   %s\n", a)
	}
}

// dashboardPort is where Ramekin devices serve the gokrazy web interface and
// update API, keeping port 80 free for the container (which shares the host
// network).
const dashboardPort = "8181"

// deviceRecord is the workstation's handle on a device, kept in
// .ramekin/devices/<name>.json (gitignored: it holds the API token).
type deviceRecord struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// MAC lets ramekin find the device again after DHCP gives it a new address.
	MAC string `json:"mac,omitempty"`

	// Set when the device is claimed (`ramekin device claim`): its signing
	// key, which every answer from it must be signed with, and the rest of
	// its identity.
	PublicKey       string `json:"public_key,omitempty"`
	HardwareID      string `json:"hardware_id,omitempty"`
	AgeRecipient    string `json:"age_recipient,omitempty"`
	GokrazyPassword string `json:"gokrazy_password,omitempty"`

	name, path string // where the record was loaded from; empty for URLs
}

func (d *deviceRecord) save() error {
	if d.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.path, b, 0o600)
}

func (d *deviceRecord) hostPort() (string, string, error) {
	u, err := url.Parse(d.URL)
	if err != nil {
		return "", "", err
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	return u.Hostname(), port, nil
}

// locate makes sure the device answers at d.URL. If it doesn't and its MAC
// is known, it searches the local network and updates the record. Once the
// device answers, its MAC is learned for next time.
func (d *deviceRecord) locate(ctx context.Context) error {
	host, port, err := d.hostPort()
	if err != nil {
		return err
	}
	if !reachable(host, port) {
		if d.MAC == "" {
			return fmt.Errorf("device not reachable at %s (its MAC isn't known yet, so it can't be searched for; update the record's url)", d.URL)
		}
		fmt.Fprintf(os.Stderr, "%s not reachable at %s; searching the local network for MAC %s...\n", d.name, net.JoinHostPort(host, port), d.MAC)
		ip, err := findByMAC(ctx, d.MAC, port)
		if err != nil {
			return err
		}
		u, _ := url.Parse(d.URL)
		u.Host = net.JoinHostPort(ip, port)
		d.URL = u.String()
		host = ip
		fmt.Fprintf(os.Stderr, "found %s at %s\n", d.name, ip)
		if err := d.save(); err != nil {
			return err
		}
	}
	if mac := macFor(host); mac != "" && mac != d.MAC && d.path != "" {
		d.MAC = mac
		return d.save()
	}
	return nil
}

func deviceRecordPath(p *config.Project, name string) string {
	return p.Path(filepath.Join(".ramekin", "devices", name+".json"))
}

// resolveDevice accepts a device name or a URL (token from RAMEKIN_DEVICE_TOKEN).
func resolveDevice(p *config.Project, arg string) (deviceRecord, error) {
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return deviceRecord{URL: strings.TrimSuffix(arg, "/"), Token: env("RAMEKIN_DEVICE_TOKEN")}, nil
	}
	r := deviceRecord{name: arg, path: deviceRecordPath(p, arg)}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return r, fmt.Errorf("unknown device %q (create it with `ramekin device init %s`, or pass a URL)", arg, arg)
	}
	return r, json.Unmarshal(b, &r)
}

func (d *deviceRecord) do(ctx context.Context, method, path string, body io.Reader, timeout time.Duration) (*http.Response, error) {
	if err := d.locate(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, d.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, errors.New("device rejected the API token")
	}
	return resp, nil
}

func cmdDevice(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ramekin device init|claim|release|set|update|find|list ...")
	}
	switch args[0] {
	case "init":
		return deviceInit(args[1:])
	case "claim":
		return deviceClaim(ctx, args[1:])
	case "release":
		return deviceRelease(ctx, args[1:])
	case "set":
		return deviceSet(ctx, args[1:])
	case "update":
		return deviceUpdate(ctx, args[1:])
	case "find":
		return deviceFind(ctx, args[1:])
	case "list":
		p, err := loadProject()
		if err != nil {
			return err
		}
		ents, _ := os.ReadDir(p.Path(filepath.Join(".ramekin", "devices")))
		for _, e := range ents {
			name := strings.TrimSuffix(e.Name(), ".json")
			if r, err := resolveDevice(p, name); err == nil {
				fmt.Printf("%-20s %s\n", name, r.URL)
			}
		}
		return nil
	}
	return fmt.Errorf("unknown device subcommand %q", args[0])
}

func deviceInit(args []string) error {
	fl := flags("device init")
	target := fl.String("target", "rpi", "hardware: rpi (Raspberry Pi 3/4/5, arm64) or amd64 (PCs, VMs)")
	channel := fl.String("channel", "stable", "release channel the device follows")
	noUpdates := fl.Bool("no-updates", false, "development device: only accept `ramekin push`")
	ownKey := fl.Bool("own-key", false, "give the device its own age key instead of the shared fleet key")
	healthHTTP := fl.String("health-http", "", "URL that must return 2xx for a new release to be accepted")
	healthTCP := fl.String("health-tcp", "", "host:port that must accept connections for a new release to be accepted")
	healthExec := fl.String("health-exec", "", "command run inside the container that must exit 0 for a new release to be accepted")
	lockscreen := fl.Bool("lockscreen", false, "also run gk-lockscreen ("+lockscreenPkg+") on a new instance")
	statusURL := fl.String("status-url", "", "URL the device POSTs a JSON status report to after each update check (optional)")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	url := fl.String("url", "", "how this workstation reaches the device API (default http://<name>:8095)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device init <name> [flags]")
	}
	name := pos[0]
	p, err := loadProject()
	if err != nil {
		return err
	}
	execArgs, err := splitArgs(*healthExec)
	if err != nil {
		return fmt.Errorf("--health-exec: %w", err)
	}

	arch, kernel, firmware, eeprom := "arm64", "github.com/gokrazy/kernel.rpi", "github.com/gokrazy/firmware", "github.com/gokrazy/rpi-eeprom"
	switch *target {
	case "rpi":
	case "amd64":
		arch, kernel, firmware, eeprom = "amd64", "github.com/gokrazy/kernel.amd64", "", ""
	default:
		return fmt.Errorf("unknown --target %q", *target)
	}
	if arch != p.Arch() {
		fmt.Fprintf(os.Stderr, "warning: device is %s but ramekin.yaml platform is %s\n", arch, p.Platform)
	}

	pub, err := os.ReadFile(p.Path(p.Signing.PubKey))
	if err != nil {
		return fmt.Errorf("%w (run `ramekin keys generate` first)", err)
	}
	var identity string
	if *ownKey {
		path := p.Path(filepath.Join("keys", "devices", name+".agekey"))
		rcpt, err := keys.GenerateAge(path)
		if err != nil {
			return err
		}
		if err := keys.AppendRecipient(p.Path(p.Encrypt.RecipientsFile), rcpt, "device "+name); err != nil {
			return err
		}
		fmt.Printf("device key %s added to %s; releases published from now on include it\n", rcpt, p.Encrypt.RecipientsFile)
		identity, err = keys.ReadAgeIdentity(path)
		if err != nil {
			return err
		}
	} else if identity, err = keys.ReadAgeIdentity(p.Path("keys/fleet.agekey")); err != nil {
		return err
	}

	dev := config.Device{
		Name:          name,
		APIToken:      randomHex(24),
		SigningPubKey: string(pub),
		AgeIdentity:   identity,
		// Devices download over HTTPS and hold no storage credentials.
		Update: config.Update{
			Enabled:   !*noUpdates,
			Channel:   *channel,
			StatusURL: *statusURL,
		},
		Container: config.Container{Hostname: name},
		Health:    config.Health{HTTP: *healthHTTP, TCP: *healthTCP, Exec: execArgs},
	}
	if dev.Update.Enabled {
		if p.Releases.GitHub == "" {
			return errors.New("releases.github must be set in ramekin.yaml (or use --no-updates for a dev device)")
		}
		dev.Update.URL = p.ChannelURL()
	}
	devJSON, err := json.MarshalIndent(dev, "", "  ")
	if err != nil {
		return err
	}
	if _, err := config.ParseDevice(devJSON); err != nil {
		return fmt.Errorf("generated device config is invalid: %w", err)
	}

	packages := []string{ramekindPkg}
	if *lockscreen {
		packages = append(packages, lockscreenPkg)
	}
	gok := map[string]any{
		"Hostname":    name,
		"Update":      map[string]any{"HTTPPassword": randomHex(16), "HTTPPort": dashboardPort},
		"Environment": []string{"GOOS=linux", "GOARCH=" + arch},
		"Packages":    packages,
		"PackageConfig": map[string]any{
			ramekindPkg: map[string]any{
				"ExtraFileContents": map[string]string{config.DefaultDevicePath: string(devJSON)},
			},
		},
		"SerialConsole":   "disabled",
		"KernelPackage":   kernel,
		"FirmwarePackage": firmware,
		"EEPROMPackage":   eeprom,
	}
	*parent = gokrazyDir(*parent)
	instDir := filepath.Join(*parent, name)
	cfgPath := filepath.Join(instDir, "config.json")
	merged := false
	if _, err := os.Stat(cfgPath); err == nil {
		// Existing instance (already flashed): add ramekind, keep everything else.
		if err := mergeGokrazy(cfgPath, devJSON, p.Arch()); err != nil {
			return err
		}
		merged = true
	} else {
		if err := os.MkdirAll(instDir, 0o700); err != nil {
			return err
		}
		gokJSON, _ := json.MarshalIndent(gok, "", "    ")
		if err := os.WriteFile(cfgPath, append(gokJSON, '\n'), 0o600); err != nil {
			return err
		}
	}

	if *url == "" {
		*url = "http://" + name + ":8095"
	}
	rec, _ := json.MarshalIndent(deviceRecord{URL: strings.TrimSuffix(*url, "/"), Token: dev.APIToken}, "", "  ")
	recPath := deviceRecordPath(p, name)
	if err := os.MkdirAll(filepath.Dir(recPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(recPath, rec, 0o600); err != nil {
		return err
	}

	if merged {
		fmt.Printf(`added ramekind to existing gokrazy instance %s (backup: config.json.bak)
device record: %s

If ramekind is not yet in the instance's build directory, add it first, e.g.
  gok --parent_dir %s -i %s add %s
Then deploy it to the running device (it reboots). This also moves the gokrazy
dashboard from port 80 to %s; ramekin bridges that one-time change:
  ramekin device update %s --gokrazy-dir %s
Then test your container on it:
  ramekin push %s
`, cfgPath, recPath, *parent, name, ramekindPkg, dashboardPort, name, *parent, name)
		return nil
	}
	fmt.Printf(`gokrazy instance: %s (contains secrets; keep it private)
device record:    %s

Flash an SD card / disk (needs github.com/gokrazy/tools/cmd/gok):
  gok --parent_dir %s -i %s overwrite --full /dev/sdX

The gokrazy dashboard is at http://%s:%s/ (port 80 is left for your app).

Later OS/agent updates over the network:
  ramekin device update %s --gokrazy-dir %s

Then test your container on it:
  ramekin push %s
`, cfgPath, recPath, *parent, name, name, dashboardPort, name, *parent, name)
	return nil
}

func cmdPush(ctx context.Context, args []string) error {
	fl := flags("push")
	version := fl.String("version", "", "version label shown on the device (default dev-<time>)")
	force := fl.Bool("force", false, "install even if this exact image failed on the device before")
	noScan := fl.Bool("no-scan", false, "skip the CVE scan")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: ramekin push <device> [sif]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, pos[0])
	if err != nil {
		return err
	}
	src := sifArg(p, pos, 1)
	if !*noScan {
		rep, err := scanSIF(ctx, p, src, sbomPath(src))
		if err != nil {
			return err
		}
		if err := applyPolicy(ctx, p, rep, p.Name+" (dev push)", false); err != nil {
			return err
		}
	}

	warnNoShell(ctx, src)

	// Sign a copy so repeated pushes don't stack signatures on the build output.
	signed := strings.TrimSuffix(src, ".sif") + ".signed.sif"
	if err := copyFile(src, signed); err != nil {
		return err
	}
	defer os.Remove(signed)
	s, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	if err := sifimg.Sign(ctx, signed, s); err != nil {
		return err
	}
	f, err := os.Open(signed)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, _ := f.Stat()

	if *version == "" {
		*version = "dev-" + time.Now().Format("20060102-150405")
	}
	q := "?version=" + *version
	if *force {
		q += "&force=1"
	}
	fmt.Printf("pushing %s (%d MiB) to %s; waiting for health check...\n", filepath.Base(src), fi.Size()>>20, d.URL)
	resp, err := d.do(ctx, http.MethodPut, "/v1/image"+q, f, 30*time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := d.readVerified(resp)
	if err != nil {
		return err
	}
	var res agent.PushResult
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("device returned %s", resp.Status)
	}
	if !res.OK {
		if res.RolledBack {
			fmt.Println("ROLLED BACK: the new image failed its health check; see `ramekin logs`")
		}
		return errors.New(res.Error)
	}
	fmt.Printf("OK: %s is running %s\n", pos[0], res.State.Current.Version)
	return nil
}

func cmdLogs(ctx context.Context, args []string) error {
	fl := flags("logs")
	follow := fl.Bool("f", false, "follow")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin logs <device> [-f]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, pos[0])
	if err != nil {
		return err
	}
	path := "/v1/logs"
	if *follow {
		path += "?follow=1"
	}
	resp, err := d.do(ctx, http.MethodGet, path, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func cmdStatus(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ramekin status <device>")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, args[0])
	if err != nil {
		return err
	}
	_, b, err := d.status(ctx, time.Minute)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func cmdRestart(ctx context.Context, args []string) error {
	return simpleDeviceCall(ctx, args, "restart", http.MethodPost, "/v1/restart")
}

func simpleDeviceCall(ctx context.Context, args []string, name, method, path string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ramekin %s <device>", name)
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, args[0])
	if err != nil {
		return err
	}
	resp, err := d.do(ctx, method, path, nil, time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode > 299 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// mergeGokrazy adds ramekind and its device.json to an existing gokrazy
// instance config, leaving all other settings untouched.
func mergeGokrazy(cfgPath string, devJSON []byte, arch string) error {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	if envs, ok := cfg["Environment"].([]any); ok {
		for _, e := range envs {
			if s, _ := e.(string); strings.HasPrefix(s, "GOARCH=") && s != "GOARCH="+arch {
				return fmt.Errorf("%s builds %s but ramekin.yaml platform is linux/%s", cfgPath, s, arch)
			}
		}
	}
	removed, added, err := enforcePackages(cfg, false)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	pc, _ := cfg["PackageConfig"].(map[string]any)
	if pc == nil {
		pc = map[string]any{}
	}
	if existing, ok := pc[ramekindPkg].(map[string]any); ok {
		if efc, ok := existing["ExtraFileContents"].(map[string]any); ok && efc[config.DefaultDevicePath] != nil {
			return fmt.Errorf("%s already configures ramekind; edit it there or remove that entry first", cfgPath)
		}
	}
	pc[ramekindPkg] = map[string]any{
		"ExtraFileContents": map[string]string{config.DefaultDevicePath: string(devJSON)},
	}
	cfg["PackageConfig"] = pc
	upd, _ := cfg["Update"].(map[string]any)
	if upd == nil {
		upd = map[string]any{}
	}
	switch port, _ := upd["HTTPPort"].(string); port {
	case "", "80":
		upd["HTTPPort"] = dashboardPort
	case dashboardPort:
	default:
		fmt.Fprintf(os.Stderr, "keeping the instance's custom dashboard port %s (Ramekin's default is %s)\n", port, dashboardPort)
	}
	cfg["Update"] = upd
	out, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath+".bak", b, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, append(out, '\n'), 0o600); err != nil {
		return err
	}
	reportPackages(cfgPath, removed, added, false)
	return nil
}

// deviceUpdate runs `gok update` for a gokrazy instance. If the instance is
// configured for a dashboard port the device does not serve yet (the device
// still runs an image whose dashboard is on port 80), it bridges this one
// update: gok connects to localhost:<port>, which is forwarded to the
// device's port 80 until the device reboots onto the new port.
func deviceUpdate(ctx context.Context, args []string) error {
	fl := flags("device update")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device update <name> [--gokrazy-dir dir]")
	}
	name := pos[0]
	*parent = gokrazyDir(*parent)
	cfgPath := filepath.Join(*parent, name, "config.json")
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(orig, &cfg); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	removed, added, err := enforcePackages(cfg, false)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	if len(removed)+len(added) > 0 {
		// Save the corrected config permanently (and back up the old one).
		fixed, err := json.MarshalIndent(cfg, "", "    ")
		if err != nil {
			return err
		}
		fixed = append(fixed, '\n')
		if err := os.WriteFile(cfgPath+".bak", orig, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, fixed, 0o600); err != nil {
			return err
		}
		reportPackages(cfgPath, removed, added, false)
		orig = fixed
	}
	upd, _ := cfg["Update"].(map[string]any)
	if upd == nil {
		upd = map[string]any{}
	}
	port, _ := upd["HTTPPort"].(string)
	host, _ := upd["Hostname"].(string)
	if host == "" {
		host, _ = cfg["Hostname"].(string)
	}

	dash := port
	if dash == "" {
		dash = "80"
	}
	// override, if set, temporarily replaces Update.Hostname for this run.
	override := ""
	if !reachable(host, dash) && !reachable(host, "80") {
		if ip := relocate(ctx, name); ip != "" {
			fmt.Printf("%s is now at %s; updating it there (update %s in /etc/hosts or DNS to make this permanent)\n", name, ip, host)
			host, override = ip, ip
		}
	}

	if dash != "80" && !reachable(host, dash) && reachable(host, "80") {
		fmt.Printf("%s still serves its dashboard on port 80; bridging this update to move it to port %s\n", host, dash)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", dash))
		if err != nil {
			return fmt.Errorf("bridge needs local port %s: %w", dash, err)
		}
		defer ln.Close()
		go bridge(ln, host, []string{"80", dash})
		override = "localhost"
	}

	if override != "" {
		upd["Hostname"] = override
		cfg["Update"] = upd
		tmp, _ := json.MarshalIndent(cfg, "", "    ")
		if err := os.WriteFile(cfgPath, append(tmp, '\n'), 0o600); err != nil {
			return err
		}
		// Always put the real config back, including on Ctrl-C or gok failure.
		defer os.WriteFile(cfgPath, orig, 0o600)
	}

	cmd := exec.CommandContext(ctx, "gok", "--parent_dir", *parent, "-i", name, "update")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gok update: %w", err)
	}
	return nil
}

func reachable(host, port string) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// bridge forwards each connection to the first of ports that accepts one, so
// gok's post-reboot checks reach the device once it serves the new port.
func bridge(ln net.Listener, host string, ports []string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var up net.Conn
			for _, p := range ports {
				if u, err := net.DialTimeout("tcp", net.JoinHostPort(host, p), 5*time.Second); err == nil {
					up = u
					break
				}
			}
			if up == nil {
				return
			}
			defer up.Close()
			go io.Copy(up, c)
			io.Copy(c, up)
		}()
	}
}

// deviceFind locates a device by its remembered MAC and updates its record.
func deviceFind(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ramekin device find <name>")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, args[0])
	if err != nil {
		return err
	}
	if err := d.locate(ctx); err != nil {
		return err
	}
	host, _, _ := d.hostPort()
	mac := d.MAC
	if mac == "" {
		mac = "unknown"
	}
	fmt.Printf("%s: %s (MAC %s)\n", args[0], d.URL, mac)
	if ips, err := net.LookupHost(host); err == nil && len(ips) > 0 && ips[0] != host {
		fmt.Printf("  %s resolves to %s\n", host, ips[0])
	}
	return nil
}

// relocate finds a device by the MAC in the current project's device record,
// for commands (like device update) that address it through gokrazy.
func relocate(ctx context.Context, name string) string {
	p, err := loadProject()
	if err != nil {
		return ""
	}
	d, err := resolveDevice(p, name)
	if err != nil || d.MAC == "" {
		return ""
	}
	if err := d.locate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ""
	}
	host, _, _ := d.hostPort()
	return host
}
