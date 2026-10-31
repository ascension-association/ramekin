// Package build produces a SIF from either a Containerfile (built with podman
// or docker, then converted by apptainer) or a persistent overlay layered on
// a base image (flattened into the final SIF).
package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ascension-association/ramekin/internal/config"
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
	case config.ModeContainerfile:
		err = b.fromContainerfile(ctx, out)
	case config.ModeOverlay:
		err = b.fromOverlay(ctx, out)
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

func (b *Builder) fromContainerfile(ctx context.Context, out string) error {
	engine, err := b.engine()
	if err != nil {
		return err
	}
	tag := "localhost/ramekin-build/" + b.P.Name + ":latest"
	if err := b.run(ctx, engine, "build",
		"--platform", b.P.Platform,
		"-f", b.P.Path(b.P.Source.Containerfile),
		"-t", tag,
		b.P.Path(b.P.Source.Context)); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".oci-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "image.tar")
	src := "docker-archive://" + archive
	saveArgs := []string{"save", "--platform", b.P.Platform, "-o", archive, tag}
	if engine == "podman" {
		saveArgs = []string{"save", "--format", "oci-archive", "-o", archive, tag}
		src = "oci-archive://" + archive
	}
	if err := b.run(ctx, engine, saveArgs...); err != nil {
		return err
	}
	// apptainer selects the image for --arch (default: the host) from the archive.
	return b.run(ctx, "apptainer", "build", "--force", "--arch", b.P.Arch(), out, src)
}

func (b *Builder) engine() (string, error) {
	if e := b.P.Source.Engine; e != "" {
		return e, need(e)
	}
	for _, e := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(e); err == nil {
			return e, nil
		}
	}
	return "", errors.New("containerfile mode needs podman or docker on PATH")
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
	fmt.Fprintf(b.Stderr, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
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
