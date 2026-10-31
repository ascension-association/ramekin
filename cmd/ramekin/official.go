package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
)

// Official firmware is a gokrazy image for one board with ramekind and no
// project, signed with the firmware project's own key and published
// unencrypted, so anyone can flash it. Devices learn their project when
// they're claimed, and keep following the board's firmware channel.

// officialInit sets up a firmware project in the current directory (the
// firmware repository's checkout) and its gokrazy instance.
func officialInit(ctx context.Context, args []string) error {
	fl := flags("firmware init")
	board := fl.String("board", "", "board name, e.g. r76s (names releases and channels)")
	github := fl.String("github", "", "repository the firmware is published to, as owner/name")
	from := fl.String("from", "", "copy hardware settings (kernel, firmware, packages, build directory) from this gokrazy instance")
	target := fl.String("target", "rpi", "hardware when not using --from: rpi (Raspberry Pi 3/4/5, arm64) or amd64 (PCs, VMs)")
	lockscreen := fl.Bool("lockscreen", false, "also run gk-lockscreen ("+lockscreenPkg+")")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	if *board == "" || *github == "" {
		return errors.New("usage: ramekin firmware init --board <name> --github owner/repo [--from <gokrazy instance> | --target rpi|amd64]")
	}
	if config.HasFirmwareProject(".") {
		return fmt.Errorf("%s already exists here", config.FirmwareFile)
	}
	if _, err := os.Stat(config.ProjectFile); err == nil {
		return errors.New("this is an app project; create official firmware in its own directory (a checkout of the firmware repository)")
	}
	yml := fmt.Sprintf(`# Official Ramekin firmware for one board: gokrazy, the kernel and ramekind,
# with no project. Anyone can flash it; devices learn their project when
# they're claimed (ramekin device claim) and follow this firmware's channel.
board: %s
releases:
  github: %s
  branch: main
signing:
  key: keys/firmware-sign.key       # secret: never commit it
  pubkey: %s   # devices and projects trust this key
channels: [stable]
`, *board, *github, config.FirmwareKeyPub)
	if err := os.WriteFile(config.FirmwareFile, []byte(yml), 0o644); err != nil {
		return err
	}
	fp, err := config.LoadFirmwareProject(".")
	if err != nil {
		return err
	}
	if err := keys.GenerateSigning(fp.Path(fp.Signing.Key), fp.Path(fp.Signing.PubKey)); err != nil {
		return err
	}
	if err := appendLine(".gitignore", "/build/\n/keys/*.key\n"); err != nil {
		return err
	}
	pub, err := os.ReadFile(fp.Path(fp.Signing.PubKey))
	if err != nil {
		return err
	}
	// No project: no signing key for apps, no app updates, no settings.
	dev := config.Device{Firmware: config.Firmware{URL: fp.Ref().ChannelURL(), SigningPubKey: string(pub)}}
	devJSON, _ := json.MarshalIndent(dev, "", "  ")
	if _, err := config.ParseDevice(devJSON); err != nil {
		return fmt.Errorf("generated device config is invalid: %w", err)
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
	}
	cfgPath, err := writeInstance(*parent, fp.Instance, *from, *target, *lockscreen, cfg, devJSON, *board, true)
	if err != nil {
		return err
	}
	fmt.Printf(`firmware project for %s: %s (commit it, with %s)
signing key:   %s (secret; keep it out of git)
gokrazy image: %s (no gokrazy password: its web interface is local to the device)

Publish firmware (unencrypted, signed with the firmware key):
  ramekin firmware publish --version 1.0.0
Anyone can then flash it:
  gok --parent_dir %s -i %s overwrite --full /dev/sdX
and point their project at it in ramekin.yaml:
  firmware: {github: %s, board: %s}
`, *board, config.FirmwareFile, fp.Signing.PubKey, fp.Signing.Key, cfgPath, *parent, fp.Instance, *github, *board)
	return nil
}

// officialPublish builds the board's image and publishes it as a release of
// the firmware repository, then points a channel at it.
func officialPublish(ctx context.Context, args []string) error {
	fl := flags("firmware publish")
	version := fl.String("version", "", "firmware version (required, immutable once published)")
	channel := fl.String("channel", "", "channel to point at the firmware (default: first in firmware.yaml)")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	if *version == "" || strings.ContainsAny(*version, "/ ") {
		return errors.New("--version is required and must not contain '/' or spaces")
	}
	fp, err := config.LoadFirmwareProject(".")
	if err != nil {
		return err
	}
	if *channel == "" {
		*channel = fp.Channels[0]
	}
	signer, err := keys.LoadSigner(fp.Path(fp.Signing.Key))
	if err != nil {
		return err
	}
	gh, err := githubClient(&config.Project{Releases: config.Releases{GitHub: fp.Releases.GitHub, Branch: fp.Releases.Branch, TokenEnv: fp.Releases.TokenEnv}}, true)
	if err != nil {
		return err
	}
	if err := gh.CheckRepo(ctx); err != nil {
		return err
	}
	tag := release.Tag(fp.Board, *version)
	if _, err := gh.GetRelease(ctx, tag); err == nil {
		return fmt.Errorf("firmware %s is already published (release %s); pick a new version", *version, tag)
	} else if !errors.Is(err, release.ErrNotFound) {
		return err
	}
	outDir := fp.Path(filepath.Join("build", *version))
	boot, root, arch, err := buildImage(ctx, gokrazyDir(*parent), fp.Instance, *version, fp.Ref().ChannelURL(), "", outDir)
	if err != nil {
		return err
	}
	m := &manifest.Manifest{Name: fp.Board, Version: *version, Arch: arch, Kind: "firmware", Plain: true, Published: time.Now().UTC()}
	for _, part := range []struct{ name, file string }{{"boot", boot}, {"root", root}} {
		sum, size, err := hashFile(part.file)
		if err != nil {
			return err
		}
		m.Parts = append(m.Parts, manifest.Part{Name: part.name, Object: gh.AssetURL(tag, filepath.Base(part.file)), SHA256: sum, Size: size})
	}
	env, err := manifest.Sign(m, signer)
	if err != nil {
		return err
	}
	manifestFile := filepath.Join(outDir, assetManifest)
	if err := os.WriteFile(manifestFile, env, 0o644); err != nil {
		return err
	}
	notes := fmt.Sprintf("Official Ramekin firmware %s for %s (linux/%s): gokrazy, the kernel and ramekind, with no project.\n\nFlash boot.img and root.img, or install over the air on a claimed device. The images are signed with the key in %s; devices keep a new image only if it boots healthy, but the kernel has no second copy, so test new kernels first.", *version, fp.Board, arch, config.FirmwareKeyPub)
	draft, err := gh.CreateDraft(ctx, tag, fp.Board+" "+*version, notes)
	if err != nil {
		return err
	}
	for _, f := range []string{boot, root, manifestFile} {
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
	return pointChannel(ctx, gh, signer, m, fp.Board, *channel)
}

// fetchFirmwareKey downloads official firmware's public signing key from its
// repository.
func fetchFirmwareKey(o config.OfficialFirmware) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := release.FetchBytes(ctx, o.KeyURL(), 16<<10)
	if err != nil {
		return "", fmt.Errorf("firmware signing key from %s: %w", o.KeyURL(), err)
	}
	if _, err := keys.LoadVerifier(b); err != nil {
		return "", fmt.Errorf("firmware signing key from %s: %w", o.KeyURL(), err)
	}
	return string(b), nil
}

func appendLine(path, text string) error {
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), text) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}
