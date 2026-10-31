package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/manifest"
	"github.com/ascension-association/ramekin/internal/scan"
	"github.com/ascension-association/ramekin/internal/sifimg"
	"github.com/ascension-association/ramekin/internal/store"
)

func openStore(p *config.Project) (*store.Store, error) {
	if p.Store.Endpoint == "" || p.Store.Bucket == "" {
		return nil, errors.New("set store.endpoint and store.bucket in ramekin.yaml")
	}
	ak, sk := env(p.Store.AccessKeyEnv), env(p.Store.SecretKeyEnv)
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("set %s and %s", p.Store.AccessKeyEnv, p.Store.SecretKeyEnv)
	}
	return store.New(store.Options{
		Endpoint: p.Store.Endpoint, Region: p.Store.Region, Bucket: p.Store.Bucket,
		Prefix: p.Store.Prefix, AccessKey: ak, SecretKey: sk,
	})
}

func cmdPublish(ctx context.Context, args []string) error {
	fl := flags("publish")
	version := fl.String("version", "", "release version (required, immutable once published)")
	channel := fl.String("channel", "", "channel to point at the release (default: first in ramekin.yaml)")
	allowVulns := fl.Bool("allow-vulns", false, "publish even if findings reach scan.fail_on")
	skipLockCheck := fl.Bool("skip-lock-check", false, "don't require bucket versioning and object lock")
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
	st, err := openStore(p)
	if err != nil {
		return err
	}
	if !*skipLockCheck {
		if err := st.CheckBucket(ctx); err != nil {
			return fmt.Errorf("%w (or pass --skip-lock-check)", err)
		}
	}
	objKey := st.Key(store.ReleaseObject(p.Name, *version)...)
	if exists, err := st.Exists(ctx, objKey); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("version %s is already published; releases are immutable, pick a new version", *version)
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
	sbom := filepath.Join(outDir, "sbom.json")
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
	enc := signed + ".age"
	if err := encryptFile(signed, enc, recipients); err != nil {
		return err
	}
	fmt.Printf("signed and encrypted for %d recipient(s): %s\n", len(recipients), enc)

	// 4. Upload immutable artifacts with object-lock retention.
	lock := store.PutOpts{Mode: p.Store.RetentionMode}
	if p.Store.RetentionDays > 0 {
		lock.RetainUntil = time.Now().AddDate(0, 0, p.Store.RetentionDays)
	}
	m := &manifest.Manifest{
		Name: p.Name, Version: *version, Arch: p.Arch(),
		Object: objKey, SHA256: sum, Size: size,
		SBOM:      st.Key(store.ReleaseSBOM(*version)...),
		Published: time.Now().UTC(),
		Vulns:     rep.Counts(),
	}
	releaseManifest, err := manifest.Sign(m, signer)
	if err != nil {
		return err
	}
	if err := putFile(ctx, st, enc, objKey, "application/octet-stream", lock); err != nil {
		return err
	}
	if err := putFile(ctx, st, sbom, m.SBOM, "application/json", lock); err != nil {
		return err
	}
	if err := putBytes(ctx, st, releaseManifest, st.Key(store.ReleaseManifest(*version)...), lock); err != nil {
		return err
	}
	fmt.Printf("uploaded %s (retained until %s, %s)\n", objKey, lock.RetainUntil.Format(time.DateOnly), lock.Mode)

	// 5. Point the channel at it.
	return pointChannel(ctx, p, st, signer, m, *channel)
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
	st, err := openStore(p)
	if err != nil {
		return err
	}
	b, err := st.GetBytes(ctx, st.Key(store.ReleaseManifest(pos[0])...))
	if err != nil {
		return fmt.Errorf("release %s: %w", pos[0], err)
	}
	m, err := manifest.Open(b, verifier)
	if err != nil {
		return err
	}
	return pointChannel(ctx, p, st, signer, m, *channel)
}

func pointChannel(ctx context.Context, p *config.Project, st *store.Store, signer signature.Signer, m *manifest.Manifest, channel string) error {
	key := st.Key(store.Channel(channel)...)
	var prev int64
	if b, err := st.GetBytes(ctx, key); err == nil {
		if old, err := manifest.Unverified(b); err == nil {
			prev = old.Serial
		}
	} else if !store.IsNotFound(err) {
		return err
	}
	cm := *m
	cm.Channel = channel
	cm.Serial = manifest.NextSerial(prev, time.Now())
	env, err := manifest.Sign(&cm, signer)
	if err != nil {
		return err
	}
	// Channel pointers are overwritten; bucket versioning keeps their history.
	if err := putBytes(ctx, st, env, key, store.PutOpts{}); err != nil {
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
	st, err := openStore(p)
	if err != nil {
		return err
	}
	verifier, err := projectVerifier(p)
	if err != nil {
		return err
	}
	fmt.Println("channels:")
	for _, ch := range p.Channels {
		m, err := channelManifest(ctx, st, verifier, ch)
		switch {
		case err != nil:
			fmt.Printf("  %-12s error: %v\n", ch, err)
		case m == nil:
			fmt.Printf("  %-12s (unpublished)\n", ch)
		default:
			fmt.Printf("  %-12s %-16s serial %d  %s\n", ch, m.Version, m.Serial, m.Published.Format(time.RFC3339))
		}
	}
	objs, err := st.List(ctx, "releases")
	if err != nil {
		return err
	}
	fmt.Println("releases:")
	for _, k := range objs {
		if strings.HasSuffix(k, "/manifest.json") {
			fmt.Printf("  %s\n", filepath.Base(filepath.Dir(k)))
		}
	}
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
	st, err := openStore(p)
	if err != nil {
		return err
	}
	verifier, err := projectVerifier(p)
	if err != nil {
		return err
	}
	for {
		found, err := watchOnce(ctx, p, st, verifier)
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

func watchOnce(ctx context.Context, p *config.Project, st *store.Store, v signature.Verifier) (int, error) {
	tmp, err := os.MkdirTemp("", "ramekin-watch-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)
	total := 0
	seen := map[string]bool{}
	for _, ch := range p.Channels {
		m, err := channelManifest(ctx, st, v, ch)
		if err != nil {
			return total, err
		}
		if m == nil || m.SBOM == "" || seen[m.Version] {
			continue
		}
		seen[m.Version] = true
		b, err := st.GetBytes(ctx, m.SBOM)
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

func cmdFleet(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: ramekin fleet status")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	st, err := openStore(p)
	if err != nil {
		return err
	}
	objs, err := st.List(ctx, "status")
	if err != nil {
		return err
	}
	fmt.Printf("%-20s %-10s %-16s %-16s %-20s %s\n", "DEVICE", "CHANNEL", "CURRENT", "PREVIOUS", "LAST CHECK", "ERROR")
	for _, k := range objs {
		b, err := st.GetBytes(ctx, k)
		if err != nil {
			return err
		}
		var s agent.DeviceStatus
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		ver := func(r *agent.Release) string {
			if r == nil {
				return "-"
			}
			return r.Version
		}
		fmt.Printf("%-20s %-10s %-16s %-16s %-20s %s\n", s.Device, s.Channel, ver(s.Current), ver(s.Previous),
			s.LastCheck.Local().Format(time.DateTime), s.LastError)
	}
	return nil
}

func channelManifest(ctx context.Context, st *store.Store, v signature.Verifier, ch string) (*manifest.Manifest, error) {
	b, err := st.GetBytes(ctx, st.Key(store.Channel(ch)...))
	if store.IsNotFound(err) {
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

func putFile(ctx context.Context, st *store.Store, path, key, ctype string, o store.PutOpts) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	o.ContentType = ctype
	if err := st.Put(ctx, key, f, fi.Size(), o); err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

func putBytes(ctx context.Context, st *store.Store, b []byte, key string, o store.PutOpts) error {
	o.ContentType = "application/json"
	if err := st.Put(ctx, key, bytes.NewReader(b), int64(len(b)), o); err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}
