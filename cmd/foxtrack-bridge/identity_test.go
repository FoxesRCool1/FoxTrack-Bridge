package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"foxtrack-bridge/config"
	"foxtrack-bridge/webhook"
)

func TestMatchesExternalID(t *testing.T) {
	klipper := config.Printer{ID: "id-k", Name: "Voron", MoonrakerURL: "http://v.local"}
	bambu := config.Printer{ID: "id-b", Name: "P1S", Serial: "01P00A", LANCode: "1234"}
	cases := []struct {
		name  string
		p     config.Printer
		bambu bool
		id    string
		want  bool
	}{
		{"klipper by ID", klipper, false, "id-k", true},
		{"klipper by name, as older servers send", klipper, false, "Voron", true},
		{"klipper, other", klipper, false, "Prusa", false},
		{"klipper with an empty id never matches an empty external id", config.Printer{Name: "X"}, false, "", false},
		{"bambu by serial", bambu, true, "01P00A", true},
		{"bambu is not matched by name or ID", bambu, true, "P1S", false},
		{"bambu is not matched by ID", bambu, true, "id-b", false},
	}
	for _, c := range cases {
		if got := matchesExternalID(c.p, c.bambu, c.id); got != c.want {
			t.Errorf("%s: matchesExternalID = %v, want %v", c.name, got, c.want)
		}
	}
}

func withConfigStore(t *testing.T, cfg *config.Config) {
	t.Helper()
	configMutex.Lock()
	old := configStore
	configStore = cfg
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
	})
}

// A command or a plug poll that arrives before the config is loaded (or after a
// test reset it) finds no printer; it must not crash.
func TestNilConfigStore_DoesNotCrash(t *testing.T) {
	withConfigStore(t, nil)
	if err := executeBridgeCommand(bridgeCommand{ID: "c", ExternalID: "x", Command: "pause"}); !errors.Is(err, errNoMatchingPrinter) {
		t.Errorf("executeBridgeCommand with no config = %v, want errNoMatchingPrinter", err)
	}
	if printerIsBambu("x") {
		t.Error("printerIsBambu with no config must be false")
	}
}

// FoxTrack now sends the config ID for a non-Bambu printer; older servers send
// the name. Both reach the printer; anything else does not.
func TestExecuteBridgeCommand_FindsKlipperByIDOrName(t *testing.T) {
	withConfigStore(t, &config.Config{Printers: []config.Printer{
		{ID: "id-k", Name: "Voron-id-test", MoonrakerURL: "http://127.0.0.1:1"},
	}})
	for _, ext := range []string{"id-k", "Voron-id-test"} {
		err := executeBridgeCommand(bridgeCommand{ID: "c", ExternalID: ext, Command: "pause"})
		if errors.Is(err, errNoMatchingPrinter) {
			t.Errorf("external_id %q did not find the printer", ext)
		}
	}
	if err := executeBridgeCommand(bridgeCommand{ID: "c", ExternalID: "nope", Command: "pause"}); !errors.Is(err, errNoMatchingPrinter) {
		t.Errorf("an unknown external_id = %v, want errNoMatchingPrinter", err)
	}
	if _, _, _, ok := findBridgePrinter("id-k"); !ok {
		t.Error("findBridgePrinter by ID failed")
	}
	if _, _, _, ok := findBridgePrinter("Voron-id-test"); !ok {
		t.Error("findBridgePrinter by name failed")
	}
}

// The plug-only report says nothing about alarms (the key is absent, which the
// server reads as no change) and has no remaining time.
func TestPlugOnlyPayload_LeavesAlarmsOut(t *testing.T) {
	p := plugOnlyPayload(config.Printer{Name: "Voron", MoonrakerURL: "http://v.local"}, &webhook.RelayPlug{On: false, Reachable: true})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"alarms"`) {
		t.Errorf("plug-only report must leave the alarms key out: %s", b)
	}
	if !strings.Contains(string(b), `"mc_remaining_time":-1`) || !strings.Contains(string(b), `"gcode_state":"OFFLINE"`) {
		t.Errorf("plug-only report = %s", b)
	}
}
