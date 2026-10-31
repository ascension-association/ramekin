// Package build produces a SIF with apptainer from either an Apptainer
// definition file or a persistent overlay layered on a base image (flattened
// into the final SIF). No other container tooling is needed.
package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

type Builder struct {
	P      *config.Project
	Stdout io.Writer
	Stderr io.Writer
}

// Build writes the project's SIF to p.Output and returns its path.
func (b *Builder) Build(ctx context.Context) (string, error) {
	if err := need("apptainer"); err != nil {
		return "", err
	}
	out := b.P.Path(b.P.Output)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	var err error
	switch b.P.Source.Mode {
	case config.ModeDef:
		err = b.fromDef(ctx, out)
	case config.ModeOverlay:
		err = b.fromOverlay(ctx, out)
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

func (b *Builder) fromDef(ctx context.Context, out string) error {
	src := b.P.Path(b.P.Source.Def)
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	_, platform, _ := strings.Cut(b.P.Platform, "/")
	arch, variant, _ := strings.Cut(platform, "/")
	def, err := PrepareDef(ctx, string(raw), v1.Platform{OS: "linux", Architecture: arch, Variant: variant}, ResolveRemote)
	if err != nil {
		return fmt.Errorf("%s: %w", b.P.Source.Def, err)
	}
	gen := filepath.Join(filepath.Dir(out), ".ramekin.def")
	if err := os.WriteFile(gen, []byte(def), 0o644); err != nil {
		return err
	}
	cache := b.P.Path(filepath.Join("build", "cache"))
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	// Run from the definition's directory so relative %files paths resolve
	// the same way as with a plain `apptainer build`.
	return b.runIn(ctx, filepath.Dir(src), "apptainer", "build", "--force",
		"--warn-unused-build-args",
		"--build-arg", "TARGETOS=linux",
		"--build-arg", "TARGETARCH="+arch,
		"--build-arg", "BUILDARCH="+runtime.GOARCH,
		"--bind", cache+":"+CacheDir,
		out, gen)
}

// OverlayInit pulls the base image and creates the persistent overlay.
func (b *Builder) OverlayInit(ctx context.Context, force bool) error {
	if err := need("apptainer"); err != nil {
		return err
	}
	s := b.P.Source
	base, ovl := b.P.Path(s.BaseSIF), b.P.Path(s.Overlay)
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return err
	}
	pull := []string{"pull", "--force"}
	if arch := b.P.Arch(); arch != runtime.GOARCH {
		pull = append(pull, "--arch", arch)
	}
	if err := b.run(ctx, "apptainer", append(pull, base, s.Base)...); err != nil {
		return err
	}
	if _, err := os.Stat(ovl); err == nil && !force {
		fmt.Fprintf(b.Stdout, "keeping existing overlay %s (use --force to recreate)\n", s.Overlay)
		return nil
	}
	os.Remove(ovl)
	return b.run(ctx, "apptainer", "overlay", "create", "--fakeroot", "--size", fmt.Sprint(s.OverlaySizeMB), ovl)
}

// Shell opens an interactive shell with the overlay writable, so changes
// (apk add, edited files, ...) persist across sessions.
func (b *Builder) Shell(ctx context.Context, stdin io.Reader) error {
	s := b.P.Source
	cmd := exec.CommandContext(ctx, "apptainer", "shell", "--fakeroot", "--overlay", b.P.Path(s.Overlay), b.P.Path(s.BaseSIF))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, b.Stdout, b.Stderr
	return cmd.Run()
}

func (b *Builder) fromOverlay(ctx context.Context, out string) error {
	s := b.P.Source
	base, ovl := b.P.Path(s.BaseSIF), b.P.Path(s.Overlay)
	for _, f := range []string{base, ovl} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("%w (run `ramekin overlay init`)", err)
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".overlay-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Read the overlay's upper layer through an image-mounted bind, using the
	// base image's own find/tar so no host tooling beyond apptainer is needed.
	execIn := func(script string) error {
		return b.run(ctx, "apptainer", "exec", "--fakeroot",
			"-B", tmp+":/ramekin-out",
			"-B", ovl+":/ramekin-upper:image-src=/upper",
			base, "/bin/sh", "-c", "cd /ramekin-upper && "+script)
	}
	if err := execIn("find . -mindepth 1 -type c > /ramekin-out/chars.txt && find . -mindepth 1 ! -type c > /ramekin-out/all.txt"); err != nil {
		return err
	}
	chars, err := readLines(filepath.Join(tmp, "chars.txt"))
	if err != nil {
		return err
	}
	all, err := readLines(filepath.Join(tmp, "all.txt"))
	if err != nil {
		return err
	}
	plan := PlanOverlay(chars, all)
	for name, lines := range map[string][]string{"include.txt": plan.Include, "whiteouts.txt": plan.Whiteouts, "opaque.txt": plan.Opaque} {
		if err := writeLines(filepath.Join(tmp, name), lines); err != nil {
			return err
		}
	}
	if len(plan.Include) == 0 {
		fmt.Fprintln(b.Stdout, "overlay has no added or changed files")
	}
	if err := execIn("tar -cf /ramekin-out/upper.tar --no-recursion -T /ramekin-out/include.txt"); err != nil {
		return err
	}
	def := filepath.Join(tmp, "ramekin.def")
	if err := os.WriteFile(def, []byte(overlayDef(base, tmp, s.Runscript)), 0o644); err != nil {
		return err
	}
	return b.run(ctx, "apptainer", "build", "--fakeroot", "--force", out, def)
}

// OverlayPlan is how an overlayfs upper layer maps onto the final image.
type OverlayPlan struct {
	Include   []string // paths to copy (relative, no leading ./)
	Whiteouts []string // paths deleted relative to the base
	Opaque    []string // directories whose base contents were replaced
}

// PlanOverlay interprets an upper layer listing. chars are character devices
// (kernel overlayfs whiteouts); all is every other entry. fuse-overlayfs may
// instead record whiteouts as ".wh.<name>" files and opaque directories with
// a ".wh..wh..opq" marker; both conventions are handled.
func PlanOverlay(chars, all []string) OverlayPlan {
	var p OverlayPlan
	for _, c := range chars {
		p.Whiteouts = append(p.Whiteouts, clean(c))
	}
	for _, e := range all {
		e = clean(e)
		dir, base := filepath.Split(e)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case base == ".wh..wh..opq":
			p.Opaque = append(p.Opaque, dir)
		case strings.HasPrefix(base, ".wh."):
			p.Whiteouts = append(p.Whiteouts, filepath.Join(dir, strings.TrimPrefix(base, ".wh.")))
		case e != "":
			p.Include = append(p.Include, e)
		}
	}
	return p
}

func clean(p string) string {
	if p == "." {
		return ""
	}
	return strings.TrimPrefix(p, "./")
}

func overlayDef(base, tmp, runscript string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Bootstrap: localimage\nFrom: %s\n\n%%files\n", base)
	for _, f := range []string{"upper.tar", "whiteouts.txt", "opaque.txt"} {
		fmt.Fprintf(&sb, "    %s /.ramekin-%s\n", filepath.Join(tmp, f), f)
	}
	sb.WriteString(`
%post
    while IFS= read -r p; do [ -n "$p" ] && rm -rf "/$p"; done < /.ramekin-whiteouts.txt
    while IFS= read -r p; do [ -n "$p" ] && find "/$p" -mindepth 1 -maxdepth 1 -exec rm -rf {} +; done < /.ramekin-opaque.txt
    tar -C / -xpf /.ramekin-upper.tar
    rm -f /.ramekin-upper.tar /.ramekin-whiteouts.txt /.ramekin-opaque.txt
`)
	if runscript != "" {
		fmt.Fprintf(&sb, "\n%%runscript\n    exec %s \"$@\"\n", runscript)
	}
	return sb.String()
}

// Sandbox unpacks a SIF into a directory (used for SBOM generation).
func (b *Builder) Sandbox(ctx context.Context, sif, dir string) error {
	return b.run(ctx, "apptainer", "build", "--force", "--sandbox", dir, sif)
}

func (b *Builder) run(ctx context.Context, name string, args ...string) error {
	return b.runIn(ctx, "", name, args...)
}

func (b *Builder) runIn(ctx context.Context, dir, name string, args ...string) error {
	fmt.Fprintf(b.Stderr, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = b.Stdout, b.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return nil
}

func need(bin string) error {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%s is required on this workstation but was not found on PATH", bin)
	}
	return nil
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func writeLines(path string, lines []string) error {
	s := strings.Join(lines, "\n")
	if s != "" {
		s += "\n"
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

// HasShell reports whether the SIF's root filesystem contains /bin/sh, which
// ramekind uses to load the image's environment and start its runscript.
// known is false when the check couldn't run (e.g. unsquashfs is missing).
func HasShell(ctx context.Context, sif string) (has, known bool) {
	rfs, err := sifimg.PrimaryRootFS(sif)
	if err != nil {
		return false, false
	}
	out, err := exec.CommandContext(ctx, "unsquashfs", "-o", fmt.Sprint(rfs.Offset), "-l", "-d", "", sif).Output()
	if err != nil {
		return false, false
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l == "/bin/sh" || l == "/usr/bin/sh" {
			return true, true
		}
	}
	return false, true
}
