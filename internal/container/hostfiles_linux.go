package container

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ascension-association/ramekin/internal/config"
)

// The host's resolv.conf and hosts reach containers through a directory
// ramekind keeps in sync, not by binding the files themselves.
//
// Binding a file pins the file that exists when the container starts. On
// gokrazy, /etc/resolv.conf leads to /proc/net/pnp, which lists no DNS
// servers, until the DHCP client writes /tmp/resolv.conf, and it rewrites
// that file by renaming a new one into place. A container started at boot,
// before the lease, would therefore keep a resolv.conf without nameservers
// for as long as it runs, and one started later would never see a change of
// DNS servers. A bound directory shows the files renamed into it, so the
// container's /etc/resolv.conf, a symlink into it, always has the host's
// current contents.

// hostFilesMount is where containers see the mirror directory.
const hostFilesMount = "/.ramekin/host"

// hostFileNames are the files mirrored from the host's /etc.
var hostFileNames = []string{"resolv.conf", "hosts"}

// hostMirrorInterval is how often the mirror checks the host's files.
const hostMirrorInterval = 2 * time.Second

// hostMirror copies the files hostFileNames from src into dir, replacing
// each copy by renaming a new file into place.
type hostMirror struct {
	src, dir string

	mu   sync.Mutex
	last map[string][]byte // contents written, nil for "removed"

	startOnce sync.Once
	startErr  error
}

var defaultHostMirror = &hostMirror{
	src: "/etc",
	// /tmp is a tmpfs on gokrazy, so the copies never reach permanent
	// storage.
	dir: filepath.Join(os.TempDir(), "ramekin-host"),
}

// ensure syncs the mirror once and starts keeping it in sync. Containers
// may start once it returns nil.
func (m *hostMirror) ensure() error {
	m.startOnce.Do(func() {
		if m.startErr = m.sync(); m.startErr != nil {
			return
		}
		go func() {
			for range time.Tick(hostMirrorInterval) {
				if err := m.sync(); err != nil {
					log.Printf("container: mirroring host files: %v", err)
				}
			}
		}()
	})
	return m.startErr
}

// sync brings the mirror up to date with the host's files. A file missing on
// the host is removed from the mirror.
func (m *hostMirror) sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		m.last = map[string][]byte{}
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	var errs []error
	for _, name := range hostFileNames {
		dst := filepath.Join(m.dir, name)
		b, err := os.ReadFile(filepath.Join(m.src, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			if prev, seen := m.last[name]; seen && prev == nil {
				continue
			}
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			m.last[name] = nil
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if b == nil {
			b = []byte{} // distinguish an empty file from a removed one
		}
		if prev, seen := m.last[name]; seen && prev != nil && bytes.Equal(prev, b) {
			continue
		}
		if err := writeReplace(dst, b); err != nil {
			errs = append(errs, err)
			continue
		}
		m.last[name] = b
	}
	return errors.Join(errs...)
}

// writeReplace writes b to path by renaming a temporary file over it, so
// readers see either the old or the new contents.
func writeReplace(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// linkHostFiles binds the mirror directory read-only at hostFilesMount in
// the container rooted at root, and points the container's /etc files into
// it. Files the configured binds already provide are left to them.
func linkHostFiles(root, mirrorDir string, binds []config.Bind) error {
	var names []string
	for _, name := range hostFileNames {
		if !boundOver(binds, "/etc/"+name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	if err := bindMount(root, config.Bind{Src: mirrorDir, Dst: hostFilesMount, ReadOnly: true}); err != nil {
		return err
	}
	etc, err := target(root, "/etc", true)
	if err != nil {
		return err
	}
	for _, name := range names {
		p := filepath.Join(etc, name)
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if err := os.Symlink(hostFilesMount+"/"+name, p); err != nil {
			return fmt.Errorf("link /etc/%s: %w", name, err)
		}
	}
	return nil
}

// boundOver reports whether a bind's destination is path or a directory
// containing it.
func boundOver(binds []config.Bind, path string) bool {
	for _, b := range binds {
		dst := filepath.Clean(b.Dst)
		if dst == path || dst == "/" || (len(path) > len(dst) && path[:len(dst)] == dst && path[len(dst)] == '/') {
			return true
		}
	}
	return false
}
