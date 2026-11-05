package main

import (
	"encoding/json"
	"testing"

	"foxtrack-bridge/config"
)

// The dashboard never sees a Tuya local key, and echoing back what it was
// given keeps the saved key.
func TestTuyaLocalKeyRedactedAndKept(t *testing.T) {
	const key = "0123456789abcdef"
	old := &config.Config{Printers: []config.Printer{{ID: "a", Name: "P1", IP: "192.168.1.9",
		Plug: &config.Plug{Kind: "tuya", Address: "192.168.1.80", DeviceID: "dev1", LocalKey: key}}}}

	rp := redactPrinter(old.Printers[0])
	if rp.Plug.LocalKey != "" || !rp.PlugSecretSet || old.Printers[0].Plug.LocalKey != key {
		t.Fatalf("redact: %+v set=%v", rp.Plug, rp.PlugSecretSet)
	}
	body, _ := json.Marshal(map[string]any{"printers": []redactedPrinter{rp}})
	got, err := resolveConfigUpdate(old, body)
	if err != nil || got.Printers[0].Plug.LocalKey != key {
		t.Fatalf("round trip lost the key: %v %+v", err, got.Printers[0].Plug)
	}

	// A different device must not inherit the old key.
	rp.Plug.DeviceID = "dev2"
	body, _ = json.Marshal(map[string]any{"printers": []redactedPrinter{rp}})
	got, _ = resolveConfigUpdate(old, body)
	if got.Printers[0].Plug.LocalKey != "" {
		t.Fatal("key leaked to a different device")
	}
}

// A Tapo password and a Home Assistant token are redacted and kept the same
// way, and never cross to another kind or address.
func TestPlugPasswordAndTokenKept(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "a", Name: "P1", IP: "192.168.1.9", Plug: &config.Plug{Kind: "tapo", Address: "192.168.1.81", Username: "me@example.com", Password: " pw with spaces "}},
		{ID: "b", Name: "P2", IP: "192.168.1.10", Plug: &config.Plug{Kind: "home_assistant", Address: "http://ha.local:8123", Token: "tok", Entity: "switch.p2"}},
	}}
	rps := []redactedPrinter{redactPrinter(old.Printers[0]), redactPrinter(old.Printers[1])}
	for _, rp := range rps {
		if rp.Plug.Password != "" || rp.Plug.Token != "" || !rp.PlugSecretSet {
			t.Fatalf("redact: %+v", rp.Plug)
		}
	}
	body, _ := json.Marshal(map[string]any{"printers": rps})
	got, err := resolveConfigUpdate(old, body)
	if err != nil || got.Printers[0].Plug.Password != " pw with spaces " || got.Printers[1].Plug.Token != "tok" {
		t.Fatalf("round trip: %v %+v %+v", err, got.Printers[0].Plug, got.Printers[1].Plug)
	}

	rps[0].Plug.Address = "192.168.1.99" // another plug: no password carried over
	rps[1].Plug.Kind = "esphome"         // another kind: no token, and token dropped
	body, _ = json.Marshal(map[string]any{"printers": rps})
	got, _ = resolveConfigUpdate(old, body)
	if got.Printers[0].Plug.Password != "" || got.Printers[1].Plug.Token != "" {
		t.Fatalf("secret leaked: %+v %+v", got.Printers[0].Plug, got.Printers[1].Plug)
	}
}
