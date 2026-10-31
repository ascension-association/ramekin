package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

type pidProc struct{ *fakeProc }

func (pidProc) Pid() int { return 42 }

func TestHealthExec(t *testing.T) {
	calls := 0
	h := Health{
		Exec: []string{"pg_isready"}, Timeout: time.Second, Poll: time.Millisecond,
		ExecProbe: func(ctx context.Context, pid int, argv []string) error {
			calls++
			if pid != 42 || argv[0] != "pg_isready" {
				t.Errorf("probe got pid=%d argv=%v", pid, argv)
			}
			if calls < 3 {
				return errors.New("no response")
			}
			return nil
		},
	}
	p := pidProc{&fakeProc{done: make(chan struct{})}}
	if err := h.Check(context.Background(), p); err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}

	h.ExecProbe = func(context.Context, int, []string) error { return errors.New("down") }
	if err := h.Check(context.Background(), p); err == nil {
		t.Fatal("failing exec probe passed")
	}

	// A runtime that can't exec into containers must fail closed.
	h.ExecProbe = nil
	if err := h.Check(context.Background(), p); err == nil {
		t.Fatal("exec probe without runtime support passed")
	}
}
