// Package netbridge keeps a Linux bridge of a board's ethernet ports, for
// firmware that puts the device in the wire (ramekind -bridge). The firmware
// runs gokrazy's DHCP client on the bridge, which waits until it exists.
//
// With one cable plugged in, the bridge behaves like that port. With a cable
// in each port, the device forwards between them like a two-port switch, so
// an app can sit in the wire between a router and the LAN and filter what
// crosses it.
package netbridge

import (
	"fmt"
	"strings"
)

// Spec names the bridge and its ports. The bridge takes the first port's MAC
// address, so the device keeps its DHCP lease and identity.
type Spec struct {
	Name  string
	Ports []string
}

// Parse reads a spec written NAME=PORT,PORT..., e.g. br0=eth0,eth1.
func Parse(s string) (Spec, error) {
	name, ports, ok := strings.Cut(s, "=")
	name = strings.TrimSpace(name)
	var sp Spec
	if ok && name != "" {
		sp.Name = name
		for _, p := range strings.Split(ports, ",") {
			if p = strings.TrimSpace(p); p != "" {
				sp.Ports = append(sp.Ports, p)
			}
		}
	}
	if len(sp.Ports) == 0 {
		return Spec{}, fmt.Errorf("invalid bridge %q (want NAME=PORT,PORT..., e.g. br0=eth0,eth1)", s)
	}
	for _, p := range sp.Ports {
		if p == sp.Name {
			return Spec{}, fmt.Errorf("invalid bridge %q: %s can't be its own port", s, p)
		}
	}
	return sp, nil
}

func (s Spec) String() string { return s.Name + "=" + strings.Join(s.Ports, ",") }
