//go:build !linux

package netbridge

import "context"

// Keep is a no-op off Linux, where ramekind doesn't run.
func Keep(ctx context.Context, s Spec, logf func(format string, args ...any)) {
	logf("bridge %s: not supported on this OS", s.Name)
}
