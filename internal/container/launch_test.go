package container

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// image writes a .singularity.d with Apptainer's boilerplate environment
// files (copied from a real image), the given extra environment files and
// runscript.
func image(t *testing.T, runscript string, env map[string]string) string {
	t.Helper()
	root := t.TempDir()
	envDir := filepath.Join(root, ".singularity.d", "env")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"99-base.sh", "95-apps.sh"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(envDir, f), b, 0o755)
	}
	os.WriteFile(filepath.Join(envDir, "01-base.sh"), []byte("#!/bin/sh\n# boilerplate\n"), 0o755)
	os.WriteFile(filepath.Join(envDir, "99-runtimevars.sh"), []byte("#!/bin/sh\nif [ -n \"${X:-}\" ]; then PATH=\"$X\"; fi\n"), 0o755)
	for n, s := range env {
		os.WriteFile(filepath.Join(envDir, n), []byte(s), 0o755)
	}
	os.WriteFile(filepath.Join(root, ".singularity.d", "runscript"), []byte(runscript), 0o755)
	return root
}

func ociRunscript(t *testing.T, entrypoint, cmd string) string {
	b, err := os.ReadFile(filepath.Join("testdata", "oci-runscript"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "OCI_ENTRYPOINT=''", "OCI_ENTRYPOINT='"+entrypoint+"'", 1)
	return strings.Replace(s, "OCI_CMD=''", "OCI_CMD='"+cmd+"'", 1)
}

const dockerEnv = `#!/bin/sh
export PATH="/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin:/bin"
export SSL_CERT_FILE="${SSL_CERT_FILE:-"/etc/ssl/certs/ca-certificates.crt"}"
`

const userEnv = `#!/bin/sh
# Copyright (c) Contributors to the Apptainer project

# Custom environment shell code should follow

    export GREETING='Hello World'
    APP_HOME=/opt/app
    export APP_HOME
    export DATA="$APP_HOME/data" LOG=${APP_HOME}/log
    export LD_LIBRARY_PATH=/opt/app/lib
`

func TestShellFreeLaunch(t *testing.T) {
	cases := []struct {
		name, runscript string
		want            []string
	}{
		{"def runscript", "#!/bin/sh\n\n    # Start the app.\n    exec /usr/bin/hello --greeting \"$GREETING\" \"$@\"\n", []string{"/usr/bin/hello", "--greeting", "Hello World"}},
		{"without exec", "#!/bin/sh\n/usr/bin/hello -v FOO=bar 'a b' $DATA\n", []string{"/usr/bin/hello", "-v", "FOO=bar", "a b", "/opt/app/data"}},
		{"continued line", "#!/bin/sh\nexec /usr/bin/hello \\\n    --port 8080 # serve\n", []string{"/usr/bin/hello", "--port", "8080"}},
		{"OCI entrypoint and cmd", ociRunscript(t, `"/usr/bin/nginx"`, `"-g" "daemon off;"`), []string{"/usr/bin/nginx", "-g", "daemon off;"}},
		{"OCI cmd only", ociRunscript(t, "", `"/app" "--x"`), []string{"/app", "--x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := image(t, c.runscript, map[string]string{"10-docker2singularity.sh": dockerEnv, "90-environment.sh": userEnv})
			argv, env, err := ShellFreeLaunch(root, []string{"PATH=/bin", "HOME=/root"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(argv, c.want) {
				t.Errorf("argv = %q, want %q", argv, c.want)
			}
			want := map[string]string{
				"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/bin:/usr/sbin:/sbin:/bin",
				"HOME":            "/root",
				"SSL_CERT_FILE":   "/etc/ssl/certs/ca-certificates.crt",
				"GREETING":        "Hello World",
				"DATA":            "/opt/app/data",
				"LOG":             "/opt/app/log",
				"LD_LIBRARY_PATH": "/opt/app/lib:/.singularity.d/libs",
			}
			got := map[string]string{}
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				got[k] = v
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestShellFreeLaunchDefaultsKeepSetValues(t *testing.T) {
	root := image(t, "#!/bin/sh\nexec /app\n", map[string]string{"10-docker2singularity.sh": dockerEnv})
	_, env, err := ShellFreeLaunch(root, []string{"SSL_CERT_FILE=/mine.pem"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "SSL_CERT_FILE=/mine.pem") {
		t.Errorf("a set value was replaced by its default: %q", env)
	}
}

func TestShellFreeLaunchNeedsShell(t *testing.T) {
	for _, rs := range []string{
		"#!/bin/sh\ncd /app && exec ./server\n",
		"#!/bin/sh\nexec /app | tee /log\n",
		"#!/bin/sh\nexec /app --at \"$(date)\"\n",
		"#!/bin/sh\nexec /app *.conf\n",
		"#!/bin/sh\necho starting\nexec /app\n",
		"#!/bin/sh\nPORT=80 /app\n",
	} {
		root := image(t, rs, nil)
		if _, _, err := ShellFreeLaunch(root, nil); !errors.Is(err, errNeedsShell) {
			t.Errorf("%q: err = %v, want errNeedsShell", rs, err)
		}
	}
	root := image(t, "#!/bin/sh\nexec /app\n", map[string]string{"90-environment.sh": "#!/bin/sh\nif [ -d /opt ]; then export A=1; fi\n"})
	if _, _, err := ShellFreeLaunch(root, nil); !errors.Is(err, errNeedsShell) {
		t.Errorf("environment with if: err = %v, want errNeedsShell", err)
	}
	root = image(t, ociRunscript(t, "", ""), nil)
	if _, _, err := ShellFreeLaunch(root, nil); err == nil || !strings.Contains(err.Error(), "no command") {
		t.Errorf("empty OCI image: err = %v", err)
	}
}
