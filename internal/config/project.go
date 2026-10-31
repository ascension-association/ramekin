// Package config holds the developer-side project configuration (ramekin.yaml)
// and the device-side agent configuration (device.json).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const ProjectFile = "ramekin.yaml"

const (
	ModeDef     = "def"
	ModeOverlay = "overlay"

	DefaultBase = "docker://cgr.dev/chainguard/wolfi-base:latest"
)

type Project struct {
	Name     string   `yaml:"name"`
	Platform string   `yaml:"platform"` // e.g. linux/arm64
	Source   Source   `yaml:"source"`
	Output   string   `yaml:"output"`
	Scan     Scan     `yaml:"scan"`
	Alerts   Alerts   `yaml:"alerts"`
	Signing  Signing  `yaml:"signing"`
	Encrypt  Encrypt  `yaml:"encryption"`
	Releases Releases `yaml:"releases"`
	Channels []string `yaml:"channels"`

	// Dir is the directory containing ramekin.yaml. Not serialized.
	Dir string `yaml:"-"`
}

type Source struct {
	Mode string `yaml:"mode"` // def | overlay

	// Def mode: an Apptainer definition file.
	Def string `yaml:"def"`

	// Overlay mode.
	Base          string `yaml:"base"`
	BaseSIF       string `yaml:"base_sif"`
	Overlay       string `yaml:"overlay"`
	OverlaySizeMB int    `yaml:"overlay_size_mb"`
	Runscript     string `yaml:"runscript"`
}

type Scan struct {
	FailOn string   `yaml:"fail_on"` // negligible|low|medium|high|critical|none
	Ignore []string `yaml:"ignore"`
}

type Alerts struct {
	Webhook     string `yaml:"webhook"`
	MinSeverity string `yaml:"min_severity"`
}

type Signing struct {
	Key    string `yaml:"key"`
	PubKey string `yaml:"pubkey"`
}

type Encrypt struct {
	Recipients     []string `yaml:"recipients"`
	RecipientsFile string   `yaml:"recipients_file"`
}

// Releases is where `ramekin publish` puts releases: GitHub Releases of a
// repository, with channel manifests on a branch of the same repository.
type Releases struct {
	// GitHub is the repository, as owner/name. It should be public: devices
	// download anonymously (images are encrypted; names and sizes are not).
	GitHub string `yaml:"github"`
	// Branch holds the channel manifests, one file per channel.
	Branch string `yaml:"branch"`
	// Firmware is the repository for firmware releases (`ramekin firmware
	// publish`), as owner/name; default: the same as GitHub.
	Firmware string `yaml:"firmware"`
	// TokenEnv names the environment variable with a GitHub token that can
	// write the repository's contents and releases (default GITHUB_TOKEN;
	// falls back to GH_TOKEN, then `gh auth token`).
	TokenEnv string `yaml:"token_env"`
}

// ChannelURL is the directory devices fetch <channel>.json from.
func (p *Project) ChannelURL() string {
	return "https://raw.githubusercontent.com/" + p.Releases.GitHub + "/" + p.Releases.Branch + "/" + p.Name
}

// FirmwareRepo is the repository firmware is published to.
func (p *Project) FirmwareRepo() string {
	if p.Releases.Firmware != "" {
		return p.Releases.Firmware
	}
	return p.Releases.GitHub
}

// FirmwareChannelURL is the directory devices fetch firmware <channel>.json from.
func (p *Project) FirmwareChannelURL() string {
	return "https://raw.githubusercontent.com/" + p.FirmwareRepo() + "/" + p.Releases.Branch + "/" + p.Name + "-firmware"
}

func (p *Project) applyDefaults() {
	if p.Platform == "" {
		p.Platform = "linux/arm64"
	}
	s := &p.Source
	if s.Mode == "" {
		s.Mode = ModeDef
	}
	if s.Def == "" {
		s.Def = "example.def"
	}
	if s.Base == "" {
		s.Base = DefaultBase
	}
	if s.BaseSIF == "" {
		s.BaseSIF = "build/base.sif"
	}
	if s.Overlay == "" {
		s.Overlay = "overlay.img"
	}
	if s.OverlaySizeMB == 0 {
		s.OverlaySizeMB = 1024
	}
	if p.Output == "" {
		p.Output = filepath.Join("build", p.Name+".sif")
	}
	if p.Scan.FailOn == "" {
		p.Scan.FailOn = "high"
	}
	if p.Alerts.MinSeverity == "" {
		p.Alerts.MinSeverity = "high"
	}
	if p.Signing.Key == "" {
		p.Signing.Key = "keys/ramekin-sign.key"
	}
	if p.Signing.PubKey == "" {
		p.Signing.PubKey = "keys/ramekin-sign.pub"
	}
	if p.Encrypt.RecipientsFile == "" {
		p.Encrypt.RecipientsFile = "keys/recipients.txt"
	}
	r := &p.Releases
	if r.Branch == "" {
		r.Branch = "ramekin-channels"
	}
	if r.TokenEnv == "" {
		r.TokenEnv = "GITHUB_TOKEN"
	}
	if len(p.Channels) == 0 {
		p.Channels = []string{"stable"}
	}
}

func (p *Project) validate() error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(p.Name, "/ ") {
		return fmt.Errorf("name %q must not contain '/' or spaces", p.Name)
	}
	switch p.Source.Mode {
	case ModeDef, ModeOverlay:
	case "containerfile":
		return errContainerfile
	default:
		return fmt.Errorf("source.mode must be %q or %q", ModeDef, ModeOverlay)
	}
	if r := p.Releases.GitHub; r != "" && strings.Count(r, "/") != 1 {
		return fmt.Errorf("releases.github must be owner/repo, got %q", r)
	}
	return nil
}

// Path resolves a project-relative path.
func (p *Project) Path(rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(p.Dir, rel)
}

// Arch returns the GOARCH-style architecture of Platform.
func (p *Project) Arch() string {
	_, arch, _ := strings.Cut(p.Platform, "/")
	arch, _, _ = strings.Cut(arch, "/") // drop variant, e.g. arm/v7
	return arch
}

// LoadProject finds ramekin.yaml in dir or any parent directory.
func LoadProject(dir string) (*Project, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for d := dir; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, ProjectFile))
		if err == nil {
			p, err := ParseProject(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Join(d, ProjectFile), err)
			}
			p.Dir = d
			return p, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if filepath.Dir(d) == d {
			return nil, fmt.Errorf("no %s found in %s or any parent (run `ramekin init`)", ProjectFile, dir)
		}
	}
}

// errContainerfile explains the move from Containerfiles to definition files.
var errContainerfile = errors.New("Containerfile mode was replaced by Apptainer definition files: " +
	"convert your Containerfile to an Apptainer definition file such as example.def (see \"Converting a Dockerfile or container image\" in the README), " +
	"then set source.mode: def and source.def to its file name, and remove source.containerfile, source.context and source.engine")

func ParseProject(b []byte) (*Project, error) {
	var p Project
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		for _, old := range []string{"containerfile", "context", "engine"} {
			if strings.Contains(err.Error(), "field "+old+" not found") {
				return nil, errContainerfile
			}
		}
		if strings.Contains(err.Error(), "field store not found") {
			return nil, errors.New("S3 storage was replaced by GitHub Releases: replace the store: section with releases: {github: owner/repo} (see \"Ship to a fleet of devices\" in the README)")
		}
		return nil, err
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
