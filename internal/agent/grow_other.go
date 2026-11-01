//go:build !linux

package agent

import "log"

// GrowPerm only applies to devices, which run Linux.
func GrowPerm(*log.Logger) error { return nil }
