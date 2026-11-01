package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ProcInfo is one process running on the device, as GET /v1/processes
// reports it.
type ProcInfo struct {
	PID  int `json:"pid"`
	PPID int `json:"ppid"`
	UID  int `json:"uid"`
	// Name is the kernel's short name for it; Exe the executable it runs
	// (as seen from its own root, so a container's paths are its own).
	Name  string   `json:"name"`
	Exe   string   `json:"exe,omitempty"`
	Args  []string `json:"args,omitempty"`
	State string   `json:"state"`
	// Container is true for processes in the app's container (another
	// mount namespace than ramekind's).
	Container  bool    `json:"container,omitempty"`
	Kernel     bool    `json:"kernel,omitempty"`
	Threads    int     `json:"threads"`
	RSSBytes   uint64  `json:"rss_bytes"`
	CPUSeconds float64 `json:"cpu_seconds"`
}

// ProcessList is the answer to GET /v1/processes.
type ProcessList struct {
	Device string `json:"device"`
	Nonce  string `json:"nonce,omitempty"`
	// App is the version of the app (SIF) whose processes are marked
	// container.
	App       string     `json:"app,omitempty"`
	Processes []ProcInfo `json:"processes"`
}

const (
	clockTicks = 100 // USER_HZ on every Linux architecture gokrazy runs on
	pageSize   = 4096
)

// Processes lists the processes under procRoot (/proc), leaving out kernel
// threads unless kernel is true. It skips processes that exit while it
// reads them.
func Processes(procRoot string, kernel bool) ([]ProcInfo, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	selfNS, _ := os.Readlink(filepath.Join(procRoot, "self", "ns", "mnt"))
	var out []ProcInfo
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, ok := readProcess(filepath.Join(procRoot, e.Name()), pid, selfNS)
		if !ok || (p.Kernel && !kernel) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

func readProcess(dir string, pid int, selfNS string) (ProcInfo, bool) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return ProcInfo{}, false
	}
	p := ProcInfo{PID: pid}
	// pid (comm) state ppid ...: comm may hold spaces and parentheses.
	open, end := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return ProcInfo{}, false
	}
	p.Name = string(stat[open+1 : end])
	f := strings.Fields(string(stat[end+1:]))
	// f[0] is field 3 (state); utime and stime are fields 14 and 15,
	// num_threads 20, rss 24.
	if len(f) < 22 {
		return ProcInfo{}, false
	}
	p.State = f[0]
	p.PPID, _ = strconv.Atoi(f[1])
	utime, _ := strconv.ParseUint(f[11], 10, 64)
	stime, _ := strconv.ParseUint(f[12], 10, 64)
	p.CPUSeconds = float64(utime+stime) / clockTicks
	p.Threads, _ = strconv.Atoi(f[17])
	rss, _ := strconv.ParseUint(f[21], 10, 64)
	p.RSSBytes = rss * pageSize
	if flags, err := strconv.ParseUint(f[6], 10, 64); err == nil && flags&0x00200000 != 0 { // PF_KTHREAD
		p.Kernel = true
	}
	if pid == 2 || p.PPID == 2 {
		p.Kernel = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "Uid:"); ok {
				if fs := strings.Fields(v); len(fs) > 0 {
					p.UID, _ = strconv.Atoi(fs[0])
				}
			}
		}
	}
	if p.Kernel {
		return p, true
	}
	p.Exe, _ = os.Readlink(filepath.Join(dir, "exe"))
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil && len(b) > 0 {
		p.Args = strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	}
	if ns, err := os.Readlink(filepath.Join(dir, "ns", "mnt")); err == nil && selfNS != "" && ns != selfNS {
		p.Container = true
	}
	return p, true
}
