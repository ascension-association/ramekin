package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	procs   []*fakeProc
}

func (r *fakeRuntime) lastProc() *fakeProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[len(r.procs)-1]
}

func (r *fakeRuntime) start(image string) (Process, error) {
	b, err := os.ReadFile(image)
	if err != nil {
		return nil, err
	}
	p := &fakeProc{done: make(chan struct{})}
	r.mu.Lock()
	r.started = append(r.started, string(b))
	r.procs = append(r.procs, p)
	r.mu.Unlock()
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
	if err := s.Install(ctx, stage(t, dir, "v2"), Release{Version: "v2", SHA256: "s2", Serial: 7, Source: "channel:stable"}, false); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Current.Version != "v2" || st.Previous.Version != "v1" || st.SeenSerials["stable"] != 7 {
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

func TestRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	if err := s.Rollback(ctx); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("want ErrNoPrevious, got %v", err)
	}
	s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false)
	s.Install(ctx, stage(t, dir, "v2"), Release{Version: "v2", SHA256: "s2", Serial: 7, Source: "channel:stable"}, false)
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.State()
	if st.Current.Version != "v1" || st.Previous.Version != "v2" || st.Trial != nil || st.SeenSerials["stable"] != 7 {
		t.Fatalf("after rollback: %+v", st)
	}
	waitFor(t, func() bool { return rt.last() == "v1" && s.App().Running })
	// Rolling back again returns to v2: the two releases swap.
	if err := s.Rollback(ctx); err != nil || s.State().Current.Version != "v2" {
		t.Fatalf("second rollback: %v %+v", err, s.State())
	}

	// A previous release that no longer starts is rejected, and the current one keeps running.
	os.WriteFile(s.State().Previous.Path, []byte("crash"), 0o600)
	if err := s.Rollback(ctx); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if st := s.State(); st.Current.Version != "v2" || st.Trial != nil {
		t.Fatalf("failed rollback changed releases: %+v", st)
	}
	waitFor(t, func() bool { return rt.last() == "v2" })
}

func TestStateDuringTrial(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	s.o.Health.Grace = 300 * time.Millisecond
	done := make(chan error)
	go func() { done <- s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false) }()
	// State and App answer while the trial holds the supervisor's lock.
	waitFor(t, func() bool { return s.State().Trial != nil })
	if s.App().Running {
		t.Fatal("app reported running before its trial passed")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a := s.App(); return a.Running && a.Restarts == 0 })
}

func TestAppStatusCountsExits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	s.o.RestartDelay = time.Millisecond
	s.Install(ctx, stage(t, dir, "v1"), Release{Version: "v1", SHA256: "s1"}, false)
	waitFor(t, func() bool { return s.App().Running })
	rt.lastProc().Stop(0) // the container exits on its own
	waitFor(t, func() bool { a := s.App(); return a.Restarts == 1 && a.Running && a.LastExit == "exit status 1" })
}

func TestSwitchToKeptRelease(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt := &fakeRuntime{}
	s := newSup(t, dir, rt)
	for _, v := range []string{"v1", "v2", "v3", "v4", "v5"} {
		if err := s.Install(ctx, stage(t, dir, v), Release{Version: v, SHA256: "s" + v}, false); err != nil {
			t.Fatal(err)
		}
	}
	versions := func() (out []string) {
		st := s.State()
		out = append(out, st.Current.Version)
		for _, r := range st.History {
			out = append(out, r.Version)
		}
		return out
	}
	// The three most recent successful releases: the current one and two more.
	if got := strings.Join(versions(), " "); got != "v5 v4 v3" {
		t.Fatalf("kept %s", got)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "images")); len(ents) != 3 {
		t.Fatalf("want 3 images kept, got %d", len(ents))
	}
	if err := s.SwitchTo(ctx, "sv2"); !errors.Is(err, ErrNotKept) {
		t.Fatalf("switched to a forgotten release: %v", err)
	}
	// Switching to the oldest kept release puts the current one at the front.
	if err := s.SwitchTo(ctx, "sv3"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions(), " "); got != "v3 v5 v4" || s.State().Previous.Version != "v5" {
		t.Fatalf("after switch: %s", got)
	}
	waitFor(t, func() bool { return rt.last() == "v3" })

	// A kept release that fails its health check is dropped from History.
	os.WriteFile(filepath.Join(dir, "images", "sv4.sif"), []byte("crash"), 0o600)
	if err := s.SwitchTo(ctx, "sv4"); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if got := strings.Join(versions(), " "); got != "v3 v5" {
		t.Fatalf("after failed switch: %s", got)
	}
}

func TestHistoryMigratesFromPrevious(t *testing.T) {
	dir := t.TempDir()
	saveState(filepath.Join(dir, "state.json"), State{
		Current:  &Release{Version: "v2", SHA256: "s2"},
		Previous: &Release{Version: "v1", SHA256: "s1"},
	})
	st, err := loadState(filepath.Join(dir, "state.json"))
	if err != nil || len(st.History) != 1 || st.History[0].Version != "v1" {
		t.Fatalf("got %+v %v", st.History, err)
	}
}

func TestBootTrimsHistory(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "images"), 0o700)
	var hist []*Release
	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		p := filepath.Join(dir, "images", "s"+v+".sif")
		os.WriteFile(p, []byte(v), 0o600)
		hist = append([]*Release{{Version: v, SHA256: "s" + v, Path: p}}, hist...)
	}
	// Written by an agent that kept three earlier releases.
	saveState(filepath.Join(dir, "state.json"), State{Current: hist[0], History: hist[1:]})
	s := newSup(t, dir, &fakeRuntime{})
	if err := s.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := s.State().History; len(h) != KeepReleases || h[1].Version != "v2" {
		t.Fatalf("history %+v", h)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "images")); len(ents) != KeepReleases+1 {
		t.Fatalf("want %d images, got %d", KeepReleases+1, len(ents))
	}
}

func TestBootHoldsRollbackOnFirmwareTestBoot(t *testing.T) {
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
	s.HoldRollback()
	s.Boot(context.Background())
	st := s.State()
	if st.Current.Version != "v2" || len(st.Bad) != 0 || s.BootHealthy() {
		t.Fatalf("app rolled back or reported healthy: %+v healthy=%v", st, s.BootHealthy())
	}
}
