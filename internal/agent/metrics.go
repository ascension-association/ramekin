package agent

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is a snapshot of the device's resource use, reported in its status.
type Metrics struct {
	CPUPercent float64 `json:"cpu_percent"` // all cores, over the last sample interval
	MemUsed    uint64  `json:"mem_used_bytes"`
	MemTotal   uint64  `json:"mem_total_bytes"`
	DiskUsed   uint64  `json:"disk_used_bytes"` // the filesystem holding the data directory
	DiskTotal  uint64  `json:"disk_total_bytes"`
	UptimeSec  float64 `json:"uptime_seconds"`
}

// Sampler measures CPU use in the background (it needs two readings of
// /proc/stat) and reads memory and disk use on demand.
type Sampler struct {
	Proc string // default /proc
	Disk string // path on the filesystem to report

	mu          sync.Mutex
	cpu         float64
	total, idle uint64
}

func (m *Sampler) proc() string {
	if m.Proc != "" {
		return m.Proc
	}
	return "/proc"
}

// Run samples CPU use every interval until ctx is done.
func (m *Sampler) Run(ctx context.Context, interval time.Duration) {
	for {
		m.sampleCPU()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (m *Sampler) sampleCPU() {
	b, err := os.ReadFile(m.proc() + "/stat")
	if err != nil {
		return
	}
	total, idle, ok := parseCPU(b)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if dt := total - m.total; m.total != 0 && total > m.total {
		m.cpu = 100 * float64(dt-min(idle-m.idle, dt)) / float64(dt)
	}
	m.total, m.idle = total, idle
}

// Snapshot returns current metrics; fields that can't be read are zero.
func (m *Sampler) Snapshot() *Metrics {
	if m == nil {
		return nil
	}
	var r Metrics
	m.mu.Lock()
	r.CPUPercent = m.cpu
	m.mu.Unlock()
	if b, err := os.ReadFile(m.proc() + "/meminfo"); err == nil {
		r.MemTotal, r.MemUsed = parseMeminfo(b)
	}
	if b, err := os.ReadFile(m.proc() + "/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			r.UptimeSec, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if m.Disk != "" {
		r.DiskTotal, r.DiskUsed = diskUsage(m.Disk)
	}
	return &r
}

// parseCPU sums the aggregate "cpu" line of /proc/stat. Idle includes
// iowait; guest time is already counted in user and nice.
func parseCPU(b []byte) (total, idle uint64, ok bool) {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, s := range f[1:min(len(f), 9)] { // user nice system idle iowait irq softirq steal
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle, true
}

// parseMeminfo returns total memory and memory in use (total minus
// MemAvailable, which counts reclaimable caches as free).
func parseMeminfo(b []byte) (total, used uint64) {
	var avail uint64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(f[0], 10, 64)
		switch k {
		case "MemTotal":
			total = n * 1024
		case "MemAvailable":
			avail = n * 1024
		}
	}
	return total, total - min(avail, total)
}
