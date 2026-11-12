// Package container runs a SIF image natively: the agent re-executes itself
// in fresh mount/PID/IPC/UTS namespaces, loop-mounts the SIF's squashfs
// partition, layers a size-limited tmpfs over it with overlayfs, pivots into
// it and executes the image's runscript. No apptainer binary, shell, or
// other helper executables are needed on the host.
//
// Isolation is for packaging, not security: the container runs as root with
// the host network and (by default) host /dev, which is what IoT workloads
// talking to GPIO, serial or USB devices generally need.
package container

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

const (
	initArg = "__ramekin_container_init"
	specEnv = "RAMEKIN_CONTAINER_SPEC"
)

// spec is passed from the agent to the namespaced init process.
type spec struct {
	Image       string
	Offset      int64
	Size        int64
	StateDir    string
	Hostname    string
	Command     []string
	Env         []string
	Binds       []config.Bind
	TmpfsSizeMB int
	// HostFilesDir is the directory mirroring the host's resolv.conf and
	// hosts (see hostMirror).
	HostFilesDir string
}

// MaybeInit must be called first thing in main. When the process was started
// as a container init it sets up the container and execs the workload; it
// never returns in that case.
func MaybeInit() {
	if len(os.Args) < 2 || os.Args[1] != initArg {
		return
	}
	var s spec
	if err := json.Unmarshal([]byte(os.Getenv(specEnv)), &s); err != nil {
		fmt.Fprintf(os.Stderr, "ramekin: bad container spec: %v\n", err)
		os.Exit(127)
	}
	err := setupAndExec(&s) // only returns on failure
	fmt.Fprintf(os.Stderr, "ramekin: container init: %v\n", err)
	os.Exit(127)
}

type Runtime struct {
	Cfg     config.Container
	DataDir string
	Output  io.Writer // receives container stdout and stderr
}

// Start launches image and returns once the container process exists.
func (r *Runtime) Start(image string) (*Process, error) {
	if err := Preflight(); err != nil {
		return nil, err
	}
	rootfs, err := sifimg.PrimaryRootFS(image)
	if err != nil {
		return nil, err
	}
	s := spec{
		Image:       image,
		Offset:      rootfs.Offset,
		Size:        rootfs.Size,
		Hostname:    r.Cfg.Hostname,
		Command:     r.Cfg.Command,
		TmpfsSizeMB: r.Cfg.TmpfsSizeMB,
	}
	for k, v := range r.Cfg.Env {
		s.Env = append(s.Env, k+"="+v)
	}
	for _, b := range r.Cfg.Binds {
		bind, err := config.ParseBind(b)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(bind.Src) {
			// Relative sources are persistent per-device data directories.
			bind.Src = filepath.Join(r.DataDir, bind.Src)
			if err := os.MkdirAll(bind.Src, 0o755); err != nil {
				return nil, err
			}
		}
		s.Binds = append(s.Binds, bind)
	}
	if err := defaultHostMirror.ensure(); err != nil {
		return nil, fmt.Errorf("mirroring host files: %w", err)
	}
	s.HostFilesDir = defaultHostMirror.dir
	if s.StateDir, err = os.MkdirTemp("", "ramekin-ctr-"); err != nil {
		return nil, err
	}
	js, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("/proc/self/exe", initArg)
	cmd.Env = []string{specEnv + "=" + string(js)}
	cmd.Stdout, cmd.Stderr = r.Output, r.Output
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		Pdeathsig:  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		os.Remove(s.StateDir)
		return nil, fmt.Errorf("start container: %w", err)
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		// All mounts lived in the container's namespace and vanished with it.
		os.Remove(s.StateDir)
		close(p.done)
	}()
	return p, nil
}

type Process struct {
	cmd      *exec.Cmd
	done     chan struct{}
	err      error
	stopOnce sync.Once
}

func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the exit error once Done is closed.
func (p *Process) Err() error { return p.err }

func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Stop sends SIGTERM and escalates to SIGKILL after timeout. Note that a
// PID-namespace init ignores SIGTERM unless the workload installs a handler.
func (p *Process) Stop(timeout time.Duration) {
	p.stopOnce.Do(func() {
		p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(timeout):
			p.cmd.Process.Kill()
			<-p.done
		}
	})
	<-p.done
}
