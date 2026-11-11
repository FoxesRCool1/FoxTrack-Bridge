//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// blockConfigFile points the config dir at a fresh temp dir in which
// config.json is a directory, so SaveConfig cannot rename its file into place
// while the dir itself (and the before-import copy) can still be written.
func blockConfigFile(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(config.ConfigDir(), "config.json"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigImport_RefusesBlankPrinterName(t *testing.T) {
	old := transferConfig()
	_, err := resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"},{"name":"   ","ip":"10.0.0.3"}]}`), false)
	if !errors.Is(err, errBlankPrinterName) {
		t.Fatalf("err = %v, want errBlankPrinterName", err)
	}
}

func TestConfigImport_BlankPrinterNameIs400AndKeepsConfig(t *testing.T) {
	old := transferConfig()
	configMutex.Lock()
	saved := configStore
	configStore = old
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = saved
		configMutex.Unlock()
	})

	req := httptest.NewRequest("POST", "/api/config/import", strings.NewReader(`{"printers":[{"name":" "}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleConfigImport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore != old {
		t.Fatalf("configStore changed by a refused import")
	}
}

func TestResolveConfigImport_KeepsVirtualPrinterWhenAbsent(t *testing.T) {
	old := transferConfig()
	old.VirtualPrinter = &config.VirtualPrinter{Enabled: true, Port: 8091, APIKey: "vp-key"}
	got, err := resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.VirtualPrinter == nil || got.VirtualPrinter.APIKey != "vp-key" || !got.VirtualPrinter.Enabled {
		t.Fatalf("virtual printer = %+v, want the current one kept", got.VirtualPrinter)
	}
}

func TestConfigImport_SaveFailureKeepsOldConfig(t *testing.T) {
	blockConfigFile(t)
	old := transferConfig()
	configMutex.Lock()
	saved := configStore
	configStore = old
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = saved
		configMutex.Unlock()
	})

	req := httptest.NewRequest("POST", "/api/config/import", strings.NewReader(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleConfigImport(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore != old {
		t.Fatalf("a failed save must leave the old config in memory")
	}
}

func TestSaveBeforeImport_PrunesOldCopies(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var fakes []string
	for i := 1; i <= 7; i++ {
		name := fmt.Sprintf("config.json.before-import-202601%02d-000000.000", i)
		fakes = append(fakes, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

	name, err := saveBeforeImport(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	copies := 0
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "config.json.before-import-") {
			copies++
		}
	}
	if copies != 5 {
		t.Fatalf("before-import copies = %d, want 5", copies)
	}
	if !present[name] {
		t.Fatalf("the copy just written (%s) was pruned", name)
	}
	for _, f := range fakes[:3] {
		if present[f] {
			t.Errorf("old copy %s was not pruned", f)
		}
	}
	for _, f := range fakes[3:] {
		if !present[f] {
			t.Errorf("recent copy %s was pruned", f)
		}
	}
	if !present["notes.txt"] {
		t.Fatalf("an unrelated file was removed")
	}
}

func TestSaveBeforeImport_NamesDifferWithinASecond(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	first, err := saveBeforeImport(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := saveBeforeImport(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two restores gave the same copy name %s", first)
	}
}

func transferConfig() *config.Config {
	return &config.Config{
		APIKey:   "ft-key",
		Printers: twoPrinters(),
		Security: &config.Security{PasswordHash: "hash"},
		BambuCloud: &config.BambuCloud{
			Email: "a@example.com", AccessToken: "cloud-token",
		},
		AI: &config.AI{Preset: "openai", Model: "m", APIKey: "sk-secret"},
	}
}

// secretConfig is transferConfig with a secret in every place the Bridge keeps
// one, each with a marker the export must not carry.
func secretConfig() *config.Config {
	cfg := transferConfig()
	cfg.APIKey, cfg.FoxTrack2APIKey = "SECRET-legacy-key", "SECRET-ft2-key"
	cfg.Printers[0].ID, cfg.Printers[1].ID = "id-monsieur", "id-sherlock"
	cfg.Printers[0].LANCode = "SECRET-lan-1"
	cfg.Printers[1].LANCode = "SECRET-lan-2"
	cfg.Printers = append(cfg.Printers,
		config.Printer{ID: "id-voron", Name: "Voron", MoonrakerURL: "http://v.local", APIKey: "SECRET-moon",
			Plug: &config.Plug{Kind: config.PlugTuya, Address: "10.0.0.9", DeviceID: "dev1", LocalKey: "SECRET-tuya", Password: "SECRET-plugpw", Token: "SECRET-plugtoken"}},
		config.Printer{ID: "id-ha", Name: "HA", HomeAssistantURL: "http://ha.local", HomeAssistantToken: "SECRET-hatoken"},
	)
	cfg.Notifications = &config.Notifications{Channels: []config.NotifyChannel{
		{ID: "n1", Kind: "telegram", Token: "SECRET-bot", Target: "123"},
		{ID: "n2", Kind: "email", SMTP: &config.SMTP{Host: "smtp.example.com", Username: "me", Password: "SECRET-smtp"}},
	}}
	cfg.MQTTExport = &config.MQTTExport{Broker: "tcp://broker:1883", Username: "u", Password: "SECRET-mqtt"}
	return cfg
}

// The export holds no password, token or key (the file may be mailed or kept in
// a cloud folder), and exporting changes nothing in the stored config.
func TestExportableConfig_HoldsNoSecret(t *testing.T) {
	cfg := secretConfig()
	out := exportableConfig(cfg)
	if out.Security != nil || out.BambuCloud != nil {
		t.Fatalf("security/bambu_cloud must not be exported")
	}
	if out.AI == nil || out.AI.APIKey != "" || out.AI.Model != "m" {
		t.Fatalf("ai = %+v, want settings without the key", out.AI)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"SECRET", "sk-secret", "cloud-token", "81f1aafd", "f849c383", "ft-key"} {
		if strings.Contains(string(data), leaked) {
			t.Errorf("export holds %q: %s", leaked, data)
		}
	}
	// What is not secret stays, so a restore brings the printers back.
	if out.Printers[0].Name != "Monsieur" || out.Printers[0].Serial != "01P00C580801716" || out.Printers[2].Plug.DeviceID != "dev1" ||
		out.Notifications.Channels[0].Target != "123" || out.MQTTExport.Broker != "tcp://broker:1883" {
		t.Fatalf("export lost settings that are not secret: %s", data)
	}
	if cfg.AI.APIKey != "sk-secret" || cfg.Security == nil || cfg.APIKey != "SECRET-legacy-key" ||
		cfg.Printers[0].LANCode != "SECRET-lan-1" || cfg.Printers[2].Plug.LocalKey != "SECRET-tuya" ||
		cfg.Notifications.Channels[0].Token != "SECRET-bot" || cfg.MQTTExport.Password != "SECRET-mqtt" {
		t.Fatalf("export mutated the stored config")
	}
}

// Restoring an export onto the install it came from asks for nothing again: the
// stored secret stays wherever the file has none, matched by printer ID, channel
// ID and broker. A secret does not follow a changed address.
func TestResolveConfigImport_KeepsStoredSecretsTheExportLacks(t *testing.T) {
	old := secretConfig()
	inner, _ := json.Marshal(exportableConfig(old))
	file, _ := json.Marshal(configBackup{Format: backupFormat, Version: 1, Config: inner})
	got, err := resolveConfigImport(old, file, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "SECRET-legacy-key" || got.FoxTrack2APIKey != "SECRET-ft2-key" {
		t.Errorf("FoxTrack keys = %q %q", got.APIKey, got.FoxTrack2APIKey)
	}
	if got.Printers[0].LANCode != "SECRET-lan-1" || got.Printers[1].LANCode != "SECRET-lan-2" ||
		got.Printers[2].APIKey != "SECRET-moon" || got.Printers[3].HomeAssistantToken != "SECRET-hatoken" {
		t.Errorf("printer secrets = %+v", got.Printers)
	}
	if pl := got.Printers[2].Plug; pl == nil || pl.LocalKey != "SECRET-tuya" || pl.Password != "SECRET-plugpw" || pl.Token != "SECRET-plugtoken" {
		t.Errorf("plug secrets = %+v", pl)
	}
	if got.Notifications.Channels[0].Token != "SECRET-bot" || got.Notifications.Channels[1].SMTP.Password != "SECRET-smtp" {
		t.Errorf("notification secrets = %+v", got.Notifications.Channels)
	}
	if got.MQTTExport.Password != "SECRET-mqtt" {
		t.Errorf("mqtt password = %q", got.MQTTExport.Password)
	}

	// A file that points the broker somewhere else must not carry the password along.
	moved := strings.Replace(string(file), "tcp://broker:1883", "tcp://elsewhere:1883", 1)
	got, err = resolveConfigImport(old, []byte(moved), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.MQTTExport.Password != "" {
		t.Errorf("mqtt password followed a changed broker: %q", got.MQTTExport.Password)
	}
}

func TestResolveConfigImport_KeepsAccountSecretsAndRefusesClear(t *testing.T) {
	old := transferConfig()

	// An export round trip restores the printers and keeps what export left out.
	inner, _ := json.Marshal(exportableConfig(old))
	file, _ := json.Marshal(configBackup{Format: backupFormat, Version: 1, Config: inner})
	got, err := resolveConfigImport(old, file, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Printers) != 2 || got.Printers[1].LANCode != "f849c383" {
		t.Fatalf("printers = %+v", got.Printers)
	}
	if got.Security != old.Security || got.BambuCloud != old.BambuCloud {
		t.Fatalf("import must keep the current password and Bambu account")
	}
	if got.AI == nil || got.AI.APIKey != "sk-secret" {
		t.Fatalf("same provider: key must be kept, got %+v", got.AI)
	}

	// A plain config.json (e.g. a daily copy) is accepted too, and its own
	// security block is ignored.
	raw := []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}],"security":{"password_hash":"old"}}`)
	got, err = resolveConfigImport(old, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Printers) != 1 || got.Printers[0].ID == "" || got.Security != old.Security {
		t.Fatalf("plain config import: %+v", got)
	}

	// Empty printer list over saved printers: refused unless confirmed.
	if _, err := resolveConfigImport(old, []byte(`{"printers":[]}`), false); !errors.Is(err, errRefuseClearPrinters) {
		t.Fatalf("err = %v, want errRefuseClearPrinters", err)
	}
	if _, err := resolveConfigImport(old, []byte(`{"printers":[]}`), true); err != nil {
		t.Fatalf("confirmed clear: %v", err)
	}

	// Not a backup at all.
	for _, body := range []string{`not json`, `{"foo":1}`, `{"format":"other","config":{"printers":[]}}`, `[]`} {
		if _, err := resolveConfigImport(old, []byte(body), true); !errors.Is(err, errNotABackup) {
			t.Errorf("%s: err = %v, want errNotABackup", body, err)
		}
	}

	// Duplicate names inside the file.
	dup := []byte(`{"printers":[{"name":"A"},{"name":"a"}]}`)
	if _, err := resolveConfigImport(old, dup, false); !errors.Is(err, errDuplicatePrinterName) {
		t.Fatalf("err = %v, want errDuplicatePrinterName", err)
	}
}

func TestResolveConfigImport_KeepsFoxTrackKeysTheFileLacks(t *testing.T) {
	old := transferConfig()
	old.APIKey, old.FoxTrack2APIKey = "ft1-current", "ft2-current"
	got, err := resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "ft1-current" || got.FoxTrack2APIKey != "ft2-current" {
		t.Fatalf("keys = %q %q, want the current ones kept", got.APIKey, got.FoxTrack2APIKey)
	}
	// A linked install stays on its current token: an older file must not roll
	// it back to one that may since have been revoked.
	got, err = resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}],"foxtrack2_api_key":"ft2-file"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.FoxTrack2APIKey != "ft2-current" {
		t.Fatalf("FoxTrack2APIKey = %q, want the current token kept", got.FoxTrack2APIKey)
	}
	// With nothing stored, the file's token is taken.
	old.APIKey, old.FoxTrack2APIKey = "", ""
	got, err = resolveConfigImport(old, []byte(`{"printers":[{"name":"Solo","ip":"10.0.0.2"}],"foxtrack2_api_key":"ft2-file"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.FoxTrack2APIKey != "ft2-file" {
		t.Fatalf("FoxTrack2APIKey = %q, want the file's key when none is stored", got.FoxTrack2APIKey)
	}
}

func TestMergeImportedAI_KeyOnlyForSameProvider(t *testing.T) {
	old := &config.AI{Preset: "custom", BaseURL: "https://a.example/v1", APIKey: "k"}
	if got := mergeImportedAI(&config.AI{Preset: "custom", BaseURL: "https://evil.example/v1"}, old); got.APIKey != "" {
		t.Fatalf("key went to a different endpoint")
	}
	if got := mergeImportedAI(&config.AI{Preset: "custom", BaseURL: "https://a.example/v1/"}, old); got.APIKey != "k" {
		t.Fatalf("same endpoint must keep the key")
	}
	if got := mergeImportedAI(nil, old); got != old {
		t.Fatalf("absent block must keep the current one")
	}
}

func TestConfigExportImportHandlers(t *testing.T) {
	configMutex.Lock()
	saved := configStore
	configStore = transferConfig()
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = saved
		configMutex.Unlock()
	})

	rec := httptest.NewRecorder()
	handleConfigExport(rec, httptest.NewRequest("GET", "/api/config/export", nil))
	if rec.Code != 200 {
		t.Fatalf("export status %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	body := rec.Body.String()
	for _, leaked := range []string{"cloud-token", "sk-secret", `"security"`, "ft-key", "81f1aafd", "f849c383"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("export leaked %q", leaked)
		}
	}

	// Wrong content type is refused.
	rec = httptest.NewRecorder()
	handleConfigImport(rec, httptest.NewRequest("POST", "/api/config/import", strings.NewReader(body)))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("no content type: status %d", rec.Code)
	}

	req := httptest.NewRequest("POST", "/api/config/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handleConfigImport(rec, req)
	if rec.Code != 200 {
		t.Fatalf("import status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Printers int    `json:"printers"`
		Backup   string `json:"backup"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Printers != 2 || resp.Backup == "" {
		t.Fatalf("resp = %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(config.ConfigDir(), resp.Backup)); err != nil {
		t.Fatalf("pre-import copy missing: %v", err)
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.Security == nil || configStore.BambuCloud == nil || configStore.AI.APIKey != "sk-secret" {
		t.Fatalf("import dropped the current account secrets")
	}
}
