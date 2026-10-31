//go:build !linux && !darwin

package agent

func diskUsage(string) (total, used uint64) { return 0, 0 }
