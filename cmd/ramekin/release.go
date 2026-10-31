package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/release"
	"github.com/ascension-association/ramekin/internal/scan"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

const (
	assetImage    = ".sif.age" // suffix after the app name
	assetSBOM     = "sbom.json"
	assetManifest = "manifest.json"
)

// githubClient returns the release client. Publishing needs a token;
// read-only commands work without one on public repositories.
func githubClient(p *config.Project, needToken bool) (*release.GitHub, error) {
	if p.Releases.GitHub == "" {
		return nil, errors.New("set releases.github (owner/repo) in ramekin.yaml")
	}
	token := firstNonEmpty(env(p.Releases.TokenEnv), env("GH_TOKEN"))
	if token == "" {
		if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	if token == "" && needToken {
		return nil, fmt.Errorf("a GitHub token is needed to publish: set %s, or log in with `gh auth login`", p.Releases.TokenEnv)
	}
	return &release.GitHub{Repo: p.Releases.GitHub, Branch: p.Releases.Branch, Token: token}, nil
}

func cmdPublish(ctx context.Context, args []string) error {
	fl := flags("publish")
	version := fl.String("version", "", "release version (required, immutable once published)")
	channel := fl.String("channel", "", "channel to point at the release (default: first in ramekin.yaml)")
	allowVulns := fl.Bool("allow-vulns", false, "publish even if findings reach scan.fail_on")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if *version == "" || strings.ContainsAny(*version, "/ ") {
		return errors.New("--version is required and must not contain '/' or spaces")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if *channel == "" {
		*channel = p.Channels[0]
	}
	if !slices.Contains(p.Channels, *channel) {
		return fmt.Errorf("channel %q is not listed in ramekin.yaml channels", *channel)
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
	if err := gh.CheckRepo(ctx); err != nil {
		return err
	}
	tag := release.Tag(p.Name, *version)
	if _, err := gh.GetRelease(ctx, tag); err == nil {
		return fmt.Errorf("version %s is already published (release %s); releases are immutable, pick a new version", *version, tag)
	} else if !errors.Is(err, release.ErrNotFound) {
		return err
	}

	src := sifArg(p, pos, 0)
	outDir := p.Path(filepath.Join("build", "release", *version))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	signed := filepath.Join(outDir, p.Name+".sif")
	if err := copyFile(src, signed); err != nil {
		return err
	}

	// 1. Scan: the release gate.
	sbom := filepath.Join(outDir, assetSBOM)
	rep, err := scanSIF(ctx, p, signed, sbom)
	if err != nil {
		return err
	}
	if err := applyPolicy(ctx, p, rep, fmt.Sprintf("%s %s (publish)", p.Name, *version), !*allowVulns); err != nil {
		return err
	}

	// 2. Sign.
	if err := sifimg.Sign(ctx, signed, signer); err != nil {
		return err
	}
	sum, size, err := hashFile(signed)
	if err != nil {
		return err
	}

	// 3. Encrypt.
	imageName := p.Name + assetImage
	enc := filepath.Join(outDir, imageName)
	if err := encryptFile(signed, enc, recipients); err != nil {
		return err
	}
	fmt.Printf("signed and encrypted for %d recipient(s): %s\n", len(recipients), enc)

	// 4. Release: create a draft, upload, publish. (Immutable releases only
	// accept assets while they are drafts.) Asset URLs are stable, so the
	// manifest can be signed before the release exists.
	m := &manifest.Manifest{
		Name: p.Name, Version: *version, Arch: p.Arch(),
		Object: gh.AssetURL(tag, imageName), SHA256: sum, Size: size,
		SBOM:      gh.AssetURL(tag, assetSBOM),
		Published: time.Now().UTC(),
		Vulns:     rep.Counts(),
	}
	env, err := manifest.Sign(m, signer)
	if err != nil {
		return err
	}
	manifestFile := filepath.Join(outDir, assetManifest)
	if err := os.WriteFile(manifestFile, env, 0o644); err != nil {
		return err
	}
	c := rep.Counts()
	notes := fmt.Sprintf("Ramekin release of %s %s for linux/%s.\n\nVulnerabilities at publish time: critical=%d high=%d medium=%d low=%d.\n\nThe image is signed and encrypted; devices verify it before installing.",
		p.Name, *version, p.Arch(), c["critical"], c["high"], c["medium"], c["low"])
	draft, err := gh.CreateDraft(ctx, tag, p.Name+" "+*version, notes)
	if err != nil {
		return err
	}
	for _, a := range []struct{ name, file, ctype string }{
		{imageName, enc, "application/octet-stream"},
		{assetSBOM, sbom, "application/json"},
		{assetManifest, manifestFile, "application/json"},
	} {
		if err := gh.UploadAsset(ctx, draft, a.name, a.file, a.ctype); err != nil {
			return fmt.Errorf("upload %s: %w", a.name, err)
		}
	}
	pub, err := gh.Publish(ctx, draft)
	if err != nil {
		return err
	}
	if pub.Immutable {
		fmt.Printf("published %s (immutable)\n", pub.HTMLURL)
	} else {
		fmt.Printf("published %s\n  note: immutable releases are off for %s, so this release's assets can still be replaced or deleted.\n  Turn them on under Settings > General > Releases.\n", pub.HTMLURL, p.Releases.GitHub)
	}

	// 5. Point the channel at it.
	return pointChannel(ctx, gh, signer, m, p.Name, *channel)
}

func cmdPromote(ctx context.Context, args []string) error {
	fl := flags("promote")
	channel := fl.String("channel", "", "channel to update (required)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *channel == "" {
		return errors.New("usage: ramekin promote <version> --channel <channel>")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if !slices.Contains(p.Channels, *channel) {
		return fmt.Errorf("channel %q is not listed in ramekin.yaml channels", *channel)
	}
	signer, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	verifier, err := projectVerifier(p)
	if err != nil {
		return err
	}
	gh, err := githubClient(p, true)
	if err != nil {
		return err
	}
	b, err := gh.DownloadAsset(ctx, release.Tag(p.Name, pos[0]), assetManifest, 1<<20)
	if err != nil {
		return fmt.Errorf("release %s: %w", pos[0], err)
	}
	m, err := manifest.Open(b, verifier)
	if err != nil {
		return err
	}
	return pointChannel(ctx, gh, signer, m, p.Name, *channel)
}

// pointChannel writes a channel manifest with a new, higher serial.
func pointChannel(ctx context.Context, gh *release.GitHub, signer signature.Signer, m *manifest.Manifest, name, channel string) error {
	path := release.ChannelPath(name, channel)
	var prev int64
	old, sha, err := gh.ReadFile(ctx, path)
	switch {
	case err == nil:
		if om, err := manifest.Unverified(old); err == nil {
			prev = om.Serial
		}
	case !errors.Is(err, release.ErrNotFound):
		return err
	}
	cm := *m
	cm.Channel = channel
	cm.Serial = manifest.NextSerial(prev, time.Now())
	env, err := manifest.Sign(&cm, signer)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%s: point %s at %s", name, channel, cm.Version)
	if err := gh.WriteFile(ctx, path, append(env, '\n'), sha, msg); err != nil {
		return err
	}
	fmt.Printf("channel %s -> %s %s (serial %d); devices pick it up on their next poll\n", channel, cm.Name, cm.Version, cm.Serial)
	return nil
}

func cmdReleases(ctx context.Context, args []string) error {
	p, err := loadProject()
	if err != nil {
		return err
	}
	gh, err := githubClient(p, false)
	if err != nil {
		return err
	}
	verifier, err := projectVerifier(p)
	if err != nil {
		return err
	}
	fmt.Println("channels:")
	for _, ch := range p.Channels {
		m, err := channelManifest(ctx, gh, verifier, p.Name, ch)
		switch {
		case err != nil:
			fmt.Printf("  %-12s error: %v\n", ch, err)
		case m == nil:
			fmt.Printf("  %-12s (unpublished)\n", ch)
		default:
			fmt.Printf("  %-12s %-16s serial %d  %s\n", ch, m.Version, m.Serial, m.Published.Format(time.RFC3339))
		}
	}
	rs, err := gh.ListReleases(ctx)
	if err != nil {
		return err
	}
	fmt.Println("releases:")
	prefix := release.Tag(p.Name, "")
	for _, r := range rs {
		if r.Draft || !strings.HasPrefix(r.TagName, prefix) {
			continue
		}
		lock := ""
		if r.Immutable {
			lock = "  (immutable)"
		}
		fmt.Printf("  %-16s%s\n", strings.TrimPrefix(r.TagName, prefix), lock)
	}
	fmt.Printf("devices poll: %s/<channel>.json\n", p.ChannelURL())
	return nil
}

// cmdWatch rescans the SBOM of every channel's release with today's
// vulnerability data, so CVEs disclosed after publishing still raise alerts.
func cmdWatch(ctx context.Context, args []string) error {
	fl := flags("watch")
	interval := fl.Duration("interval", 0, "repeat every interval (default: run once, for cron/CI)")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	gh, err := githubClient(p, false)
	if err != nil {
		return err
	}
	verifier, err := projectVerifier(p)
	if err != nil {
		return err
	}
	for {
		found, err := watchOnce(ctx, p, gh, verifier)
		if *interval == 0 {
			if err == nil && found > 0 {
				return fmt.Errorf("%d alerting vulnerabilities in published releases", found)
			}
			return err
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func watchOnce(ctx context.Context, p *config.Project, gh *release.GitHub, v signature.Verifier) (int, error) {
	tmp, err := os.MkdirTemp("", "ramekin-watch-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	total := 0
	seen := map[string]bool{}
	for _, ch := range p.Channels {
		m, err := channelManifest(ctx, gh, v, p.Name, ch)
		if err != nil {
			return total, err
		}
		if m == nil || seen[m.Version] {
			continue
		}
		seen[m.Version] = true
		b, err := gh.DownloadAsset(ctx, release.Tag(p.Name, m.Version), assetSBOM, 256<<20)
		if err != nil {
			return total, fmt.Errorf("sbom for %s: %w", m.Version, err)
		}
		path := filepath.Join(tmp, m.Version+".json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return total, err
		}
		rep, err := scan.Grype(ctx, path, os.Stderr)
		if err != nil {
			return total, err
		}
		fmt.Printf("== %s %s (channel %s)\n", p.Name, m.Version, ch)
		rep.Print(os.Stdout, 10)
		alert := rep.AtLeast(p.Alerts.MinSeverity, p.Scan.Ignore)
		total += len(alert)
		if len(alert) > 0 {
			subject := fmt.Sprintf("%s %s (deployed on channel %s)", p.Name, m.Version, ch)
			if err := scan.Alert(ctx, p.Alerts.Webhook, subject, alert); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %v\n", err)
			}
		}
	}
	return total, nil
}

// channelManifest returns a channel's verified manifest, or nil if the
// channel was never published.
func channelManifest(ctx context.Context, gh *release.GitHub, v signature.Verifier, name, ch string) (*manifest.Manifest, error) {
	b, _, err := gh.ReadFile(ctx, release.ChannelPath(name, ch))
	if errors.Is(err, release.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return manifest.Open(b, v)
}

func projectVerifier(p *config.Project) (signature.Verifier, error) {
	pub, err := os.ReadFile(p.Path(p.Signing.PubKey))
	if err != nil {
		return nil, err
	}
	return keys.LoadVerifier(pub)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func encryptFile(src, dst string, recipients []age.Recipient) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	w, err := age.Encrypt(out, recipients...)
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		out.Close()
		return err
	}
	if err := w.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
