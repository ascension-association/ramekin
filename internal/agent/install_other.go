//go:build !linux

package agent

import "net/http"

// Installer only works on devices, which run Linux.
type Installer struct{}

func (in *Installer) install(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not supported", http.StatusNotImplemented)
}
