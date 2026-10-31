package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultDevicePath is where gokrazy's ExtraFileContents places the agent config.
const DefaultDevicePath = "/etc/ramekin/device.json"

// Device is the configuration of ramekind, the on-device agent.
type Device struct {
	Name          string `json:"name"`
	DataDir       string `json:"data_dir,omitempty"`
	Listen        string `json:"listen,omitempty"`
	APIToken      string `json:"api_token"`
	SigningPubKey string `json:"signing_pubkey"`         // PEM
	AgeIdentity   string `json:"age_identity,omitempty"` // AGE-SECRET-KEY-1...

	Update    Update    `json:"update,omitempty"`
	Container Container `json:"container,omitempty"`
	Health    Health    `json:"health,omitempty"`
}

type Update struct {
	Enabled      bool     `json:"enabled"`
	Interval     Duration `json:"interval,omitempty"`
	Channel      string   `json:"channel,omitempty"`
	ReportStatus bool     `json:"report_status,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Region       string   `json:"region,omitempty"`
	Bucket       string   `json:"bucket,omitempty"`
	Prefix       string   `json:"prefix,omitempty"`
	AccessKey    string   `json:"access_key,omitempty"`
	SecretKey    string   `json:"secret_key,omitempty"`
}

type Container struct {
	Hostname string            `json:"hostname,omitempty"`
	Command  []string          `json:"command,omitempty"` // overrides the SIF runscript
	Env      map[string]string `json:"env,omitempty"`
	// Binds are "src:dst[:ro]". Relative sources are resolved against DataDir.
	Binds        []string `json:"binds,omitempty"`
	TmpfsSizeMB  int      `json:"tmpfs_size_mb,omitempty"`
	StopTimeout  Duration `json:"stop_timeout,omitempty"`
	RestartDelay Duration `json:"restart_delay,omitempty"`
}

type Health struct {
	// Grace is how long the container must stay up before it is considered started.
	Grace Duration `json:"grace,omitempty"`
	// HTTP, if set, must return 2xx before Timeout elapses.
	HTTP string `json:"http,omitempty"`
	// TCP, if set (host:port), must accept a connection before Timeout elapses.
	TCP     string   `json:"tcp,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
}

func (d *Device) applyDefaults() {
	if d.DataDir == "" {
		d.DataDir = "/perm/ramekin"
	}
	if d.Listen == "" {
		d.Listen = ":8095"
	}
	u := &d.Update
	if u.Interval == 0 {
		u.Interval = Duration(5 * time.Minute)
	}
	if u.Channel == "" {
		u.Channel = "stable"
	}
	if u.Region == "" {
		u.Region = "auto"
	}
	c := &d.Container
	if c.Binds == nil {
		c.Binds = []string{"data:/data", "/dev:/dev"}
	}
	if c.TmpfsSizeMB == 0 {
		c.TmpfsSizeMB = 64
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = Duration(10 * time.Second)
	}
	if c.RestartDelay == 0 {
		c.RestartDelay = Duration(2 * time.Second)
	}
	h := &d.Health
	if h.Grace == 0 {
		h.Grace = Duration(15 * time.Second)
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(2 * time.Minute)
	}
}

func (d *Device) validate() error {
	if d.SigningPubKey == "" {
		return errors.New("signing_pubkey is required: unsigned images are never run")
	}
	if d.Update.Enabled {
		if d.Update.Endpoint == "" || d.Update.Bucket == "" {
			return errors.New("update.endpoint and update.bucket are required when updates are enabled")
		}
		if d.AgeIdentity == "" {
			return errors.New("age_identity is required when updates are enabled")
		}
	}
	for _, b := range d.Container.Binds {
		if _, err := ParseBind(b); err != nil {
			return err
		}
	}
	return nil
}

func LoadDevice(path string) (*Device, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseDevice(b)
}

func ParseDevice(b []byte) (*Device, error) {
	var d Device
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	d.applyDefaults()
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

type Bind struct {
	Src, Dst string
	ReadOnly bool
}

func ParseBind(s string) (Bind, error) {
	parts := strings.Split(s, ":")
	var b Bind
	switch len(parts) {
	case 3:
		if parts[2] != "ro" && parts[2] != "rw" {
			return b, fmt.Errorf("bind %q: option must be ro or rw", s)
		}
		b.ReadOnly = parts[2] == "ro"
		fallthrough
	case 2:
		b.Src, b.Dst = parts[0], parts[1]
	default:
		return b, fmt.Errorf("bind %q: want src:dst[:ro]", s)
	}
	if b.Src == "" || !strings.HasPrefix(b.Dst, "/") {
		return b, fmt.Errorf("bind %q: destination must be absolute", s)
	}
	return b, nil
}

// Duration is a time.Duration that (un)marshals as a string like "30s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
