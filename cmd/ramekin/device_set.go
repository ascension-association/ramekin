package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ascension-association/ramekin/internal/config"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// deviceSet changes a device's device.json inside its gokrazy instance config.
// Changes reach the device on its next `ramekin device update` (or --apply).
func deviceSet(ctx context.Context, args []string) error {
	fl := flags("device set")
	parent := fl.String("gokrazy-dir", "", "gokrazy parent directory (default ~/gokrazy)")
	apply := fl.Bool("apply", false, "deploy the change now with `ramekin device update` (reboots the device)")
	healthHTTP := fl.String("health-http", "", "URL that must return 2xx before a release is accepted (\"\" to remove)")
	healthTCP := fl.String("health-tcp", "", "host:port that must accept connections before a release is accepted (\"\" to remove)")
	healthExec := fl.String("health-exec", "", "command run inside the container that must exit 0, e.g. \"pg_isready -h 127.0.0.1\" (\"\" to remove)")
	grace := fl.Duration("health-grace", 0, "how long a new container must stay up before probing")
	timeout := fl.Duration("health-timeout", 0, "how long the probes may take to pass")
	updates := fl.String("updates", "", "on: follow the release channel (its URL comes from this project's releases.github); off: only accept pushes")
	updateURL := fl.String("update-url", "", "directory of channel manifests to download from (default: this project's GitHub release channels)")
	channel := fl.String("channel", "", "release channel to follow")
	interval := fl.Duration("interval", 0, "how often to poll the channel")
	statusURL := fl.String("status-url", "", "URL the device POSTs a JSON status report to after each check (\"\" to remove)")
	hostname := fl.String("hostname", "", "container hostname")
	command := fl.String("command", "", "command overriding the image's runscript, with shell-style quoting (\"\" to use the runscript)")
	var envs, unsetEnvs, binds, unbinds stringList
	fl.Var(&envs, "env", "set a container environment variable KEY=VALUE (repeatable)")
	fl.Var(&unsetEnvs, "unset-env", "remove a container environment variable (repeatable)")
	fl.Var(&binds, "bind", "add a bind mount src:dst[:ro] (repeatable)")
	fl.Var(&unbinds, "unbind", "remove the bind mount with this destination (repeatable)")
	pos, err := parse(fl, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ramekin device set <name> [flags]   (no flags: show current settings)")
	}
	name := pos[0]
	cfgPath := filepath.Join(gokrazyDir(*parent), name, "config.json")
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(orig, &cfg); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	files, err := deviceFiles(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	raw, _ := files[config.DefaultDevicePath].(string)
	// Decode without defaults so only what the user set is written back.
	var dev config.Device
	if err := json.Unmarshal([]byte(raw), &dev); err != nil {
		return fmt.Errorf("device.json: %w", err)
	}

	set := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { set[f.Name] = true })
	changed := false
	for n := range set {
		if n != "gokrazy-dir" && n != "apply" {
			changed = true
		}
	}
	if !changed {
		return showDevice(dev)
	}

	if set["health-http"] {
		dev.Health.HTTP = *healthHTTP
	}
	if set["health-tcp"] {
		dev.Health.TCP = *healthTCP
	}
	if set["health-exec"] {
		if dev.Health.Exec, err = splitArgs(*healthExec); err != nil {
			return fmt.Errorf("--health-exec: %w", err)
		}
	}
	if set["health-grace"] {
		dev.Health.Grace = config.Duration(*grace)
	}
	if set["health-timeout"] {
		dev.Health.Timeout = config.Duration(*timeout)
	}
	if set["channel"] {
		dev.Update.Channel = *channel
	}
	if set["update-url"] {
		dev.Update.URL = *updateURL
	}
	switch *updates {
	case "":
	case "on":
		dev.Update.Enabled = true
		if dev.Update.URL == "" {
			p, err := loadProject()
			if err != nil || p.Releases.GitHub == "" {
				return errors.New("--updates on needs releases.github in this project's ramekin.yaml, or --update-url")
			}
			dev.Update.URL = p.ChannelURL()
		}
	case "off":
		dev.Update.Enabled = false
	default:
		return fmt.Errorf("--updates must be on or off, got %q", *updates)
	}
	if set["interval"] {
		dev.Update.Interval = config.Duration(*interval)
	}
	if set["status-url"] {
		dev.Update.StatusURL = *statusURL
	}
	if set["hostname"] {
		dev.Container.Hostname = *hostname
	}
	if set["command"] {
		if dev.Container.Command, err = splitArgs(*command); err != nil {
			return fmt.Errorf("--command: %w", err)
		}
	}
	for _, kv := range envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("--env %q: want KEY=VALUE", kv)
		}
		if dev.Container.Env == nil {
			dev.Container.Env = map[string]string{}
		}
		dev.Container.Env[k] = v
	}
	for _, k := range unsetEnvs {
		delete(dev.Container.Env, k)
	}
	if len(binds) > 0 || len(unbinds) > 0 {
		if dev.Container.Binds == nil {
			// Start from the defaults the agent would otherwise apply.
			dev.Container.Binds = []string{"data:/data", "/dev:/dev"}
		}
		for _, dst := range unbinds {
			dev.Container.Binds = slices.DeleteFunc(dev.Container.Binds, func(b string) bool {
				pb, err := config.ParseBind(b)
				return err == nil && filepath.Clean(pb.Dst) == filepath.Clean(dst)
			})
		}
		for _, b := range binds {
			if _, err := config.ParseBind(b); err != nil {
				return err
			}
			dev.Container.Binds = append(dev.Container.Binds, b)
		}
		if len(dev.Container.Binds) == 0 {
			return errors.New("removing every bind would restore the defaults (data:/data, /dev:/dev); keep at least one")
		}
	}

	out, err := json.MarshalIndent(dev, "", "  ")
	if err != nil {
		return err
	}
	// The agent refuses to start with an invalid config, so check it here.
	if _, err := config.ParseDevice(out); err != nil {
		return fmt.Errorf("resulting device.json is invalid: %w", err)
	}
	files[config.DefaultDevicePath] = string(out)
	newCfg, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath+".bak", orig, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, append(newCfg, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("updated %s (backup: config.json.bak)\n", cfgPath)
	if err := showDevice(dev); err != nil {
		return err
	}
	if !*apply {
		fmt.Printf("\ndeploy with: ramekin device update %s   (reboots the device)\n", name)
		return nil
	}
	return deviceUpdate(ctx, []string{name, "--gokrazy-dir", gokrazyDir(*parent)})
}

// deviceFiles returns ramekind's ExtraFileContents map within a gokrazy config.
func deviceFiles(cfg map[string]any) (map[string]any, error) {
	pc, _ := cfg["PackageConfig"].(map[string]any)
	pkg, _ := pc[ramekindPkg].(map[string]any)
	files, _ := pkg["ExtraFileContents"].(map[string]any)
	if _, ok := files[config.DefaultDevicePath].(string); !ok {
		return nil, fmt.Errorf("no ramekind device.json; run `ramekin device init` first")
	}
	return files, nil
}

// showDevice prints device.json with secrets hidden.
func showDevice(dev config.Device) error {
	redact := func(s *string) {
		if *s != "" {
			*s = "<hidden>"
		}
	}
	redact(&dev.APIToken)
	redact(&dev.AgeIdentity)
	if dev.SigningPubKey != "" {
		dev.SigningPubKey = "<set>"
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(dev)
}

func gokrazyDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "gokrazy")
	}
	return "gokrazy"
}

// splitArgs splits a command line into words, honoring single quotes,
// double quotes and backslash escapes like a POSIX shell (no expansion).
func splitArgs(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}
