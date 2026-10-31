package container

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylabs/sif/v2/pkg/sif"

	"github.com/ascension-association/ramekin/internal/config"
)

func TestMain(m *testing.M) {
	MaybeInit() // the runtime re-executes this test binary as container init
	os.Exit(m.Run())
}

const workload = `package main

import (
	"fmt"
	"os"
)

func main() {
	host, _ := os.Hostname()
	_, procErr := os.Stat("/proc/self/status")
	// Writes to the image land in the tmpfs overlay; /data is persistent.
	if err := os.WriteFile("/etc/scratch", []byte("x"), 0o644); err != nil {
		fmt.Println("overlay not writable:", err)
		os.Exit(1)
	}
	msg := fmt.Sprintf("host=%s pid=%d proc=%v arg=%s", host, os.Getpid(), procErr == nil, os.Args[1])
	if err := os.WriteFile("/data/result", []byte(msg), 0o644); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
`

// TestRunSIF exercises the real loop/squashfs/overlay/pivot_root path.
// Run with: sudo go test ./internal/container
func TestRunSIF(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root (sudo go test ./internal/container)")
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("needs mksquashfs")
	}
	if err := Preflight(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755)

	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte(workload), 0o644)
	build := exec.Command("go", "build", "-o", filepath.Join(rootfs, "app"), src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workload: %v\n%s", err, out)
	}
	sq := filepath.Join(dir, "root.squashfs")
	if out, err := exec.Command("mksquashfs", rootfs, sq, "-noappend", "-quiet").CombinedOutput(); err != nil {
		t.Fatalf("mksquashfs: %v\n%s", err, out)
	}
	sqf, _ := os.Open(sq)
	defer sqf.Close()
	di, err := sif.NewDescriptorInput(sif.DataPartition, sqf, sif.OptPartitionMetadata(sif.FsSquash, sif.PartPrimSys, runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "test.sif")
	f, err := sif.CreateContainerAtPath(image, sif.OptCreateWithDescriptors(di))
	if err != nil {
		t.Fatal(err)
	}
	f.UnloadContainer()

	data := filepath.Join(dir, "data")
	var out bytes.Buffer
	rt := &Runtime{
		Cfg: config.Container{
			Hostname: "ramekin-test", Command: []string{"/app", "hello"},
			Binds: []string{"data:/data"}, TmpfsSizeMB: 16,
		},
		DataDir: dir,
		Output:  &out,
	}
	p, err := rt.Start(image)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		p.Stop(time.Second)
		t.Fatal("container did not exit")
	}
	if p.Err() != nil {
		t.Fatalf("container failed: %v\n%s", p.Err(), out.String())
	}
	got, err := os.ReadFile(filepath.Join(data, "result"))
	if err != nil {
		t.Fatalf("no result: %v\n%s", err, out.String())
	}
	if want := "host=ramekin-test pid=1 proc=true arg=hello"; !strings.Contains(string(got), want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "etc", "scratch")); err == nil {
		t.Fatal("container write leaked into the image")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin", "HOME=/root"}, []string{"PATH=/usr/games:/bin", "LC_ALL=C"})
	want := []string{"PATH=/usr/games:/bin", "HOME=/root", "LC_ALL=C"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v", got)
	}
}
