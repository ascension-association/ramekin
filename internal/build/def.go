package build

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// CacheDir is where every stage's %post sees Ramekin's persistent build
// cache (point GOCACHE, pip, npm, ... at it to make rebuilds fast).
const CacheDir = "/cache"

// Apptainer's --arch applies to every stage of a multi-stage definition, which
// would run compilers in build stages under qemu. Ramekin instead builds for
// the workstation's architecture and pins only the final stage's base image
// to the device's platform, resolved at build time so `latest` stays fresh.
// This mirrors Docker's FROM --platform=$BUILDPLATFORM pattern: build stages
// run natively and cross-compile using the TARGETOS/TARGETARCH build args.

var (
	headerLine  = regexp.MustCompile(`(?i)^\s*bootstrap\s*:\s*(\S+)`)
	fromLine    = regexp.MustCompile(`(?i)^(\s*from\s*:\s*)(\S+)\s*$`)
	sectionLine = regexp.MustCompile(`^%\w+`)
)

// stage is a line range of a definition file starting at a Bootstrap header.
type stage struct {
	start, end int // [start, end) line indexes
	bootstrap  string
	from       int // line index of From:, or -1
}

func parseStages(lines []string) []stage {
	var stages []stage
	inHeader := false
	for i, l := range lines {
		if m := headerLine.FindStringSubmatch(l); m != nil && !inSection(lines, i) {
			if n := len(stages); n > 0 {
				stages[n-1].end = i
			}
			stages = append(stages, stage{start: i, bootstrap: strings.ToLower(m[1]), from: -1})
			inHeader = true
			continue
		}
		if len(stages) == 0 {
			continue
		}
		if sectionLine.MatchString(l) {
			inHeader = false
		} else if inHeader && fromLine.MatchString(l) {
			stages[len(stages)-1].from = i
		}
	}
	if n := len(stages); n > 0 {
		stages[n-1].end = len(lines)
	}
	return stages
}

// inSection reports whether line i sits inside a % section body, where a
// "Bootstrap:" string is script text, not a new stage. A header line is never
// indented in practice, so indentation is treated as section content.
func inSection(lines []string, i int) bool {
	return strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")
}

// Resolver maps an image reference to a digest reference for one platform.
type Resolver func(ctx context.Context, image string, platform v1.Platform) (string, error)

// PrepareDef rewrites a definition file for a Ramekin build: the final stage's
// docker base image is pinned to the target platform, and every stage gets a
// %setup that creates the cache mount point.
func PrepareDef(ctx context.Context, def string, target v1.Platform, resolve Resolver) (string, error) {
	lines := strings.Split(def, "\n")
	stages := parseStages(lines)
	if len(stages) == 0 {
		return "", fmt.Errorf("no Bootstrap: header found")
	}
	final := stages[len(stages)-1]
	if final.from >= 0 && final.bootstrap == "docker" && !strings.Contains(lines[final.from], "{{") {
		m := fromLine.FindStringSubmatch(lines[final.from])
		ref := strings.TrimPrefix(m[2], "docker://")
		pinned, err := resolve(ctx, ref, target)
		if err != nil {
			return "", fmt.Errorf("final stage base image %s: %w", ref, err)
		}
		lines[final.from] = m[1] + pinned
	}

	var out []string
	setup := []string{"", "%setup", `    mkdir -p "$APPTAINER_ROOTFS` + CacheDir + `"`, ""}
	for i, st := range stages {
		if i == 0 {
			out = append(out, lines[:st.start]...)
		}
		body := lines[st.start:st.end]
		// Keep trailing blank lines after our section, not before it.
		j := len(body)
		for j > 0 && strings.TrimSpace(body[j-1]) == "" {
			j--
		}
		out = append(out, body[:j]...)
		out = append(out, setup...)
		out = append(out, body[j:]...)
	}
	return strings.Join(out, "\n"), nil
}

// ResolveRemote looks image up in its registry (with the user's docker/podman
// credentials) and returns repo@sha256:... for the platform's manifest.
func ResolveRemote(ctx context.Context, image string, platform v1.Platform) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(platform))
	if err != nil {
		return "", err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", err
	}
	if cf.Architecture != platform.Architecture {
		return "", fmt.Errorf("has no %s/%s variant (found %s)", platform.OS, platform.Architecture, cf.Architecture)
	}
	d, err := img.Digest()
	if err != nil {
		return "", err
	}
	return ref.Context().Digest(d.String()).Name(), nil
}
