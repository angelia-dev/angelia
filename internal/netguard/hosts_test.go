package netguard

import "testing"

func TestHostListed(t *testing.T) {
	hosts := []string{"Mosquitto.lan", "hooks.example.net.", "10.0.0.5"}
	for host, want := range map[string]bool{
		"mosquitto.lan": true, "MOSQUITTO.LAN.": true, "hooks.example.net": true, "hooks.example.net.": true,
		"10.0.0.5": true, "other.lan": false, "": false, ".": false, "mosquitto.lan.evil": false,
	} {
		if got := HostListed(hosts, host); got != want {
			t.Errorf("HostListed(%q) = %v, want %v", host, got, want)
		}
	}
	if HostListed(nil, "a") {
		t.Fatal("empty list matched")
	}
}
