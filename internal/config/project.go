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
	Store    Store    `yaml:"store"`
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

type Store struct {
	Endpoint      string `yaml:"endpoint"`
	Region        string `yaml:"region"`
	Bucket        string `yaml:"bucket"`
	Prefix        string `yaml:"prefix"`
	AccessKeyEnv  string `yaml:"access_key_env"`
	SecretKeyEnv  string `yaml:"secret_key_env"`
	RetentionDays int    `yaml:"retention_days"`
	RetentionMode string `yaml:"retention_mode"` // GOVERNANCE | COMPLIANCE
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
	st := &p.Store
	if st.Region == "" {
		st.Region = "auto"
	}
	if st.Prefix == "" {
		st.Prefix = p.Name
	}
	if st.AccessKeyEnv == "" {
		st.AccessKeyEnv = "RAMEKIN_S3_ACCESS_KEY"
	}
	if st.SecretKeyEnv == "" {
		st.SecretKeyEnv = "RAMEKIN_S3_SECRET_KEY"
	}
	if st.RetentionMode == "" {
		st.RetentionMode = "GOVERNANCE"
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
	switch strings.ToUpper(p.Store.RetentionMode) {
	case "GOVERNANCE", "COMPLIANCE":
	default:
		return fmt.Errorf("store.retention_mode must be GOVERNANCE or COMPLIANCE")
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
		return nil, err
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
