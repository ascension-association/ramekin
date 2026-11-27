package netbridge

import (
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	s, err := Parse(" br0 = eth0, eth1 ,")
	if err != nil || s.Name != "br0" || !slices.Equal(s.Ports, []string{"eth0", "eth1"}) {
		t.Errorf("Parse = %+v, %v", s, err)
	}
	if s.String() != "br0=eth0,eth1" {
		t.Errorf("String = %q", s)
	}
	for _, bad := range []string{"", "br0", "br0=", "=eth0", "br0=br0,eth0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}
