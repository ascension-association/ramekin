package main

import "testing"

func TestParseNeighbors(t *testing.T) {
	linux := parseProcARP([]byte(`IP address       HW type     Flags       HW address            Mask     Device
192.168.31.1     0x1         0x2         74:DA:88:1C:A4:20     *        enp4s0
192.168.31.212   0x1         0x0         96:17:af:6e:a4:32     *        enp4s0
192.168.31.104   0x1         0x2         9e:66:e3:70:ff:de     *        enp4s0
`))
	if len(linux) != 2 || linux["192.168.31.1"] != "74:da:88:1c:a4:20" || linux["192.168.31.104"] != "9e:66:e3:70:ff:de" {
		t.Fatalf("linux: %v", linux)
	}
	mac := parseArpAn([]byte(`? (192.168.1.1) at 0:1b:2c:a:4d:5e on en0 ifscope [ethernet]
? (192.168.1.9) at (incomplete) on en0 ifscope [ethernet]
`))
	if len(mac) != 1 || mac["192.168.1.1"] != "00:1b:2c:0a:4d:5e" {
		t.Fatalf("arp -an: %v", mac)
	}
}

func TestVirtualInterface(t *testing.T) {
	for name, want := range map[string]bool{"enp4s0": false, "wlan0": false, "docker0": true, "br-825d8a7bb7a7": true, "veth12": true} {
		if virtualInterface(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}
