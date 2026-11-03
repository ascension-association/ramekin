package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDropCopiedKeys(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		present []string
		dropped []string
	}{
		{"cloned project", []string{"sign.pub", "recipients.txt"}, []string{"sign.pub", "recipients.txt"}},
		{"own keys", []string{"sign.key", "sign.pub", "fleet.agekey", "recipients.txt"}, nil},
		{"fresh project", nil, nil},
		{"only fleet key missing", []string{"sign.key", "sign.pub", "recipients.txt"}, []string{"recipients.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := func(n string) string { return filepath.Join(dir, n) }
			for _, f := range tc.present {
				write(t, p(f))
			}
			got, err := dropCopiedKeys(p("sign.key"), p("sign.pub"), p("fleet.agekey"), p("recipients.txt"))
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, f := range tc.dropped {
				want = append(want, p(f))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("dropped %v, want %v", got, want)
			}
			for _, f := range tc.present {
				_, err := os.Stat(p(f))
				gone := err != nil
				dropped := false
				for _, d := range tc.dropped {
					dropped = dropped || d == f
				}
				if gone != dropped {
					t.Errorf("%s: removed=%v, want %v", f, gone, dropped)
				}
			}
		})
	}
}
