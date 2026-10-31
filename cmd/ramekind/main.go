// ramekind is the Ramekin device agent. It is the only program Ramekin adds
// to a gokrazy image: it runs one SIF container natively, installs updates
// from the release bucket or a developer push, and rolls back on failure.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/ascension-association/ramekin/internal/agent"
	"github.com/ascension-association/ramekin/internal/config"
	"github.com/ascension-association/ramekin/internal/container"
	"github.com/ascension-association/ramekin/internal/keys"
	"github.com/ascension-association/ramekin/internal/sifimg"
	"github.com/ascension-association/ramekin/internal/store"
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

	var upd *agent.Updater
	if u := cfg.Update; u.Enabled {
		st, err := store.New(store.Options{
			Endpoint: u.Endpoint, Region: u.Region, Bucket: u.Bucket, Prefix: u.Prefix,
			AccessKey: u.AccessKey, SecretKey: u.SecretKey,
		})
		if err != nil {
			logger.Fatal(err)
		}
		id, err := age.ParseX25519Identity(cfg.AgeIdentity)
		if err != nil {
			logger.Fatalf("age_identity: %v", err)
		}
		upd = &agent.Updater{
			Sup: sup, Store: st, Channel: u.Channel, Verifier: verifier, Identity: id,
			Interval: time.Duration(u.Interval), Device: cfg.Name, Report: u.ReportStatus, Log: logger,
		}
	}

	api := &agent.API{Sup: sup, Upd: upd, Logs: logs, Token: cfg.APIToken, Device: cfg.Name}
	srv := &http.Server{Addr: cfg.Listen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("api: %v", err)
		}
	}()

	go func() {
		if err := sup.Boot(ctx); err != nil {
			logger.Printf("boot: %v", err)
		}
		if upd != nil {
			upd.Run(ctx)
		}
	}()

	<-ctx.Done()
	logger.Print("shutting down")
	srv.Close()
	sup.Shutdown()
}
