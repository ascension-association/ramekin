package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProc exits immediately when its image content is "crash".
type fakeProc struct {
	done chan struct{}
	once sync.Once
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { return errors.New("exit status 1") }
func (p *fakeProc) Stop(time.Duration)    { p.once.Do(func() { close(p.done) }) }

type fakeRuntime struct {
	mu      sync.Mutex
	started []string // image contents, in start order
}

func (r *fakeRuntime) start(image string) (Process, error) {
	b, err := os.ReadFile(image)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.started = append(r.started, string(b))
	r.mu.Unlock()
	p := &fakeProc{done: make(chan struct{})}
	if string(b) == "crash" {
		p.Stop(0)
	}
	return p, nil
}

func (r *fakeRuntime) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.started) == 0 {
		return ""
	}
	return r.started[len(r.started)-1]
}

func newSup(t *testing.T, dir string, rt *fakeRuntime) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(SupervisorOptions{
		DataDir:      dir,
		Start:        rt.start,
		Verify:       func(context.Context, string) error { return nil },
		Health:       Health{Grace: 20 * time.Millisecond},
		RestartDelay: time.Hour, // keep crashed containers from restarting mid-test
		Log:          log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

func stage(t *testing.T, dir, content string) string {
	t.Helper()
	f, err := os.CreateTemp(TmpDir(dir), "push-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func TestInstallAndRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)

	if err := s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(ctx, stage(t, dir, "v2"), Release{Version: "v2", SHA256: "s2", Serial: 7}, false); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Current.Version != "v2" || st.Previous.Version != "v1" || st.SeenSerial != 7 {
		t.Fatalf("after v2: %+v", st)
	}

	err := s.Install(ctx, stage(t, dir, "crash"), Release{Version: "v3", SHA256: "s3"}, false)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want rollback, got %v", err)
	}
	st = s.State()
	if st.Current.Version != "v2" || st.Previous.Version != "v1" || st.Trial != nil {
		t.Fatalf("rollback did not restore v2/v1: %+v", st)
	}
	if _, ok := st.Bad["s3"]; !ok {
		t.Fatal("failed release not marked bad")
	}
	waitFor(t, func() bool { return rt.last() == "v2" })

	// The failed image file is garbage collected; v1 and v2 are kept.
	ents, _ := os.ReadDir(filepath.Join(dir, "images"))
	if len(ents) != 2 {
		t.Fatalf("want 2 images kept, got %d", len(ents))
	}

	// A known-bad image is refused unless forced.
	if err := s.Install(ctx, stage(t, dir, "crash"), Release{Version: "v3", SHA256: "s3"}, false); !errors.Is(err, ErrKnownBad) {
		t.Fatalf("want ErrKnownBad, got %v", err)
	}
}

func TestVerifyFailureLeavesCurrentRunning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false)
	s.o.Verify = func(context.Context, string) error { return errors.New("bad signature") }
	if err := s.Install(ctx, stage(t, dir, "evil"), Release{Version: "evil", SHA256: "e"}, false); err == nil {
		t.Fatal("unverified image installed")
	}
	if s.State().Current.Version != "v1" || len(rt.started) != 1 {
		t.Fatal("verification failure disturbed the running container")
	}
}

func TestBootResumesInterruptedTrial(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false)
	s.Shutdown()

	// Simulate power loss while v2 was on trial.
	v2 := filepath.Join(dir, "images", "s2.sif")
	os.WriteFile(v2, []byte("v2"), 0o600)
	st := s.State()
	st.Trial = &Release{Version: "v2", SHA256: "s2", Path: v2, Attempts: 1}
	saveState(filepath.Join(dir, "state.json"), st)

	s2 := newSup(t, dir, rt)
	if err := s2.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s2.State(); got.Current.Version != "v2" || got.Previous.Version != "v1" || got.Trial != nil {
		t.Fatalf("trial not completed on boot: %+v", got)
	}
}

func TestBootAbandonsTrialAfterRepeatedReboots(t *testing.T) {
	dir := t.TempDir()
	rt := &fakeRuntime{}
	os.MkdirAll(filepath.Join(dir, "images"), 0o700)
	v1 := filepath.Join(dir, "images", "s1.sif")
	os.WriteFile(v1, []byte("v1"), 0o600)
	saveState(filepath.Join(dir, "state.json"), State{
		Current: &Release{Version: "v1", SHA256: "s1", Path: v1},
		Trial:   &Release{Version: "v2", SHA256: "s2", Attempts: maxTrialAttempts},
	})
	s := newSup(t, dir, rt)
	if err := s.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Current.Version != "v1" || st.Trial != nil {
		t.Fatalf("got %+v", st)
	}
	if _, ok := st.Bad["s2"]; !ok {
		t.Fatal("repeatedly interrupted trial not marked bad")
	}
}

func TestBootFallsBackToPrevious(t *testing.T) {
	dir := t.TempDir()
	rt := &fakeRuntime{}
	os.MkdirAll(filepath.Join(dir, "images"), 0o700)
	v1 := filepath.Join(dir, "images", "s1.sif")
	v2 := filepath.Join(dir, "images", "s2.sif")
	os.WriteFile(v1, []byte("v1"), 0o600)
	os.WriteFile(v2, []byte("crash"), 0o600)
	saveState(filepath.Join(dir, "state.json"), State{
		Current:  &Release{Version: "v2", SHA256: "s2", Path: v2},
		Previous: &Release{Version: "v1", SHA256: "s1", Path: v1},
	})
	s := newSup(t, dir, rt)
	s.Boot(context.Background())
	if st := s.State(); st.Current.Version != "v1" || st.Previous != nil {
		t.Fatalf("got %+v", st)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
