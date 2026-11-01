package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
)

// Firmware is the gokrazy OS image itself (kernel, gokrazy and ramekind),
// built from the project's fleet instance: one image for every device of a
// kind, since each device keeps its identity on /perm.

func cmdFirmware(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ramekin firmware init|publish|push ...")
	}
	official := config.HasFirmwareProject(".")
	switch args[0] {
	case "init":
		for _, a := range args[1:] {
			if a == "--board" || strings.HasPrefix(a, "--board=") || a == "-board" {
				return officialInit(ctx, args[1:])
			}
		}
		return firmwareInit(args[1:])
	case "publish":
		if official {
			return officialPublish(ctx, args[1:])
		}
		return firmwarePublish(ctx, args[1:])
	case "push":
		return firmwarePush(ctx, args[1:])
	}
	return fmt.Errorf("unknown firmware subcommand %q", args[0])
}

func fleetInstance(p *config.Project) string { return p.Name + "-fleet" }

// firmwareInit creates the fleet instance: a gokrazy instance whose image
// holds no per-device secrets.
func firmwareInit(args []string) error {
	fl := flags("firmware init")
	from := fl.String("from", "", "copy hardware settings (kernel, firmware, packages, build directory) from this gokrazy instance")
	target := fl.String("target", "rpi", "hardware when not using --from: rpi (Raspberry Pi 3/4/5, arm64) or amd64 (PCs, VMs)")
	channel := fl.String("channel", "stable", "release channel devices follow, for apps and firmware")
	healthHTTP := fl.String("health-http", "", "URL that must return 2xx for a new release to be accepted")
	healthTCP := fl.String("health-tcp", "", "host:port that must accept connections for a new release to be accepted")
	healthExec := fl.String("health-exec", "", "command run inside the container that must exit 0 for a new release to be accepted")
	lockscreen := fl.Bool("lockscreen", false, "also run gk-lockscreen ("+lockscreenPkg+")")
	statusURL := fl.String("status-url", "", "URL devices POST a signed JSON status report to after each update check (optional)")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	set := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { set[f.Name] = true })
	p, err := loadProject()
	if err != nil {
		return err
	}
	if p.Releases.GitHub == "" {
		return errors.New("releases.github must be set in ramekin.yaml: fleet devices get apps and firmware from it")
	}
	execArgs, err := splitArgs(*healthExec)
	if err != nil {
		return fmt.Errorf("--health-exec: %w", err)
	}
	pub, err := os.ReadFile(p.Path(p.Signing.PubKey))
	if err != nil {
		return fmt.Errorf("%w (run `ramekin keys generate` first)", err)
	}
	// No name, API token or age key: each device gets those when claimed.
	dev := config.Device{
		SigningPubKey: string(pub),
		Update:        config.Update{Enabled: true, Channel: *channel, URL: p.ChannelURL(), StatusURL: *statusURL},
		Firmware:      config.Firmware{URL: p.FirmwareChannelURL()},
		Container:     config.Container{Hostname: p.Name},
		Health:        config.Health{HTTP: *healthHTTP, TCP: *healthTCP, Exec: execArgs},
	}
	if p.Firmware.GitHub != "" {
		// The project's devices run official firmware: this image follows
		// its channel and trusts its key, so devices move over to it.
		key, err := fetchFirmwareKey(p.Firmware)
		if err != nil {
			return err
		}
		dev.Firmware = config.Firmware{URL: p.Firmware.ChannelURL(), SigningPubKey: key}
	}
	*parent = gokrazyDir(*parent)
	var cfg map[string]any
	if *from != "" {
		b, err := os.ReadFile(filepath.Join(*parent, *from, "config.json"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return err
		}
		// Keep the source device's app settings (health checks, container,
		// channel), unless overridden by flags; drop its identity.
		if files, err := deviceFiles(cfg); err == nil {
			var src config.Device
			if json.Unmarshal([]byte(files[config.DefaultDevicePath].(string)), &src) == nil {
				dev.Container = src.Container
				if !set["status-url"] {
					dev.Update.StatusURL = src.Update.StatusURL
				}
				dev.Container.Hostname = p.Name
				if !set["channel"] && src.Update.Channel != "" {
					dev.Update.Channel = src.Update.Channel
				}
				if !set["health-http"] && !set["health-tcp"] && !set["health-exec"] {
					dev.Health = src.Health
				}
			}
		}
	}
	devJSON, _ := json.MarshalIndent(dev, "", "  ")
	if _, err := config.ParseDevice(devJSON); err != nil {
		return fmt.Errorf("generated device config is invalid: %w", err)
	}

	name := fleetInstance(p)
	cfgPath, err := writeInstance(*parent, name, *from, *target, *lockscreen, cfg, devJSON, p.Name, false)
	if err != nil {
		return err
	}
	fmt.Printf(`fleet image: %s
Every device flashed from it starts unclaimed. Flash one:
  gok --parent_dir %s -i %s overwrite --full /dev/sdX
then, once it's on the network, claim it with the pairing code on its console:
  ramekin device claim <name> <its-ip>
Publish OS updates for the whole fleet with:
  ramekin firmware publish --version 1
`, cfgPath, *parent, name)
	return nil
}

// writeInstance creates a gokrazy instance for a fleet or official image:
// from the instance named from (cfg is its config, copied with its build
// directory), or from scratch for target. Its devices share hostname until
// they're claimed. noPassword leaves gokrazy without a network password, so
// its web interface is reachable only from the device itself: an image
// anyone can download mustn't carry a password.
func writeInstance(parent, name, from, target string, lockscreen bool, cfg map[string]any, devJSON []byte, hostname string, noPassword bool) (string, error) {
	instDir := filepath.Join(parent, name)
	if _, err := os.Stat(instDir); err == nil {
		return "", fmt.Errorf("%s already exists; change its settings with `ramekin device set %s ...`", instDir, name)
	}
	if from != "" {
		// The build directory pins package versions (and any local replaces).
		if _, err := os.Stat(filepath.Join(parent, from, "builddir")); err == nil {
			if err := os.MkdirAll(instDir, 0o700); err != nil {
				return "", err
			}
			if out, err := exec.Command("cp", "-a", filepath.Join(parent, from, "builddir"), instDir).CombinedOutput(); err != nil {
				return "", fmt.Errorf("copy build directory: %v: %s", err, out)
			}
		}
		if err := setDeviceJSON(cfg, devJSON); err != nil {
			return "", err
		}
	} else {
		arch, kernel, fw, eeprom := "arm64", "github.com/gokrazy/kernel.rpi", "github.com/gokrazy/firmware", "github.com/gokrazy/rpi-eeprom"
		switch target {
		case "rpi":
		case "amd64":
			arch, kernel, fw, eeprom = "amd64", "github.com/gokrazy/kernel.amd64", "", ""
		default:
			return "", fmt.Errorf("unknown --target %q", target)
		}
		packages := []any{ramekindPkg}
		if lockscreen {
			packages = append(packages, lockscreenPkg)
		}
		cfg = map[string]any{
			"Environment": []any{"GOOS=linux", "GOARCH=" + arch},
			"Packages":    packages,
			"PackageConfig": map[string]any{
				ramekindPkg: map[string]any{"ExtraFileContents": map[string]any{config.DefaultDevicePath: string(devJSON)}},
			},
			"SerialConsole":   "disabled",
			"KernelPackage":   kernel,
			"FirmwarePackage": fw,
			"EEPROMPackage":   eeprom,
		}
	}
	cfg["Hostname"] = hostname
	update, _ := cfg["Update"].(map[string]any)
	if update == nil {
		update = map[string]any{}
	}
	delete(update, "Hostname")
	update["HTTPPort"] = dashboardPort
	if noPassword {
		delete(update, "HTTPPassword")
		update["NoPassword"] = true
	} else {
		// Shared by every device until it's claimed, when each gets its own
		// (on /perm, which gokrazy reads first).
		update["HTTPPassword"] = randomHex(16)
	}
	cfg["Update"] = update
	if lockscreen && from != "" {
		pk, _ := cfg["Packages"].([]any)
		cfg["Packages"] = append(pk, lockscreenPkg)
	}
	removed, added, err := enforcePackages(cfg, true)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(instDir, 0o700); err != nil {
		return "", err
	}
	out, _ := json.MarshalIndent(cfg, "", "    ")
	cfgPath := filepath.Join(instDir, "config.json")
	if err := os.WriteFile(cfgPath, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	reportPackages(cfgPath, removed, added, true)
	return cfgPath, nil
}

func setDeviceJSON(cfg map[string]any, devJSON []byte) error {
	pc, _ := cfg["PackageConfig"].(map[string]any)
	if pc == nil {
		pc = map[string]any{}
		cfg["PackageConfig"] = pc
	}
	pkg, _ := pc[ramekindPkg].(map[string]any)
	if pkg == nil {
		pkg = map[string]any{}
		pc[ramekindPkg] = pkg
	}
	files, _ := pkg["ExtraFileContents"].(map[string]any)
	if files == nil {
		files = map[string]any{}
		pkg["ExtraFileContents"] = files
	}
	files[config.DefaultDevicePath] = string(devJSON)
	return nil
}

// buildFirmware builds the project's fleet image (see buildImage).
func buildFirmware(ctx context.Context, p *config.Project, parent, version, dir string) (boot, root, arch string, err error) {
	url, key := p.FirmwareChannelURL(), ""
	if p.Firmware.GitHub != "" {
		// Hand devices over to official firmware: follow its channel, trust its key.
		if key, err = fetchFirmwareKey(p.Firmware); err != nil {
			return "", "", "", err
		}
		url = p.Firmware.ChannelURL()
	}
	return buildImage(ctx, parent, fleetInstance(p), version, url, key, dir)
}

// buildImage stamps version, the firmware channel URL and (if set) the
// firmware signing key into a fleet or
// official instance's device.json and builds its boot and root partition
// images into dir.
func buildImage(ctx context.Context, parent, name, version, url, key, dir string) (boot, root, arch string, err error) {
	cfgPath := filepath.Join(parent, name, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w (create the fleet image with `ramekin firmware init`)", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", "", err
	}
	files, err := deviceFiles(cfg)
	if err != nil {
		return "", "", "", err
	}
	var dev config.Device
	if err := json.Unmarshal([]byte(files[config.DefaultDevicePath].(string)), &dev); err != nil {
		return "", "", "", err
	}
	if !dev.Fleet() {
		return "", "", "", fmt.Errorf("%s holds a device's secrets; firmware is built from a fleet image (ramekin firmware init)", cfgPath)
	}
	// Development tools may have been added to the fleet instance by hand.
	removed, added, err := enforcePackages(cfg, true)
	if err != nil {
		return "", "", "", err
	}
	reportPackages(cfgPath, removed, added, true)
	dev.Firmware.Version = version
	dev.Firmware.URL = url // follows ramekin.yaml or firmware.yaml
	if key != "" {
		dev.Firmware.SigningPubKey = key
	}
	devJSON, _ := json.MarshalIndent(dev, "", "  ")
	files[config.DefaultDevicePath] = string(devJSON)
	out, _ := json.MarshalIndent(cfg, "", "    ")
	if err := os.WriteFile(cfgPath, append(out, '\n'), 0o600); err != nil {
		return "", "", "", err
	}
	arch = "arm64"
	env, _ := cfg["Environment"].([]any)
	for _, e := range env {
		if v, ok := strings.CutPrefix(fmt.Sprint(e), "GOARCH="); ok {
			arch = v
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	boot, root = filepath.Join(dir, "boot.img"), filepath.Join(dir, "root.img")
	fmt.Printf("building firmware %s from %s\n", version, name)
	cmd := exec.CommandContext(ctx, "gok", "--parent_dir", parent, "-i", name, "overwrite", "--boot", boot, "--root", root)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", "", "", fmt.Errorf("gok overwrite: %w", err)
	}
	return boot, root, arch, nil
}

func firmwarePublish(ctx context.Context, args []string) error {
	fl := flags("firmware publish")
	version := fl.String("version", "", "firmware version (required, immutable once published)")
	channel := fl.String("channel", "", "channel to point at the firmware (default: first in ramekin.yaml)")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	if *version == "" || strings.ContainsAny(*version, "/ ") {
		return errors.New("--version is required and must not contain '/' or spaces")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if p.Firmware.GitHub != "" {
		return fmt.Errorf("this project's devices run official firmware from %s; publish firmware from that repository's firmware project", p.Firmware.GitHub)
	}
	if *channel == "" {
		*channel = p.Channels[0]
	}
	signer, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	recipients, err := keys.ParseRecipients(p.Encrypt.Recipients, p.Path(p.Encrypt.RecipientsFile))
	if err != nil {
		return err
	}
	gh, err := githubClient(p, true)
	if err != nil {
		return err
	}
	gh.Repo = p.FirmwareRepo()
	if err := gh.CheckRepo(ctx); err != nil {
		return err
	}
	fwName := p.Name + "-firmware"
	tag := release.Tag(fwName, *version)
	if _, err := gh.GetRelease(ctx, tag); err == nil {
		return fmt.Errorf("firmware %s is already published (release %s); pick a new version", *version, tag)
	} else if !errors.Is(err, release.ErrNotFound) {
		return err
	}
	outDir := p.Path(filepath.Join("build", "firmware", *version))
	boot, root, arch, err := buildFirmware(ctx, p, gokrazyDir(*parent), *version, outDir)
	if err != nil {
		return err
	}
	// The image holds the fleet's gokrazy password, so it's encrypted like
	// app releases: to the fleet key and every claimed device.
	m := &manifest.Manifest{Name: fwName, Version: *version, Arch: arch, Kind: "firmware", Published: time.Now().UTC()}
	var assets []string
	for _, part := range []struct{ name, file string }{{"boot", boot}, {"root", root}} {
		sum, size, err := hashFile(part.file)
		if err != nil {
			return err
		}
		asset := part.name + ".img.age"
		enc := filepath.Join(outDir, asset)
		if err := encryptFile(part.file, enc, recipients); err != nil {
			return err
		}
		assets = append(assets, enc)
		m.Parts = append(m.Parts, manifest.Part{Name: part.name, Object: gh.AssetURL(tag, asset), SHA256: sum, Size: size})
	}
	fmt.Printf("encrypted for %d recipient(s)\n", len(recipients))
	env, err := manifest.Sign(m, signer)
	if err != nil {
		return err
	}
	manifestFile := filepath.Join(outDir, assetManifest)
	if err := os.WriteFile(manifestFile, env, 0o644); err != nil {
		return err
	}
	notes := fmt.Sprintf("Ramekin firmware %s for %s devices (linux/%s): the gokrazy OS image, kernel and ramekind.\n\nThe images are signed and encrypted. Devices install them with a test boot and keep them only if they come up healthy; the kernel has no second copy, so try new kernels on a test device first.", *version, p.Name, arch)
	draft, err := gh.CreateDraft(ctx, tag, fwName+" "+*version, notes)
	if err != nil {
		return err
	}
	for _, f := range append(assets, manifestFile) {
		ctype := "application/octet-stream"
		if strings.HasSuffix(f, ".json") {
			ctype = "application/json"
		}
		if err := gh.UploadAsset(ctx, draft, filepath.Base(f), f, ctype); err != nil {
			return fmt.Errorf("upload %s: %w", filepath.Base(f), err)
		}
	}
	pub, err := gh.Publish(ctx, draft)
	if err != nil {
		return err
	}
	fmt.Printf("published %s\n", pub.HTMLURL)
	return pointChannel(ctx, gh, signer, m, fwName, *channel)
}

// firmwarePush builds the fleet image and installs it on one device over
// the local network, for trying firmware before publishing it. The device
// must be claimed: the fleet image carries no identity of its own. With
// --firmware-dir it builds official firmware instead, signed with that
// firmware project's key, for trying it on a device before publishing it to
// every device of the board.
func firmwarePush(ctx context.Context, args []string) error {
	fl := flags("firmware push")
	version := fl.String("version", "", "version to stamp the image with (default dev-<time>)")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	fwDir := fl.String("firmware-dir", "", "push official firmware built from this firmware project (its firmware.yaml and signing key)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin firmware push <device> [--version V] [--firmware-dir DIR]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	d, err := resolveDevice(p, pos[0])
	if err != nil {
		return err
	}
	if d.PublicKey == "" {
		return fmt.Errorf("%s isn't claimed; claim it first (ramekin device claim %s), so it keeps its identity on the fleet image", pos[0], pos[0])
	}
	if *fwDir == "" && p.Firmware.GitHub != "" {
		return fmt.Errorf("this project's devices run official firmware (%s), which only accepts images signed with its key: pass --firmware-dir with a checkout of that firmware project", p.Firmware.GitHub)
	}
	if *version == "" {
		*version = "dev-" + time.Now().Format("20060102-150405")
	}
	var (
		signer           signature.Signer
		boot, root, arch string
		m                *manifest.Manifest
	)
	if *fwDir != "" {
		fp, err := config.LoadFirmwareProject(*fwDir)
		if err != nil {
			return err
		}
		if signer, err = keys.LoadSigner(fp.Path(fp.Signing.Key)); err != nil {
			return err
		}
		outDir := fp.Path(filepath.Join("build", *version))
		if boot, root, arch, err = buildImage(ctx, gokrazyDir(*parent), fp.Instance, *version, fp.Ref().ChannelURL(), "", outDir); err != nil {
			return err
		}
		m = &manifest.Manifest{Name: fp.Board, Version: *version, Arch: arch, Kind: "firmware", Plain: true, Published: time.Now().UTC()}
	} else {
		if signer, err = keys.LoadSigner(p.Path(p.Signing.Key)); err != nil {
			return err
		}
		outDir := p.Path(filepath.Join("build", "firmware", *version))
		if boot, root, arch, err = buildFirmware(ctx, p, gokrazyDir(*parent), *version, outDir); err != nil {
			return err
		}
		m = &manifest.Manifest{Name: p.Name + "-firmware", Version: *version, Arch: arch, Kind: "firmware", Published: time.Now().UTC()}
	}
	for _, part := range []struct{ name, file string }{{"boot", boot}, {"root", root}} {
		sum, size, err := hashFile(part.file)
		if err != nil {
			return err
		}
		m.Parts = append(m.Parts, manifest.Part{Name: part.name, SHA256: sum, Size: size})
	}
	env, err := manifest.Sign(m, signer)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := addTar(tw, "manifest.json", env)
		for _, f := range []struct{ name, path string }{{"boot.img", boot}, {"root.img", root}} {
			if err == nil {
				err = addTarFile(tw, f.name, f.path)
			}
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	fmt.Printf("pushing firmware %s to %s; it writes the image and reboots for a test boot...\n", *version, d.URL)
	resp, err := d.do(ctx, http.MethodPut, "/v1/firmware", pr, 30*time.Minute)
	if err != nil {
		return err
	}
	b, err := d.readVerified(resp)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return waitForFirmware(ctx, &d, *version)
}

// waitForFirmware follows a device through its test boot.
func waitForFirmware(ctx context.Context, d *deviceRecord, version string) error {
	deadline := time.Now().Add(15 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		st, _, err := d.status(ctx, 10*time.Second)
		msg := ""
		switch {
		case err != nil:
			msg = "waiting for the device to come back"
		case st.Firmware == nil:
			msg = "the device is running firmware without firmware support"
		case st.Firmware.Failed[version] != "":
			return fmt.Errorf("firmware %s %s; the device is back on %s", version, st.Firmware.Failed[version], st.Firmware.Version)
		case st.Firmware.Version == version && st.Firmware.Pending == "":
			fmt.Printf("OK: %s is running firmware %s\n", d.name, version)
			return nil
		case st.Firmware.Version == version:
			msg = "test boot: checking that the device is healthy"
		default:
			msg = "still on " + st.Firmware.Version
		}
		if msg != last {
			fmt.Println(msg + "...")
			last = msg
		}
	}
	return fmt.Errorf("%s didn't report firmware %s within 15 minutes", d.name, version)
}

func addTar(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func addTarFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: fi.Size()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
