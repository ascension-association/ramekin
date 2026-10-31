package config

import (
	"testing"
	"time"
)

func TestProjectDefaults(t *testing.T) {
	p, err := ParseProject([]byte("name: demo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source.Mode != ModeDef || p.Source.Def != "example.def" || p.Output != "build/demo.sif" || p.Arch() != "arm64" || p.Releases.Branch != "ramekin-channels" || p.Releases.TokenEnv != "GITHUB_TOKEN" {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if _, err := ParseProject([]byte("name: demo\nbogus: 1\n")); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := ParseProject([]byte("name: demo\nsource: {mode: vm}\n")); err == nil {
		t.Fatal("bad mode accepted")
	}
	// Projects from before definition files get a migration hint.
	for _, old := range []string{"name: demo\nsource: {mode: containerfile}\n", "name: demo\nsource: {containerfile: Containerfile}\n"} {
		if _, err := ParseProject([]byte(old)); err != errContainerfile {
			t.Errorf("%q: got %v", old, err)
		}
	}
}

func TestDevice(t *testing.T) {
	d, err := ParseDevice([]byte(`{"signing_pubkey":"x","health":{"grace":"3s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(d.Health.Grace) != 3*time.Second || d.DataDir != "/perm/ramekin" || len(d.Container.Binds) != 2 {
		t.Fatalf("unexpected: %+v", d)
	}
	if _, err := ParseDevice([]byte(`{}`)); err == nil {
		t.Fatal("device without signing key accepted")
	}
	if _, err := ParseDevice([]byte(`{"signing_pubkey":"x","update":{"enabled":true,"url":"https://e"}}`)); err == nil {
		t.Fatal("updates without age identity accepted")
	}
	if _, err := ParseDevice([]byte(`{"signing_pubkey":"x","age_identity":"k","update":{"enabled":true}}`)); err == nil {
		t.Fatal("updates without a channel URL accepted")
	}
}

func TestParseBind(t *testing.T) {
	b, err := ParseBind("/dev/ttyUSB0:/dev/ttyUSB0:ro")
	if err != nil || !b.ReadOnly || b.Dst != "/dev/ttyUSB0" {
		t.Fatalf("%+v %v", b, err)
	}
	for _, bad := range []string{"x", "a:relative", "a:/b:rx"} {
		if _, err := ParseBind(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
