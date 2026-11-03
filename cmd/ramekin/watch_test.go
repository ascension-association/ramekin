package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ascension-association/ramekin/internal/config"
)

// A new project publishes nothing, so init leaves the CVE watch workflow
// for the first publish to add.
func TestInitThenAddWatchWorkflow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	if err := cmdInit(context.Background(), []string{dir}); err != nil {
		t.Fatal(err)
	}
	wf := filepath.Join(dir, watchWorkflow)
	if _, err := os.Stat(wf); !os.IsNotExist(err) {
		t.Fatalf("init created %s (err %v)", watchWorkflow, err)
	}
	p, err := config.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := addWatchWorkflow(p)
	if err != nil || !added {
		t.Fatalf("addWatchWorkflow = %v, %v; want true, nil", added, err)
	}
	b, err := os.ReadFile(wf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "every published release of demo") || !strings.Contains(string(b), "ramekin watch") {
		t.Fatalf("workflow not rendered for project demo:\n%s", b)
	}
	if err := os.WriteFile(wf, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if added, err := addWatchWorkflow(p); err != nil || added {
		t.Fatalf("second addWatchWorkflow = %v, %v; want false, nil", added, err)
	}
	if b, _ := os.ReadFile(wf); string(b) != "edited" {
		t.Fatal("addWatchWorkflow overwrote an existing workflow")
	}
}
