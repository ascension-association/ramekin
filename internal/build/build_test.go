package build

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanOverlay(t *testing.T) {
	chars := []string{"./etc/motd"}
	all := []string{
		"./usr", "./usr/bin", "./usr/bin/app",
		"./.profile",
		"./var/cache/.wh.apk",
		"./opt/thing", "./opt/thing/.wh..wh..opq", "./opt/thing/new",
	}
	got := PlanOverlay(chars, all)
	want := OverlayPlan{
		Include:   []string{"usr", "usr/bin", "usr/bin/app", ".profile", "opt/thing", "opt/thing/new"},
		Whiteouts: []string{"etc/motd", "var/cache/apk"},
		Opaque:    []string{"opt/thing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestOverlayDef(t *testing.T) {
	def := overlayDef("/p/base.sif", "/tmp/x", "/usr/bin/app --serve")
	for _, s := range []string{"Bootstrap: localimage", "From: /p/base.sif", "/tmp/x/upper.tar /.ramekin-upper.tar", "exec /usr/bin/app --serve \"$@\""} {
		if !strings.Contains(def, s) {
			t.Errorf("def missing %q:\n%s", s, def)
		}
	}
}
