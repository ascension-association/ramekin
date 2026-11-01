package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFakeProc(t *testing.T, root string, pid, stat, cmdline, uid, mntNS, exe string) {
	t.Helper()
	d := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(d, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644)
	os.WriteFile(filepath.Join(d, "status"), []byte("Name:\tx\nUid:\t"+uid+"\t"+uid+"\t"+uid+"\t"+uid+"\n"), 0o644)
	if mntNS != "" {
		os.Symlink(mntNS, filepath.Join(d, "ns", "mnt"))
	}
	if exe != "" {
		os.Symlink(exe, filepath.Join(d, "exe"))
	}
}

// stat builds a /proc/<pid>/stat line; flags is field 9, utime/stime 14/15,
// threads 20, rss 24 (pages).
func stat(pid, comm, state, ppid, flags string) string {
	return pid + " (" + comm + ") " + state + " " + ppid + " 1 1 0 -1 " + flags +
		" 0 0 0 0 250 50 0 0 20 0 3 0 100 1000000 256 18446744073709551615\n"
}

func TestProcesses(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "self", "ns"), 0o755)
	os.Symlink("mnt:[1]", filepath.Join(root, "self", "ns", "mnt"))
	writeFakeProc(t, root, "1", stat("1", "init", "S", "0", "4194560"), "/gokrazy/init\x00", "0", "mnt:[1]", "/gokrazy/init")
	writeFakeProc(t, root, "2", stat("2", "kthreadd", "S", "0", "2129984"), "", "0", "", "")
	writeFakeProc(t, root, "40", stat("40", "a) b (c", "R", "1", "4194560"), "/bin/sh\x00/usr/local/bin/app\x00", "1000", "mnt:[9]", "/bin/busybox")
	os.MkdirAll(filepath.Join(root, "net"), 0o755)

	ps, err := Processes(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].PID != 1 || ps[1].PID != 40 {
		t.Fatalf("got %+v", ps)
	}
	app := ps[1]
	if app.Name != "a) b (c" || app.State != "R" || app.PPID != 1 || app.UID != 1000 {
		t.Errorf("app: %+v", app)
	}
	if !app.Container || ps[0].Container {
		t.Errorf("container: init %v, app %v", ps[0].Container, app.Container)
	}
	if app.Exe != "/bin/busybox" || len(app.Args) != 2 || app.Args[1] != "/usr/local/bin/app" {
		t.Errorf("exe/args: %q %q", app.Exe, app.Args)
	}
	if app.CPUSeconds != 3 || app.Threads != 3 || app.RSSBytes != 256*pageSize {
		t.Errorf("cpu %v threads %d rss %d", app.CPUSeconds, app.Threads, app.RSSBytes)
	}

	all, _ := Processes(root, true)
	if len(all) != 3 || !all[1].Kernel {
		t.Fatalf("with kernel threads: %+v", all)
	}
}

func TestProcessesLive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	ps, err := Processes("/proc", false)
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	for _, p := range ps {
		if p.PID == self {
			if p.Kernel || p.Container || p.Exe == "" || p.RSSBytes == 0 {
				t.Errorf("self: %+v", p)
			}
			return
		}
	}
	t.Errorf("own process %d not listed among %d", self, len(ps))
}
