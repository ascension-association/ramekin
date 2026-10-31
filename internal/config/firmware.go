package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FirmwareFile describes an official firmware project: a gokrazy image for
// one board, with ramekind and no project, which anyone can flash. Its
// devices learn their project when they're claimed.
const FirmwareFile = "firmware.yaml"

type FirmwareProject struct {
	// Board names the hardware (e.g. r76s); it names releases and channels.
	Board    string           `yaml:"board"`
	Releases FirmwareReleases `yaml:"releases"`
	Signing  Signing          `yaml:"signing"`
	Channels []string         `yaml:"channels"`
	// Instance is the gokrazy instance the image is built from.
	Instance string `yaml:"instance"`

	Dir string `yaml:"-"`
}

type FirmwareReleases struct {
	GitHub   string `yaml:"github"`
	Branch   string `yaml:"branch"`
	TokenEnv string `yaml:"token_env"`
}

// OfficialFirmware, in a project's ramekin.yaml, names the official
// firmware its devices run.
type OfficialFirmware struct {
	GitHub string `yaml:"github"` // owner/name of the firmware repository
	Board  string `yaml:"board"`
	Branch string `yaml:"branch"` // default main
}

func (o OfficialFirmware) branch() string {
	if o.Branch == "" {
		return "main"
	}
	return o.Branch
}

// ChannelURL is the directory devices fetch firmware <channel>.json from.
func (o OfficialFirmware) ChannelURL() string {
	return "https://raw.githubusercontent.com/" + o.GitHub + "/" + o.branch() + "/" + o.Board
}

// KeyURL is where the firmware's public signing key is published.
func (o OfficialFirmware) KeyURL() string {
	return "https://raw.githubusercontent.com/" + o.GitHub + "/" + o.branch() + "/" + FirmwareKeyPub
}

// FirmwareKeyPub is where a firmware project keeps its public signing key;
// devices are built with it, and projects fetch it from the repository.
const FirmwareKeyPub = "keys/firmware-sign.pub"

func (f *FirmwareProject) Path(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(f.Dir, rel)
}

// Ref is how a project's ramekin.yaml refers to this firmware.
func (f *FirmwareProject) Ref() OfficialFirmware {
	return OfficialFirmware{GitHub: f.Releases.GitHub, Board: f.Board, Branch: f.Releases.Branch}
}

func (f *FirmwareProject) applyDefaults() {
	if f.Releases.Branch == "" {
		f.Releases.Branch = "main"
	}
	if f.Releases.TokenEnv == "" {
		f.Releases.TokenEnv = "GITHUB_TOKEN"
	}
	if f.Signing.Key == "" {
		f.Signing.Key = "keys/firmware-sign.key"
	}
	if f.Signing.PubKey == "" {
		f.Signing.PubKey = FirmwareKeyPub
	}
	if len(f.Channels) == 0 {
		f.Channels = []string{"stable"}
	}
	if f.Instance == "" {
		f.Instance = "firmware." + f.Board
	}
}

// LoadFirmwareProject reads firmware.yaml from dir; it returns an error
// wrapping os.ErrNotExist if there is none.
func LoadFirmwareProject(dir string) (*FirmwareProject, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, FirmwareFile))
	if err != nil {
		return nil, err
	}
	var f FirmwareProject
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", FirmwareFile, err)
	}
	if f.Board == "" || f.Releases.GitHub == "" {
		return nil, fmt.Errorf("%s: board and releases.github are required", FirmwareFile)
	}
	f.applyDefaults()
	f.Dir = dir
	return &f, nil
}

// HasFirmwareProject reports whether dir holds a firmware project.
func HasFirmwareProject(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, FirmwareFile))
	return !errors.Is(err, os.ErrNotExist)
}
