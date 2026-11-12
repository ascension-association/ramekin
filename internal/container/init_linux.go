package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ascension-association/ramekin/internal/config"
	"golang.org/x/sys/unix"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// launcher sources the apptainer environment scripts baked into the SIF and
// runs its runscript, mirroring `apptainer run`.
const launcher = `for f in /.singularity.d/env/*.sh; do [ -r "$f" ] && . "$f"; done
exec /.singularity.d/runscript "$@"`

// Preflight loads (gokrazy has no udev/modprobe) and checks the kernel
// features the runtime depends on.
func Preflight() error {
	for _, m := range []string{"loop", "squashfs", "overlay"} {
		loadModule(m)
	}
	fss, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return err
	}
	var missing []string
	for _, fs := range []string{"squashfs", "overlay", "tmpfs"} {
		if !bytes.Contains(fss, []byte("\t"+fs+"\n")) {
			missing = append(missing, fs)
		}
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		missing = append(missing, "loop (CONFIG_BLK_DEV_LOOP)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("kernel lacks required features: %s", strings.Join(missing, ", "))
	}
	return nil
}

func setupAndExec(s *spec) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	if err := unix.Mount("tmpfs", s.StateDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV,
		fmt.Sprintf("size=%dm,mode=0700", s.TmpfsSizeMB)); err != nil {
		return fmt.Errorf("mount state tmpfs: %w", err)
	}
	lower := filepath.Join(s.StateDir, "lower")
	upper := filepath.Join(s.StateDir, "upper")
	work := filepath.Join(s.StateDir, "work")
	root := filepath.Join(s.StateDir, "root")
	for _, d := range []string{lower, upper, work, root} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}

	dev, loop, err := attachLoop(s.Image, s.Offset, s.Size)
	if err != nil {
		return err
	}
	err = unix.Mount(dev, lower, "squashfs", unix.MS_RDONLY|unix.MS_NODEV, "")
	loop.Close() // the mount holds the device now; autoclear detaches it on unmount
	if err != nil {
		return fmt.Errorf("mount squashfs from %s: %w", dev, err)
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}

	if err := populate(root, s); err != nil {
		return err
	}
	if s.Hostname != "" {
		if err := unix.Sethostname([]byte(s.Hostname)); err != nil {
			return err
		}
	}

	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}

	env := mergeEnv([]string{"PATH=" + defaultPath, "HOME=/root", "container=ramekin"}, s.Env)
	argv := []string{"/bin/sh", "-c", launcher, "ramekin"}
	switch {
	case len(s.Command) > 0:
		argv = s.Command
	case !hasShell():
		// No shell in the image (distroless, scratch): read its environment
		// and runscript here and run its command directly.
		var err error
		if argv, env, err = ShellFreeLaunch("/", env); err != nil {
			return fmt.Errorf("the image has no /bin/sh, and %w (add a shell to the image, or set its command with ramekin device set --command)", err)
		}
	}
	if !strings.Contains(argv[0], "/") {
		p, err := lookPath("/", argv[0], env)
		if err != nil {
			return err
		}
		argv = append([]string{p}, argv[1:]...)
	}
	return fmt.Errorf("exec %s: %w", argv[0], unix.Exec(argv[0], argv, env))
}

// hasShell reports whether the image (now the root) has a /bin/sh.
func hasShell() bool {
	fi, err := os.Stat("/bin/sh")
	return err == nil && !fi.IsDir()
}

// populate mounts the pseudo-filesystems and binds into the new root.
func populate(root string, s *spec) error {
	type m struct {
		src, dst, fstype string
		flags            uintptr
		data             string
	}
	mounts := []m{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RDONLY, ""},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
	}
	devBound := false
	for _, b := range s.Binds {
		if filepath.Clean(b.Dst) == "/dev" {
			devBound = true
		}
	}
	if !devBound {
		mounts = append(mounts, m{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, ""})
	}
	for _, mt := range mounts {
		dst, err := target(root, mt.dst, true)
		if err != nil {
			return err
		}
		if err := unix.Mount(mt.src, dst, mt.fstype, mt.flags, mt.data); err != nil {
			return fmt.Errorf("mount %s on %s: %w", mt.fstype, mt.dst, err)
		}
	}

	for _, b := range s.Binds {
		if err := bindMount(root, b); err != nil {
			return err
		}
	}
	if s.HostFilesDir != "" {
		return linkHostFiles(root, s.HostFilesDir, s.Binds)
	}
	return nil
}

// bindMount binds b.Src at b.Dst inside root, read-only if b says so.
func bindMount(root string, b config.Bind) error {
	st, err := os.Stat(b.Src)
	if err != nil {
		return fmt.Errorf("bind %s: %w", b.Src, err)
	}
	dst, err := target(root, b.Dst, st.IsDir())
	if err != nil {
		return err
	}
	if err := unix.Mount(b.Src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s to %s: %w", b.Src, b.Dst, err)
	}
	if b.ReadOnly {
		if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", b.Dst, err)
		}
	}
	return nil
}

// target creates rel inside root as a directory or empty file and returns
// the host path. Symlinks are refused (or, for the final component, replaced)
// so an image cannot redirect a mount to a host path before pivot_root.
func target(root, rel string, dir bool) (string, error) {
	p := root
	parts := strings.Split(strings.Trim(filepath.Clean(rel), "/"), "/")
	for i, part := range parts {
		p = filepath.Join(p, part)
		last := i == len(parts)-1
		fi, err := os.Lstat(p)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			if !last {
				return "", fmt.Errorf("mount target %s: path component %s is a symlink", rel, part)
			}
			if err := os.Remove(p); err != nil {
				return "", err
			}
		case err == nil:
			continue
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		if !last || dir {
			if err := os.Mkdir(p, 0o755); err != nil {
				return "", err
			}
		} else if err := os.WriteFile(p, nil, 0o644); err != nil {
			return "", err
		}
	}
	return p, nil
}

// attachLoop binds the byte range [off, off+size) of image to a free,
// read-only, auto-clearing loop device.
func attachLoop(image string, off, size int64) (string, *os.File, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", nil, err
	}
	defer ctl.Close()
	img, err := os.Open(image)
	if err != nil {
		return "", nil, err
	}
	defer img.Close()

	for attempt := 0; attempt < 10; attempt++ {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return "", nil, fmt.Errorf("get free loop device: %w", err)
		}
		dev := fmt.Sprintf("/dev/loop%d", n)
		lf, err := os.OpenFile(dev, os.O_RDONLY, 0)
		if err != nil {
			return "", nil, err
		}
		cfg := unix.LoopConfig{
			Fd: uint32(img.Fd()),
			Info: unix.LoopInfo64{
				Offset:    uint64(off),
				Sizelimit: uint64(size),
				Flags:     unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR,
			},
		}
		copy(cfg.Info.File_name[:len(cfg.Info.File_name)-1], image)
		err = unix.IoctlLoopConfigure(int(lf.Fd()), &cfg)
		if err == nil {
			return dev, lf, nil
		}
		lf.Close()
		if !errors.Is(err, unix.EBUSY) {
			return "", nil, fmt.Errorf("configure %s: %w", dev, err)
		}
		// Raced with another user of the device; try the next free one.
	}
	return "", nil, errors.New("no free loop device")
}

// lookPath finds name on env's PATH inside the filesystem rooted at root
// and returns its path relative to that root.
func lookPath(root, name string, env []string) (string, error) {
	path := defaultPath
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(filepath.Join(root, p)); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in container PATH", name)
}

// Exec runs argv inside the filesystem of the running container whose init
// has the given (host) PID, with the workload's environment, and succeeds
// only if it exits 0. It uses the container's root via /proc/<pid>/root,
// which includes the container's mounts (tmpfs, binds, sockets), but it does
// not join the container's PID namespace.
func Exec(ctx context.Context, pid int, argv []string) error {
	root := fmt.Sprintf("/proc/%d/root", pid)
	env := []string{"PATH=" + defaultPath}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		if vars := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"); len(vars) > 0 && vars[0] != "" {
			env = vars
		}
	}
	path := argv[0]
	if !strings.Contains(path, "/") {
		var err error
		if path, err = lookPath(root, path, env); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Env = env
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// loadModule loads a kernel module from /lib/modules if it is shipped as a
// module rather than built in. Failures are ignored; Preflight reports what
// is actually missing.
func loadModule(name string) {
	var uts unix.Utsname
	if unix.Uname(&uts) != nil {
		return
	}
	dir := filepath.Join("/lib/modules", unix.ByteSliceToString(uts.Release[:]))
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()
		if base != name+".ko" && !strings.HasPrefix(base, name+".ko.") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		flags := 0
		if base != name+".ko" {
			flags = unix.MODULE_INIT_COMPRESSED_FILE
		}
		unix.FinitModule(int(f.Fd()), "", flags)
		return filepath.SkipAll
	})
}

// mergeEnv returns base with entries from override replacing same-named ones,
// since libc getenv returns the first match.
func mergeEnv(base, override []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range override {
		name, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, e := range out {
			if n, _, _ := strings.Cut(e, "="); n == name {
				out[i], replaced = kv, true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}
