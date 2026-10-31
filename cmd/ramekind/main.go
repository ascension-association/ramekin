// ramekind is the Ramekin device agent. It is the only program Ramekin adds
// to a gokrazy image: it runs one SIF container natively, installs updates
// from the release bucket or a developer push, and rolls back on failure.
package main

import (
	"context"
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
	verifier, err := keys.LoadVerifier([]byte(cfg.SigningPubKey))
	if err != nil {
		logger.Fatal(err)
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
		name, token, ageID, sign = ident.Record.Name, ident.Token, ident.Age, ident.Sign
		if cfg.AgeIdentity != "" {
			// Built for one board, then claimed: releases may be encrypted
			// to either key.
			if old, err := age.ParseX25519Identity(cfg.AgeIdentity); err == nil {
				ageID = anyIdentity{ident.Age, old}
			}
		}
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
		DataDir: cfg.DataDir, Version: cfg.Firmware.Version, URL: cfg.Firmware.URL, Channel: cfg.Update.Channel,
		Verifier: verifier, Gokrazy: agent.LocalGokrazy, Log: logger,
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
			// never be updated again.
			if _, err := release.FetchBytes(ctx, strings.TrimSuffix(cfg.Firmware.URL, "/")+"/"+cfg.Update.Channel+".json", 1<<20); err != nil && !errors.Is(err, release.ErrNotFound) {
				return fmt.Errorf("can't reach the firmware channel: %w", err)
			}
		}
		return nil
	}

	var upd *agent.Updater
	if u := cfg.Update; u.Enabled && ageID != nil {
		upd = &agent.Updater{
			Sup: sup, URL: u.URL, Channel: u.Channel, Verifier: verifier, Identity: ageID,
			Interval: time.Duration(u.Interval), Device: name, StatusURL: u.StatusURL, Metrics: metrics,
			Firmware: fw, Sign: sign, Log: logger,
		}
	}

	api := &agent.API{
		Sup: sup, Upd: upd, Logs: logs, Metrics: metrics, Token: token, Device: name,
		Sign: sign, Claim: claimer, Firmware: fw, Verifier: verifier,
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
			upd.Run(ctx)
		}
	}()

	<-ctx.Done()
	logger.Print("shutting down")
	srv.Close()
	sup.Shutdown()
	if restart.Load() {
		// gokrazy restarts a service that fails, but not one that exits 0.
		logger.Print("restarting to apply the new identity")
		os.Exit(1)
	}
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
