// ramekind is the Ramekin device agent. It is the only program Ramekin adds
// to a gokrazy image: it runs one SIF container natively, installs updates
// from the release bucket or a developer push, and rolls back on failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/sigstore/sigstore/pkg/signature"
	"golang.org/x/sys/unix"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/container"
	"github.com/ascension-association/ramekin/internal/identity"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/release"
	"github.com/ascension-association/ramekin/internal/sifimg"
)

func main() {
	// Must come first: in the re-executed child this becomes the container.
	container.MaybeInit()

	cfgPath := flag.String("config", config.DefaultDevicePath, "device configuration file")
	flag.Parse()

	logs := agent.NewLogBuffer(os.Stdout, 2000)
	logger := log.New(io.MultiWriter(logs), "ramekind: ", log.LstdFlags)

	cfg, err := config.LoadDevice(*cfgPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	// Project settings: official firmware has none and uses the ones saved
	// when the device was claimed; an image built for a project has them,
	// and a copy is kept in case the device moves to official firmware.
	settingsPath := filepath.Join(cfg.DataDir, "settings.json")
	settingsFromDevice := cfg.Official()
	if settingsFromDevice {
		if s, err := config.LoadSettings(settingsPath); err != nil {
			logger.Printf("settings: %v", err)
		} else if s != nil {
			if err := cfg.Apply(*s); err != nil {
				logger.Fatalf("settings: %v", err)
			}
		}
	} else if err := saveSettings(settingsPath, cfg.Settings()); err != nil {
		logger.Printf("saving settings: %v", err)
	}
	// verifier checks apps and identity records (the project key; nil on
	// official firmware until claimed); fwVerifier checks firmware.
	var verifier signature.Verifier
	if cfg.SigningPubKey != "" {
		if verifier, err = keys.LoadVerifier([]byte(cfg.SigningPubKey)); err != nil {
			logger.Fatal(err)
		}
	}
	fwVerifier := verifier
	if cfg.Firmware.SigningPubKey != "" {
		if fwVerifier, err = keys.LoadVerifier([]byte(cfg.Firmware.SigningPubKey)); err != nil {
			logger.Fatalf("firmware signing key: %v", err)
		}
	}
	if err := container.Preflight(); err != nil {
		// Keep running so the API can report the problem; starts will fail.
		logger.Printf("WARNING: %v", err)
	}

	rt := &container.Runtime{Cfg: cfg.Container, DataDir: cfg.DataDir, Output: logs}
	sup, err := agent.NewSupervisor(agent.SupervisorOptions{
		DataDir: cfg.DataDir,
		Start: func(image string) (agent.Process, error) {
			p, err := rt.Start(image)
			if err != nil {
				return nil, err
			}
			return p, nil
		},
		Verify: func(ctx context.Context, image string) error {
			if verifier == nil {
				return errors.New("this device isn't claimed, so it has no project key to check apps with")
			}
			if err := sifimg.Verify(ctx, image, verifier); err != nil {
				return err
			}
			_, err := sifimg.PrimaryRootFS(image)
			return err
		},
		Health: agent.Health{
			Grace:     time.Duration(cfg.Health.Grace),
			HTTP:      cfg.Health.HTTP,
			TCP:       cfg.Health.TCP,
			Exec:      cfg.Health.Exec,
			ExecProbe: container.Exec,
			Timeout:   time.Duration(cfg.Health.Timeout),
		},
		StopTimeout:  time.Duration(cfg.Container.StopTimeout),
		RestartDelay: time.Duration(cfg.Container.RestartDelay),
		Log:          logger,
	})
	if err != nil {
		logger.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	metrics := &agent.Sampler{Disk: cfg.DataDir}
	go metrics.Run(ctx, 5*time.Second)

	// Who this device is: claimed (identity on /perm), built for one board
	// (identity in the image), or an unclaimed fleet device.
	var restart atomic.Bool
	ident, err := identity.Ensure(filepath.Join(cfg.DataDir, "identity"))
	if err != nil {
		logger.Fatalf("identity: %v", err)
	}
	hw := identity.HardwareID()
	if hw == "" {
		logger.Print("WARNING: this board exposes no hardware serial, so its identity can't be bound to it")
	}
	claimer := &agent.Claimer{
		Identity: ident, Verifier: verifier, HardwareID: hw, LegacyToken: cfg.APIToken,
		GokrazyPasswordPath: "/perm/gokr-pw.txt", Log: logger,
		SaveSettings: func(s config.Settings) error { return saveSettings(settingsPath, s) },
		// Restart to run with the new identity (see the end of main).
		Claimed: func() { restart.Store(true); stop() },
	}
	if err := ident.Load(verifier, hw); err != nil {
		claimer.Problem = err.Error()
		logger.Printf("identity on /perm rejected: %v; the device must be claimed again", err)
	}
	name, token := cfg.Name, cfg.APIToken
	var ageID age.Identity
	var sign func([]byte) string
	switch {
	case ident.Record != nil:
		name, token, sign = ident.Record.Name, ident.Token, ident.Sign
		// Releases may be encrypted to the device's own key, the fleet key it
		// was given when claimed, or (built for one board, then claimed) the
		// key in its image.
		ids := anyIdentity{ident.Age}
		if ident.Fleet != nil {
			ids = append(ids, ident.Fleet)
		}
		if cfg.AgeIdentity != "" {
			if old, err := age.ParseX25519Identity(cfg.AgeIdentity); err == nil {
				ids = append(ids, old)
			}
		}
		ageID = ids
		os.Remove(agent.PairingCodeFile)
		if err := unix.Sethostname([]byte(name)); err != nil {
			logger.Printf("hostname: %v", err)
		}
		logger.Printf("device %s (claimed, hardware %s)", name, hw)
	case cfg.AgeIdentity != "":
		ageID, err = age.ParseX25519Identity(cfg.AgeIdentity)
		if err != nil {
			logger.Fatalf("age_identity: %v", err)
		}
	}
	if token == "" {
		go claimer.Announce(ctx)
	}

	fw := &agent.Firmware{
		DataDir: cfg.DataDir, Version: cfg.Firmware.Version, URL: cfg.Firmware.URL, Channel: cfg.FirmwareChannel(),
		Verifier: fwVerifier, Gokrazy: agent.LocalGokrazy, Log: logger,
	}
	if ageID != nil {
		fw.Identity = ageID
	}
	bootDone := make(chan struct{})
	fw.Healthy = func(ctx context.Context) error {
		select {
		case <-bootDone:
		case <-ctx.Done():
			return errors.New("timed out waiting for the app to start")
		}
		if !sup.BootHealthy() {
			return errors.New("the app failed its health check")
		}
		// The app's runner reports it running just after boot; give it time.
		if st := sup.State(); st.Current != nil {
			deadline := time.Now().Add(2 * time.Minute)
			for !sup.App().Running {
				if time.Now().After(deadline) {
					return fmt.Errorf("app %s is not running", st.Current.Version)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("app %s is not running", st.Current.Version)
				case <-time.After(time.Second):
				}
			}
		}
		if cfg.Firmware.URL != "" {
			// The new image must still reach the update server, or it could
			// never be updated again. The network (DHCP, DNS) may still be
			// coming up, so keep trying for a while.
			url := strings.TrimSuffix(cfg.Firmware.URL, "/") + "/" + cfg.FirmwareChannel() + ".json"
			deadline := time.Now().Add(3 * time.Minute)
			for {
				_, err := release.FetchBytes(ctx, url, 1<<20)
				if err == nil || errors.Is(err, release.ErrNotFound) {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("can't reach the firmware channel: %w", err)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("can't reach the firmware channel: %w", err)
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil
	}

	var upd *agent.Updater
	if u := cfg.Update; u.Enabled && ageID != nil && verifier != nil {
		upd = &agent.Updater{
			Sup: sup, URL: u.URL, Channel: u.Channel, Verifier: verifier, Identity: ageID,
			Interval: time.Duration(u.Interval), Device: name, StatusURL: u.StatusURL, Metrics: metrics,
			Firmware: fw, Sign: sign, Log: logger,
		}
	}

	api := &agent.API{
		Sup: sup, Upd: upd, Logs: logs, Metrics: metrics, Token: token, Device: name,
		Sign: sign, Claim: claimer, Firmware: fw, Verifier: fwVerifier,
		Settings: &agent.SettingsStore{
			Current: cfg.Settings(), FromDevice: settingsFromDevice, Verifier: verifier,
			Save:    func(s config.Settings) error { return saveSettings(settingsPath, s) },
			Restart: func() { restart.Store(true); stop() },
		},
	}
	if ident.Record != nil {
		api.Release = &agent.Releaser{
			Name: name, Verifier: verifier, DataDir: cfg.DataDir, Log: logger,
			Stop: sup.Shutdown, Restart: func() { restart.Store(true); stop() },
		}
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("api: %v", err)
		}
	}()

	go agent.KeepWatchdog(ctx, logger) // only if gokrazy couldn't (test boots)
	if fw.TestBooted() {
		// If the app fails here, the new firmware is to blame: revert that.
		sup.HoldRollback()
	}
	go fw.Resume(ctx) // confirms or reverts a firmware test boot
	go func() {
		if err := sup.Boot(ctx); err != nil {
			logger.Printf("boot: %v", err)
		}
		close(bootDone)
		if upd != nil {
			upd.Run(ctx) // checks firmware too
			return
		}
		if cfg.Firmware.URL != "" {
			// No app updates (not claimed yet, or a dev device): still
			// follow the firmware channel.
			for {
				if err := fw.Check(ctx); err != nil {
					logger.Printf("firmware check: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(cfg.Update.Interval)):
				}
			}
		}
	}()

	<-ctx.Done()
	logger.Print("shutting down")
	srv.Close()
	sup.Shutdown()
	if restart.Load() {
		// gokrazy restarts a service that fails, but not one that exits 0.
		logger.Print("restarting to apply the new identity or settings")
		os.Exit(1)
	}
}

func saveSettings(path string, s config.Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	f, err := os.Open(tmp)
	if err == nil {
		f.Sync()
		f.Close()
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// anyIdentity decrypts with whichever of its identities a file was
// encrypted to.
type anyIdentity []age.Identity

func (ids anyIdentity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	var err error
	for _, id := range ids {
		var key []byte
		if key, err = id.Unwrap(stanzas); err == nil {
			return key, nil
		}
	}
	return nil, err
}
