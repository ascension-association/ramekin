package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCPU(t *testing.T) {
	total, idle, ok := parseCPU([]byte("cpu  100 5 50 800 45 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8\n"))
	if !ok || total != 1000 || idle != 845 {
		t.Fatalf("got %d %d %v", total, idle, ok)
	}
	if _, _, ok := parseCPU([]byte("intr 1 2 3\n")); ok {
		t.Fatal("parsed a line that isn't the cpu line")
	}
}

func TestParseMeminfo(t *testing.T) {
	total, used := parseMeminfo([]byte("MemTotal:        1000 kB\nMemFree:          100 kB\nMemAvailable:     250 kB\n"))
	if total != 1000*1024 || used != 750*1024 {
		t.Fatalf("got %d %d", total, used)
	}
}

func TestSampler(t *testing.T) {
	proc := t.TempDir()
	write := func(name, s string) { os.WriteFile(filepath.Join(proc, name), []byte(s), 0o600) }
	write("meminfo", "MemTotal: 2048 kB\nMemAvailable: 1024 kB\n")
	write("uptime", "123.45 400.00\n")
	m := &Sampler{Proc: proc, Disk: proc}
	write("stat", "cpu 100 0 100 800 0 0 0 0\n")
	m.sampleCPU()
	write("stat", "cpu 200 0 200 1400 200 0 0 0\n") // 200 busy of 1000
	m.sampleCPU()
	got := m.Snapshot()
	if got.CPUPercent != 20 || got.MemUsed != 1024*1024 || got.UptimeSec != 123.45 || got.DiskTotal == 0 || got.DiskUsed > got.DiskTotal {
		t.Fatalf("got %+v", got)
	}
	if (*Sampler)(nil).Snapshot() != nil {
		t.Fatal("nil sampler reported metrics")
	}
}
