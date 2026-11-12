package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPlanOverlay(t *testing.T) {
	chars := []string{"./etc/motd"}
	all := []string{
		"./usr", "./usr/bin", "./usr/bin/app",
		"./.profile",
		"./var/cache/.wh.apk",
		"./opt/thing", "./opt/thing/.wh..wh..opq", "./opt/thing/new",
	}
	got := PlanOverlay(chars, all)
	want := OverlayPlan{
		Include:   []string{"usr", "usr/bin", "usr/bin/app", ".profile", "opt/thing", "opt/thing/new"},
		Whiteouts: []string{"etc/motd", "var/cache/apk"},
		Opaque:    []string{"opt/thing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestOverlayDef(t *testing.T) {
	def := overlayDef("/p/base.sif", "/tmp/x", "/usr/bin/app --serve")
	for _, s := range []string{"Bootstrap: localimage", "From: /p/base.sif", "/tmp/x/upper.tar /.ramekin-upper.tar", "exec /usr/bin/app --serve \"$@\""} {
		if !strings.Contains(def, s) {
			t.Errorf("def missing %q:\n%s", s, def)
		}
	}
}

// TestOverlayDefOpaqueDirs runs the generated opaque-directory step under
// sh -e, as %post does. An opaque directory the base image doesn't have
// (new in the overlay, e.g. /var/cache/ldconfig after a glibc upgrade) must
// be skipped, not fail the build; one it has must be emptied.
func TestOverlayDefOpaqueDirs(t *testing.T) {
	var line string
	for _, l := range strings.Split(overlayDef("/b.sif", "/t", ""), "\n") {
		if strings.Contains(l, ".ramekin-opaque.txt") && strings.Contains(l, "while") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatal("no opaque-directory step in the definition")
	}
	root := t.TempDir()
	// Run against root instead of /.
	line = strings.ReplaceAll(line, `"/$p"`, `"$R/$p"`)
	line = strings.ReplaceAll(line, "/.ramekin-opaque.txt", "$R/.ramekin-opaque.txt")
	if err := os.MkdirAll(filepath.Join(root, "opt/thing"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "opt/thing/old"), []byte("x"), 0o644)
	// The missing directory comes last, so the loop ends on it.
	os.WriteFile(filepath.Join(root, ".ramekin-opaque.txt"), []byte("opt/thing\nvar/cache/ldconfig\n"), 0o644)

	cmd := exec.Command("/bin/sh", "-e", "-c", line+"\necho done")
	cmd.Env = append(os.Environ(), "R="+root)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "done") {
		t.Fatalf("opaque step failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "opt/thing/old")); !os.IsNotExist(err) {
		t.Errorf("the base's opt/thing wasn't emptied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "opt/thing")); err != nil {
		t.Errorf("opt/thing itself was removed: %v", err)
	}
}
