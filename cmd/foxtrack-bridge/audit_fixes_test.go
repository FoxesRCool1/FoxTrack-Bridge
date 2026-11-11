//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// A blank secret is kept only while it still goes to the machine it was saved
// for. A printer repointed elsewhere must not carry the old key along.
func TestApplyStoredSecrets_SecretsFollowTheAddressNotTheName(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "id-voron", Name: "Voron", MoonrakerURL: "http://voron.local:7125", APIKey: "moon-key"},
		{ID: "id-ha", Name: "HA", HomeAssistantURL: "http://ha.local:8123", HomeAssistantToken: "ha-token"},
		{ID: "id-x1", Name: "X1C", IP: "10.0.0.5", Serial: "01P00A", LANCode: "12345678"},
	}}

	same := &config.Config{Printers: []config.Printer{
		{ID: "id-voron", Name: "Voron", MoonrakerURL: "http://voron.local:7125/"},
		{ID: "id-ha", Name: "HA", HomeAssistantURL: "http://ha.local:8123"},
		{ID: "id-x1", Name: "X1C", IP: "10.0.0.9", Serial: "01p00a"}, // new DHCP address, same printer
	}}
	applyStoredSecrets(same, old)
	if same.Printers[0].APIKey != "moon-key" || same.Printers[1].HomeAssistantToken != "ha-token" || same.Printers[2].LANCode != "12345678" {
		t.Fatalf("same machine must keep its secrets: %+v", same.Printers)
	}

	moved := &config.Config{Printers: []config.Printer{
		{ID: "id-voron", Name: "Voron", MoonrakerURL: "http://attacker.example:7125"},
		{ID: "id-ha", Name: "HA", HomeAssistantURL: "http://other.local:8123"},
		{ID: "id-x1", Name: "X1C", IP: "10.0.0.5", Serial: "01P00B"}, // another printer
	}}
	applyStoredSecrets(moved, old)
	if moved.Printers[0].APIKey != "" || moved.Printers[1].HomeAssistantToken != "" || moved.Printers[2].LANCode != "" {
		t.Fatalf("a changed address must not inherit the old secret: %+v", moved.Printers)
	}
}

// Only an ID-less payload may match a stored printer by name; a new printer
// that reuses an old name starts clean. The ID-less match adopts the stored ID.
func TestApplyStoredSecrets_NameMatchOnlyWithoutID(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "id-old", Name: "Voron", MoonrakerURL: "http://voron.local:7125", APIKey: "moon-key"},
	}}
	fresh := &config.Config{Printers: []config.Printer{{ID: "id-new", Name: "Voron", MoonrakerURL: "http://voron.local:7125"}}}
	applyStoredSecrets(fresh, old)
	if fresh.Printers[0].APIKey != "" {
		t.Fatalf("a new ID with an old name inherited the key")
	}
	legacy := &config.Config{Printers: []config.Printer{{Name: "Voron", MoonrakerURL: "http://voron.local:7125"}}}
	applyStoredSecrets(legacy, old)
	if legacy.Printers[0].APIKey != "moon-key" || legacy.Printers[0].ID != "id-old" {
		t.Fatalf("an ID-less payload must match by name and take the stored ID: %+v", legacy.Printers[0])
	}
}

// The assistant key follows the same rule as /api/ai/settings: a new provider
// or base URL with no new key drops the old one.
func TestApplyStoredSecrets_AIKeyStaysWithItsProvider(t *testing.T) {
	old := &config.Config{AI: &config.AI{Preset: "custom", BaseURL: "https://a.example/v1", APIKey: "k"}}
	same := &config.Config{AI: &config.AI{Preset: "custom", BaseURL: "https://a.example/v1/"}}
	applyStoredSecrets(same, old)
	if same.AI.APIKey != "k" {
		t.Fatalf("same provider must keep the key, got %q", same.AI.APIKey)
	}
	moved := &config.Config{AI: &config.AI{Preset: "custom", BaseURL: "https://evil.example/v1"}}
	applyStoredSecrets(moved, old)
	if moved.AI.APIKey != "" {
		t.Fatalf("a new base URL must not get the old key, got %q", moved.AI.APIKey)
	}
}

func TestResolveConfigUpdate_ClearFoxTrackTokenUnlinks(t *testing.T) {
	old := &config.Config{APIKey: "legacy", FoxTrack2APIKey: "ftb_current", Printers: twoPrinters()}
	kept, err := resolveConfigUpdate(old, []byte(`{"auto_update":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if kept.FoxTrack2APIKey != "ftb_current" || kept.APIKey != "legacy" {
		t.Fatalf("a blank token must keep the stored one: %q %q", kept.FoxTrack2APIKey, kept.APIKey)
	}
	cleared, err := resolveConfigUpdate(old, []byte(`{"clear_foxtrack_token":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.FoxTrack2APIKey != "" || cleared.APIKey != "" {
		t.Fatalf("clear_foxtrack_token must remove both tokens: %q %q", cleared.FoxTrack2APIKey, cleared.APIKey)
	}
	if len(cleared.Printers) != 2 {
		t.Fatalf("unlinking must not touch printers: %d", len(cleared.Printers))
	}
}

func TestPrinterNames_LimitedTo128Characters(t *testing.T) {
	long := strings.Repeat("é", 129)
	ok := strings.Repeat("é", 128)
	body, _ := json.Marshal(map[string]any{"printers": []config.Printer{{Name: long, IP: "10.0.0.1"}}})
	if _, err := resolveConfigUpdate(&config.Config{}, body); !errors.Is(err, errPrinterNameTooLong) {
		t.Fatalf("129-character name: err = %v, want errPrinterNameTooLong", err)
	}
	body, _ = json.Marshal(map[string]any{"printers": []config.Printer{{Name: ok, IP: "10.0.0.1"}}})
	if _, err := resolveConfigUpdate(&config.Config{}, body); err != nil {
		t.Fatalf("128-character name refused: %v", err)
	}
	old := &config.Config{Printers: []config.Printer{{ID: "k", Name: "Voron", MoonrakerURL: "http://v.local"}}}
	if _, _, err := applyPrinterEdit(old, "k", printerEdit{Name: strp(long)}, nil); !errors.Is(err, errPrinterNameTooLong) {
		t.Fatalf("edit to a 129-character name: err = %v, want errPrinterNameTooLong", err)
	}
}

func TestExportableConfig_BlanksVirtualPrinterKey(t *testing.T) {
	cfg := &config.Config{VirtualPrinter: &config.VirtualPrinter{Enabled: true, APIKey: "vp-secret"}}
	if out := exportableConfig(cfg); out.VirtualPrinter == nil || out.VirtualPrinter.APIKey != "" {
		t.Fatalf("virtual printer key exported: %+v", out.VirtualPrinter)
	}
	if cfg.VirtualPrinter.APIKey != "vp-secret" {
		t.Fatal("export changed the stored config")
	}
}

// A backup from before a feature existed must not switch that feature off.
func TestResolveConfigImport_KeepsFeatureBlocksTheFileLacks(t *testing.T) {
	old := transferConfig()
	old.Notifications = &config.Notifications{Channels: []config.NotifyChannel{{ID: "n1", Kind: "telegram", Token: "bot", Target: "1"}}}
	old.Costs = &config.Costs{}
	got, err := resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Notifications == nil || len(got.Notifications.Channels) != 1 || got.Costs == nil {
		t.Fatalf("feature blocks absent from the file were dropped: %+v", got)
	}
}

// A notification token is carried to an imported channel only while its URL is
// unchanged, whatever the kind.
func TestKeepImportedSecrets_TokenStaysWithItsURL(t *testing.T) {
	old := &config.Config{Notifications: &config.Notifications{Channels: []config.NotifyChannel{
		{ID: "w1", Kind: "webhook", URL: "https://hooks.example/a", Token: "t"},
	}}}
	same := &config.Config{Notifications: &config.Notifications{Channels: []config.NotifyChannel{
		{ID: "w1", Kind: "webhook", URL: "https://hooks.example/a"},
	}}}
	keepImportedSecrets(same, old)
	if same.Notifications.Channels[0].Token != "t" {
		t.Fatal("same URL must keep the token")
	}
	moved := &config.Config{Notifications: &config.Notifications{Channels: []config.NotifyChannel{
		{ID: "w1", Kind: "webhook", URL: "https://elsewhere.example/b"},
	}}}
	keepImportedSecrets(moved, old)
	if moved.Notifications.Channels[0].Token != "" {
		t.Fatalf("a changed URL must not carry the token: %q", moved.Notifications.Channels[0].Token)
	}
}
