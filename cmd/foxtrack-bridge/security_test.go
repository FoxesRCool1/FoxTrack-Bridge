package main

import (
	"testing"

	"foxtrack-bridge/config"
)

// A saved secret fills a test only when the test goes to the saved printer.
func TestSameEndpoint(t *testing.T) {
	klipper := config.Printer{MoonrakerURL: "http://10.0.0.5:7125", APIKey: "k"}
	bambu := config.Printer{IP: "10.0.0.7", Serial: "S", LANCode: "c"}
	cases := []struct {
		name  string
		saved config.Printer
		test  config.Printer
		want  bool
	}{
		{"same klipper url", klipper, config.Printer{MoonrakerURL: "HTTP://10.0.0.5:7125/"}, true},
		{"other host", klipper, config.Printer{MoonrakerURL: "http://evil:8000"}, false},
		{"other kind", klipper, config.Printer{OctoPrintURL: "http://10.0.0.5:7125"}, false},
		{"same bambu ip", bambu, config.Printer{IP: " 10.0.0.7 "}, true},
		{"other bambu ip", bambu, config.Printer{IP: "10.0.0.66"}, false},
		{"blank address", bambu, config.Printer{}, false},
	}
	for _, tc := range cases {
		if got := sameEndpoint(tc.saved, tc.test); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// DNS rebinding sends a public name as Host; LAN names and addresses pass.
func TestHostAllowed(t *testing.T) {
	t.Setenv("FOXTRACK_BRIDGE_ALLOWED_HOSTS", "bridge.myshop.com")
	for host, want := range map[string]bool{
		"localhost:8080":      true,
		"127.0.0.1:8080":      true,
		"[::1]:8080":          true,
		"192.168.1.20:8080":   true,
		"printpc:8080":        true,
		"printpc.local:8080":  true,
		"bridge.lan":          true,
		"bridge.myshop.com":   true,
		"evil.example:8080":   false,
		"evil.example.:8080":  false,
		"192.168.1.20.nip.io": false,
		"attacker.com":        false,
	} {
		if got := hostAllowed(host); got != want {
			t.Errorf("%s: got %v, want %v", host, got, want)
		}
	}
}
