package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/ascension-association/ramekin/internal/build"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/scan"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

//go:embed all:templates
var templates embed.FS

func loadProject() (*config.Project, error) { return config.LoadProject(".") }

func builder(p *config.Project) *build.Builder {
	return &build.Builder{P: p, Stdout: os.Stdout, Stderr: os.Stderr}
}

// sifArg returns the SIF named on the command line or the project output.
func sifArg(p *config.Project, pos []string, i int) string {
	if len(pos) > i {
		return pos[i]
	}
	return p.Path(p.Output)
}

func cmdInit(ctx context.Context, args []string) error {
	fl := flags("init")
	mode := fl.String("mode", config.ModeDef, "def (an Apptainer definition file) or overlay")
	name := fl.String("name", "", "project name (default: directory name)")
	platform := fl.String("platform", "linux/arm64", "target platform of the devices")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	if *mode != config.ModeDef && *mode != config.ModeOverlay {
		return fmt.Errorf("--mode must be %s or %s", config.ModeDef, config.ModeOverlay)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = filepath.Base(abs)
	}
	data := map[string]string{"Name": *name, "Mode": *mode, "Platform": *platform, "Base": config.DefaultBase}
	err = fs.WalkDir(templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(path, "templates/"), ".tmpl")
		rel = strings.ReplaceAll(rel, "dot-", ".")
		if (rel == "example.def" || rel == "app.sh") && *mode != config.ModeDef {
			return nil
		}
		if rel == watchWorkflow {
			// A new project publishes nothing yet, so there is nothing to
			// watch; the first `ramekin publish` adds the workflow.
			return nil
		}
		dst := filepath.Join(abs, rel)
		if _, err := os.Stat(dst); err == nil {
			fmt.Printf("exists, skipping: %s\n", rel)
			return nil
		}
		t, err := template.New(filepath.Base(path)).Delims("[[", "]]").ParseFS(templates, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Printf("created %s\n", rel)
		return t.Execute(f, data)
	})
	if err != nil {
		return err
	}
	next := "edit example.def, then: ramekin keys generate && ramekin build"
	if *mode == config.ModeOverlay {
		next = "ramekin keys generate && ramekin overlay init && ramekin shell"
	}
	fmt.Printf("\nnext: cd %s && %s\n", dir, next)
	return nil
}

func cmdKeys(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "generate" {
		return errors.New("usage: ramekin keys generate")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	fleet := p.Path("keys/fleet.agekey")
	dropped, err := dropCopiedKeys(p.Path(p.Signing.Key), p.Path(p.Signing.PubKey), fleet, p.Path(p.Encrypt.RecipientsFile))
	if err != nil {
		return err
	}
	for _, f := range dropped {
		if rel, err := filepath.Rel(p.Path("."), f); err == nil {
			f = rel
		}
		fmt.Printf("replacing:     %s (its secret key isn't here, so it came with a cloned project)\n", f)
	}
	if err := keys.GenerateSigning(p.Path(p.Signing.Key), p.Path(p.Signing.PubKey)); err != nil {
		return err
	}
	fmt.Printf("signing key:   %s (keep secret; CI can pass it in RAMEKIN_SIGNING_KEY)\n", p.Signing.Key)
	fmt.Printf("verify key:    %s (installed on devices)\n", p.Signing.PubKey)
	rcpt, err := keys.GenerateAge(fleet)
	if err != nil {
		return err
	}
	if err := keys.AppendRecipient(p.Path(p.Encrypt.RecipientsFile), rcpt, "fleet key (keys/fleet.agekey)"); err != nil {
		return err
	}
	fmt.Printf("fleet age key: keys/fleet.agekey -> %s\n", rcpt)
	fmt.Printf("recipients:    %s\n", p.Encrypt.RecipientsFile)
	return nil
}

func cmdOverlay(ctx context.Context, args []string) error {
	fl := flags("overlay")
	force := fl.Bool("force", false, "recreate overlay.img even if it exists (discards changes)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "init" {
		return errors.New("usage: ramekin overlay init [--force]")
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if p.Source.Mode != config.ModeOverlay {
		return errors.New("project source.mode is not overlay")
	}
	return builder(p).OverlayInit(ctx, *force)
}

func cmdShell(ctx context.Context, args []string) error {
	p, err := loadProject()
	if err != nil {
		return err
	}
	if p.Source.Mode != config.ModeOverlay {
		return errors.New("`ramekin shell` is for overlay mode; in def mode edit your definition file (source.def in ramekin.yaml)")
	}
	return builder(p).Shell(ctx, os.Stdin)
}

func cmdBuild(ctx context.Context, args []string) error {
	fl := flags("build")
	noScan := fl.Bool("no-scan", false, "skip the CVE scan")
	if _, err := parse(fl, args); err != nil {
		return err
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	out, err := builder(p).Build(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("built %s\n", out)
	warnNoShell(ctx, out)
	if *noScan {
		return nil
	}
	rep, err := scanSIF(ctx, p, out, sbomPath(out))
	if err != nil {
		return err
	}
	return applyPolicy(ctx, p, rep, p.Name+" (local build)", false)
}

func cmdScan(ctx context.Context, args []string) error {
	fl := flags("scan")
	strict := fl.Bool("strict", false, "exit non-zero when findings reach scan.fail_on")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	sif := sifArg(p, pos, 0)
	rep, err := scanSIF(ctx, p, sif, sbomPath(sif))
	if err != nil {
		return err
	}
	return applyPolicy(ctx, p, rep, p.Name+" ("+filepath.Base(sif)+")", *strict)
}

func cmdSign(ctx context.Context, args []string) error {
	p, err := loadProject()
	if err != nil {
		return err
	}
	pos, _ := parse(flags("sign"), args)
	s, err := keys.LoadSigner(p.Path(p.Signing.Key))
	if err != nil {
		return err
	}
	sif := sifArg(p, pos, 0)
	if err := sifimg.Sign(ctx, sif, s); err != nil {
		return err
	}
	fmt.Printf("signed %s\n", sif)
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	p, err := loadProject()
	if err != nil {
		return err
	}
	pos, _ := parse(flags("verify"), args)
	pub, err := os.ReadFile(p.Path(p.Signing.PubKey))
	if err != nil {
		return err
	}
	v, err := keys.LoadVerifier(pub)
	if err != nil {
		return err
	}
	sif := sifArg(p, pos, 0)
	if err := sifimg.Verify(ctx, sif, v); err != nil {
		return err
	}
	fmt.Printf("%s: signature OK\n", sif)
	return nil
}

func sbomPath(sif string) string {
	return strings.TrimSuffix(sif, ".sif") + ".sbom.json"
}

// scanSIF unpacks the SIF, writes an SBOM to sbomOut and matches it with grype.
func scanSIF(ctx context.Context, p *config.Project, sif, sbomOut string) (*scan.Report, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(sif), ".scan-")
	if err != nil {
		return nil, err
	}
	defer removeAll(tmp)
	rootfs := filepath.Join(tmp, "rootfs")
	if err := builder(p).Sandbox(ctx, sif, rootfs); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "+ generating SBOM and scanning for CVEs")
	if err := scan.SBOM(ctx, rootfs, sbomOut, os.Stderr); err != nil {
		return nil, err
	}
	return scan.Grype(ctx, sbomOut, os.Stderr)
}

// applyPolicy prints the report, alerts the webhook, and, when gate is set,
// fails if findings reach scan.fail_on.
func applyPolicy(ctx context.Context, p *config.Project, rep *scan.Report, subject string, gate bool) error {
	rep.Print(os.Stdout, 25)
	if alert := rep.AtLeast(p.Alerts.MinSeverity, p.Scan.Ignore); len(alert) > 0 {
		if err := scan.Alert(ctx, p.Alerts.Webhook, subject, alert); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	blocking := rep.AtLeast(p.Scan.FailOn, p.Scan.Ignore)
	if len(blocking) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%d vulnerabilities at or above %q (scan.fail_on)", len(blocking), p.Scan.FailOn)
	if gate {
		return errors.New(msg + "; fix them, add accepted IDs to scan.ignore, or pass --allow-vulns")
	}
	fmt.Fprintf(os.Stderr, "WARNING: %s; `ramekin publish` will refuse this image\n", msg)
	return nil
}

// removeAll deletes a tree even if unpacked image directories are read-only.
func removeAll(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o755)
		}
		return nil
	})
	os.RemoveAll(dir)
}

// warnNoShell checks how ramekind will start an image without /bin/sh: it
// runs the image's command directly, which works for simple runscripts and
// for an OCI image's ENTRYPOINT and CMD, but not for ones that need a shell.
func warnNoShell(ctx context.Context, sif string) {
	has, known := build.HasShell(ctx, sif)
	if !known || has {
		return
	}
	argv, err := build.ShellFreeCommand(ctx, sif)
	if err == nil {
		fmt.Fprintf(os.Stderr, "%s has no shell; ramekind will run %q directly\n", filepath.Base(sif), argv)
		return
	}
	fmt.Fprintf(os.Stderr, `WARNING: %s has no /bin/sh, and ramekind can't start it without one:
  %v
so it would fail its health check on a device. Make its %%runscript a single
command (such as: exec /usr/bin/app --flag "$@"), add a shell to the image,
or run the app directly with
  ramekin device set <device> --command "/path/to/app --flags"
`, filepath.Base(sif), err)
}

// dropCopiedKeys removes public key files whose secret half is absent. A
// project cloned from someone else's repository carries their committed
// verify key and recipients but not their secret keys; those files must go
// before `ramekin keys generate` writes the new owner's keys. It returns the
// paths it removed.
func dropCopiedKeys(signKey, signPub, fleetKey, recipients string) ([]string, error) {
	var dropped []string
	for _, pair := range [][2]string{{signKey, signPub}, {fleetKey, recipients}} {
		if _, err := os.Stat(pair[0]); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return dropped, err
			}
			continue
		}
		if err := os.Remove(pair[1]); err == nil {
			dropped = append(dropped, pair[1])
		} else if !errors.Is(err, os.ErrNotExist) {
			return dropped, err
		}
	}
	return dropped, nil
}

// watchWorkflow is the GitHub Actions workflow that runs `ramekin watch`.
const watchWorkflow = ".github/workflows/ramekin-cve-watch.yml"

// addWatchWorkflow writes the CVE watch workflow into the project unless it
// already exists, and reports whether it did.
func addWatchWorkflow(p *config.Project) (bool, error) {
	dst := p.Path(watchWorkflow)
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	src := "templates/dot-github/workflows/ramekin-cve-watch.yml.tmpl"
	t, err := template.New(filepath.Base(src)).Delims("[[", "]]").ParseFS(templates, src)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]string{"Name": p.Name}); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, buf.Bytes(), 0o644)
}
