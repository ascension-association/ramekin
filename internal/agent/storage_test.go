package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDisks(t *testing.T) {
	sys := t.TempDir()
	add := func(name string, files map[string]string) {
		for f, v := range files {
			p := filepath.Join(sys, "block", name, f)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(v+"\n"), 0o644)
		}
	}
	add("mmcblk0", map[string]string{"size": "61071360", "device/type": "MMC", "device/name": "A3A564"})
	add("mmcblk0boot0", map[string]string{"size": "8192"})
	add("mmcblk1", map[string]string{"size": "62333952", "device/type": "SD", "device/name": "SD32G"})
	add("nvme0n1", map[string]string{"size": "500118192", "device/model": "Samsung SSD 980"})
	add("loop0", map[string]string{"size": "100"})
	got := Disks(sys, "mmcblk1")
	want := map[string]string{"mmcblk0": "emmc", "mmcblk1": "sd", "nvme0n1": "nvme"}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	for _, d := range got {
		if want[d.Name] != d.Kind || (d.Name == "mmcblk1") != d.Boot {
			t.Errorf("%+v", d)
		}
	}
}
