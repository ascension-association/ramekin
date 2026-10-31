package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Process is a running container.
type Process interface {
	Done() <-chan struct{}
	Err() error // valid once Done is closed
	Stop(timeout time.Duration)
}

type StartFunc func(image string) (Process, error)

var (
	ErrRolledBack = errors.New("new release failed and was rolled back")
	ErrKnownBad   = errors.New("release previously failed on this device")
	ErrNoPrevious = errors.New("no previous release to roll back to")
	ErrNotKept    = errors.New("release is not among this device's earlier releases")
)

const maxTrialAttempts = 3

type SupervisorOptions struct {
	DataDir      string
	Start        StartFunc
	Verify       func(ctx context.Context, image string) error
	Health       Health
	StopTimeout  time.Duration
	RestartDelay time.Duration
	Log          *log.Logger
	// OnChange is called (without locks held) after every state transition.
	OnChange func(State)
}

type Supervisor struct {
	o SupervisorOptions

	mu    sync.Mutex // serializes Boot/Install/Restart and guards state, run
	state State
	run   *runner

	// snap is a copy of state taken at every save, so State never waits for
	// a trial that holds mu.
	smu  sync.Mutex
	snap State
	app  AppStatus
}

// AppStatus is whether the current release's container is up.
type AppStatus struct {
	Running  bool      `json:"running"`
	Since    time.Time `json:"since,omitzero"` // when it last started or stopped
	Restarts int       `json:"restarts"`       // exits since the release was started
	LastExit string    `json:"last_exit,omitempty"`
}

func NewSupervisor(o SupervisorOptions) (*Supervisor, error) {
	if o.Log == nil {
		o.Log = log.Default()
	}
	for _, d := range []string{o.DataDir, imagesDir(o.DataDir), TmpDir(o.DataDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := loadState(filepath.Join(o.DataDir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if st.Bad == nil {
		st.Bad = map[string]BadRelease{}
	}
	return &Supervisor{o: o, state: st, snap: st.clone()}, nil
}

func imagesDir(dataDir string) string { return filepath.Join(dataDir, "images") }

// TmpDir is on the same filesystem as the image store so installs can rename.
func TmpDir(dataDir string) string { return filepath.Join(dataDir, "tmp") }

func (s *Supervisor) State() State {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.snap.clone()
}

// App reports whether the container is running.
func (s *Supervisor) App() AppStatus {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.app
}

func (s *Supervisor) setApp(f func(*AppStatus)) {
	s.smu.Lock()
	defer s.smu.Unlock()
	f(&s.app)
}

// Boot resumes after power-on: it finishes or abandons an interrupted trial,
// then starts the current release, falling back to the previous one if the
// current release no longer passes its health check.
func (s *Supervisor) Boot(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.RemoveAll(TmpDir(s.o.DataDir))
	os.MkdirAll(TmpDir(s.o.DataDir), 0o700)
	s.gc() // drops images of releases no longer kept

	if t := s.state.Trial; t != nil {
		if t.Attempts >= maxTrialAttempts {
			s.o.Log.Printf("release %s was on trial for %d boots without passing; marking bad", t.Version, t.Attempts)
			s.markBad(t, "device rebooted repeatedly during health check")
			s.state.Trial = nil
			s.save()
		} else {
			t.Attempts++
			s.save()
			s.o.Log.Printf("resuming interrupted trial of %s (attempt %d)", t.Version, t.Attempts)
			cp := *t
			if err := s.trial(ctx, &cp); err != nil {
				s.o.Log.Print(err)
			}
			return nil
		}
	}

	cur := s.state.Current
	if cur == nil {
		s.o.Log.Print("no release installed yet; waiting for a push or channel update")
		return nil
	}
	s.o.Log.Printf("starting %s", cur.Version)
	proc, err := s.startHealthy(ctx, cur)
	if err != nil && s.state.Previous != nil {
		prev := s.state.Previous
		s.o.Log.Printf("current release %s failed at boot (%v); rolling back to %s", cur.Version, err, prev.Version)
		s.markBad(cur, "failed health check at boot: "+err.Error())
		s.state.Current = prev
		s.state.forget(prev.SHA256)
		s.save()
		cur = prev
		proc, err = s.startHealthy(ctx, cur)
	}
	if err != nil {
		s.o.Log.Printf("release %s is unhealthy (%v); will keep restarting it", cur.Version, err)
		proc = nil
	} else {
		s.o.Log.Printf("release %s is healthy", cur.Version)
	}
	s.run = s.supervise(cur, proc)
	return nil
}

// Install verifies the SIF at src, moves it into the image store and runs it
// on trial. On a failed health check the previous container is restored and
// ErrRolledBack is returned.
func (s *Supervisor) Install(ctx context.Context, src string, rel Release, force bool) error {
	// The trial must not be cut short because an HTTP client hung up.
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer os.Remove(src)

	if s.state.markSeen(channelOf(rel), rel.Serial) {
		s.save()
	}
	if b, ok := s.state.Bad[rel.SHA256]; ok && !force {
		return fmt.Errorf("%w: %s (%s)", ErrKnownBad, b.Version, b.Reason)
	}
	if err := s.o.Verify(ctx, src); err != nil {
		return err
	}
	rel.Path = filepath.Join(imagesDir(s.o.DataDir), rel.SHA256+".sif")
	if err := os.Rename(src, rel.Path); err != nil {
		return err
	}
	delete(s.state.Bad, rel.SHA256)
	rel.Attempts = 1
	trial := rel
	s.state.Trial = &trial
	s.save()
	return s.trial(ctx, &rel)
}

// trial must be called with mu held and s.state.Trial set.
func (s *Supervisor) trial(ctx context.Context, rel *Release) error {
	if s.run != nil {
		s.run.stop()
		s.run = nil
	}
	s.o.Log.Printf("starting %s on trial", rel.Version)
	proc, err := s.startHealthy(ctx, rel)
	if err != nil {
		s.markBad(rel, err.Error())
		s.state.forget(rel.SHA256) // a release switched back to no longer counts as good
		s.state.Trial = nil
		s.save()
		s.gc()
		if cur := s.state.Current; cur != nil {
			s.o.Log.Printf("rolling back to %s", cur.Version)
			s.run = s.supervise(cur, nil)
		}
		return fmt.Errorf("%w: %s: %v", ErrRolledBack, rel.Version, err)
	}
	rel.Attempts = 0
	rel.InstalledAt = time.Now().UTC()
	s.state.commit(rel)
	s.state.Trial = nil
	s.save()
	s.gc()
	s.o.Log.Printf("release %s is healthy and committed", rel.Version)
	s.run = s.supervise(rel, proc)
	return nil
}

// Rollback switches to the previous release.
func (s *Supervisor) Rollback(ctx context.Context) error {
	st := s.State()
	if st.Previous == nil || st.Current == nil {
		return ErrNoPrevious
	}
	return s.SwitchTo(ctx, st.Previous.SHA256)
}

// SwitchTo runs one of the device's earlier releases (History, by SHA-256)
// on trial, as if it had just been installed. If it passes its health check,
// it becomes current and the release it replaced moves to the front of
// History; otherwise the current release keeps running and the failed one
// is forgotten. A channel won't reinstall a newer release until it publishes
// a new serial.
func (s *Supervisor) SwitchTo(ctx context.Context, sha string) error {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.state.release(sha)
	if target == nil || s.state.Current == nil {
		return ErrNotKept
	}
	rel := *target
	rel.Attempts = 1
	trial := rel
	s.state.Trial = &trial
	s.save()
	s.o.Log.Printf("switching from %s to %s", s.state.Current.Version, rel.Version)
	return s.trial(ctx, &rel)
}

// Restart stops and restarts the current container.
func (s *Supervisor) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		s.run.stop()
	}
	if s.state.Current != nil {
		s.run = s.supervise(s.state.Current, nil)
	}
}

// MarkSeen records that a channel's manifest with this serial was handled.
func (s *Supervisor) MarkSeen(channel string, serial int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.markSeen(channel, serial) {
		s.save()
	}
}

// ClearBad forgets failed releases so they may be retried.
func (s *Supervisor) ClearBad() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Bad = map[string]BadRelease{}
	s.save()
}

// Shutdown stops the container.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run != nil {
		s.run.stop()
		s.run = nil
	}
}

func (s *Supervisor) startHealthy(ctx context.Context, rel *Release) (Process, error) {
	proc, err := s.o.Start(rel.Path)
	if err != nil {
		return nil, err
	}
	if err := s.o.Health.Check(ctx, proc); err != nil {
		proc.Stop(s.o.StopTimeout)
		return nil, err
	}
	return proc, nil
}

func (s *Supervisor) markBad(rel *Release, reason string) {
	s.state.Bad[rel.SHA256] = BadRelease{Version: rel.Version, Reason: reason, At: time.Now().UTC()}
}

func (s *Supervisor) save() {
	s.smu.Lock()
	s.snap = s.state.clone()
	s.smu.Unlock()
	if err := saveState(filepath.Join(s.o.DataDir, "state.json"), s.state); err != nil {
		s.o.Log.Printf("saving state: %v", err)
	}
	if s.o.OnChange != nil {
		st := s.state.clone()
		go s.o.OnChange(st)
	}
}

// gc deletes images that are neither current, kept in History, nor on trial.
func (s *Supervisor) gc() {
	keep := map[string]bool{}
	for _, r := range append([]*Release{s.state.Current, s.state.Trial}, s.state.History...) {
		if r != nil {
			keep[r.SHA256+".sif"] = true
		}
	}
	ents, _ := os.ReadDir(imagesDir(s.o.DataDir))
	for _, e := range ents {
		if !keep[e.Name()] && strings.HasSuffix(e.Name(), ".sif") {
			os.Remove(filepath.Join(imagesDir(s.o.DataDir), e.Name()))
		}
	}
}

// runner keeps one release running, restarting it with backoff when it exits.
type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Supervisor) supervise(rel *Release, proc Process) *runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan struct{})}
	base := s.o.RestartDelay
	if base == 0 {
		base = 2 * time.Second
	}
	s.setApp(func(a *AppStatus) { *a = AppStatus{} })
	go func() {
		defer close(r.done)
		defer s.setApp(func(a *AppStatus) { a.Running, a.Since = false, time.Now().UTC() })
		delay := base
		for {
			if proc == nil {
				p, err := s.o.Start(rel.Path)
				if err != nil {
					s.o.Log.Printf("start %s: %v", rel.Version, err)
					s.setApp(func(a *AppStatus) { a.LastExit = "start: " + err.Error() })
				}
				proc = p
			}
			started := time.Now()
			if proc != nil {
				s.setApp(func(a *AppStatus) { a.Running, a.Since = true, started.UTC() })
				select {
				case <-ctx.Done():
					proc.Stop(s.o.StopTimeout)
					return
				case <-proc.Done():
					s.o.Log.Printf("container %s exited: %v", rel.Version, proc.Err())
					s.setApp(func(a *AppStatus) {
						a.Running, a.Since = false, time.Now().UTC()
						a.Restarts++
						a.LastExit = fmt.Sprint(proc.Err())
					})
				}
				proc = nil
			}
			if time.Since(started) > 5*time.Minute {
				delay = base
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, 5*time.Minute)
		}
	}()
	return r
}

func (r *runner) stop() {
	r.cancel()
	<-r.done
}
