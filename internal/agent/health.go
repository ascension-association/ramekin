package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Health decides whether a freshly started container is good.
type Health struct {
	Grace time.Duration
	HTTP  string
	TCP   string
	// Exec runs inside the container and must exit 0. It needs ExecProbe.
	Exec []string
	// ExecProbe runs argv in the filesystem of the container whose init has
	// the given PID (see container.Exec).
	ExecProbe func(ctx context.Context, pid int, argv []string) error
	Timeout   time.Duration
	// Poll is the probe interval; zero means 2s.
	Poll time.Duration
}

func (h Health) Check(ctx context.Context, p Process) error {
	select {
	case <-p.Done():
		return exitErr(p)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(h.Grace):
	}
	if h.HTTP == "" && h.TCP == "" && len(h.Exec) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	poll := h.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	var last error
	for {
		if last = h.probe(ctx, p); last == nil {
			return nil
		}
		select {
		case <-p.Done():
			return exitErr(p)
		case <-ctx.Done():
			return fmt.Errorf("health check timed out: %w", last)
		case <-time.After(poll):
		}
	}
}

func (h Health) probe(ctx context.Context, p Process) error {
	if err := h.probeNet(ctx); err != nil {
		return err
	}
	if len(h.Exec) > 0 {
		pp, ok := p.(interface{ Pid() int })
		if !ok || h.ExecProbe == nil {
			return errors.New("health.exec is not supported by this runtime")
		}
		// Readiness tools can be slow to answer while a database recovers.
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := h.ExecProbe(ctx, pp.Pid(), h.Exec); err != nil {
			return fmt.Errorf("health.exec %s: %w", h.Exec[0], err)
		}
	}
	return nil
}

func (h Health) probeNet(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if h.TCP != "" {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", h.TCP)
		if err != nil {
			return err
		}
		c.Close()
	}
	if h.HTTP != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.HTTP, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("%s returned %s", h.HTTP, resp.Status)
		}
	}
	return nil
}

func exitErr(p Process) error {
	if err := p.Err(); err != nil {
		return fmt.Errorf("container exited during health check: %w", err)
	}
	return errors.New("container exited during health check")
}
