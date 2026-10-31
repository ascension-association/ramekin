package build

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func fakeResolve(_ context.Context, image string, p v1.Platform) (string, error) {
	if strings.Contains(image, "missing") {
		return "", errors.New("not found")
	}
	return image + "@" + p.Architecture, nil
}

var arm64 = v1.Platform{OS: "linux", Architecture: "arm64"}

func TestPrepareDefMultiStage(t *testing.T) {
	def := `Bootstrap: docker
From: cgr.dev/chainguard/go:latest
Stage: build

%post
    # Indented text is script content. (Like apptainer itself, an
    # unindented "Bootstrap:" line always starts a new stage.)
    echo "Bootstrap: docker
    From: not-a-stage" > /notes.txt
    go build -o /app .

Bootstrap: docker
From: docker://cgr.dev/chainguard/wolfi-base:latest
Stage: final

%files from build
    /app /usr/bin/app
`
	got, err := PrepareDef(context.Background(), def, arm64, fakeResolve)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "From: cgr.dev/chainguard/go:latest\n") {
		t.Error("build stage base image must stay unpinned (it runs natively)")
	}
	if !strings.Contains(got, "From: cgr.dev/chainguard/wolfi-base:latest@arm64\n") {
		t.Errorf("final stage not pinned to the target:\n%s", got)
	}
	if strings.Contains(got, "not-a-stage@") {
		t.Error("a Bootstrap: line inside a script was treated as a stage")
	}
	if n := strings.Count(got, `mkdir -p "$APPTAINER_ROOTFS/cache"`); n != 2 {
		t.Errorf("want the cache mount point created in both stages, got %d:\n%s", n, got)
	}
	// The injected %setup must come after the build stage's own content.
	if strings.Index(got, "go build") > strings.Index(got, "%setup") {
		t.Errorf("%%setup inserted inside the %%post script:\n%s", got)
	}
}

func TestPrepareDefLeavesOthersAlone(t *testing.T) {
	for _, def := range []string{
		"Bootstrap: localimage\nFrom: base.sif\n",
		"Bootstrap: docker\nFrom: {{ BASE }}\n",
	} {
		got, err := PrepareDef(context.Background(), def, arm64, fakeResolve)
		if err != nil || strings.Contains(got, "@arm64") {
			t.Errorf("%q: got %q, %v", def, got, err)
		}
	}
	if _, err := PrepareDef(context.Background(), "Bootstrap: docker\nFrom: missing:1\n", arm64, fakeResolve); err == nil {
		t.Error("resolver error not reported")
	}
	if _, err := PrepareDef(context.Background(), "%post\n    true\n", arm64, fakeResolve); err == nil {
		t.Error("definition without Bootstrap accepted")
	}
}
