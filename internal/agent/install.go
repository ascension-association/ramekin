package agent

import "time"

// InstallRequest, signed with the project key, asks a device to install
// itself onto internal storage (POST /v1/install).
type InstallRequest struct {
	Device    string    `json:"device"` // the device's name
	Target    string    `json:"target"` // a disk name from its status, e.g. mmcblk0
	Overwrite bool      `json:"overwrite,omitempty"`
	At        time.Time `json:"at"`
}

// InstallResult is the answer to POST /v1/install.
type InstallResult struct {
	Target   string `json:"target"`
	Firmware string `json:"firmware"`
	// Erased lists the partitions the target had, if Overwrite allowed it.
	Erased []string `json:"erased,omitempty"`
}
