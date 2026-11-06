//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

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

func TestExportableConfig_LeavesOutAccountSecrets(t *testing.T) {
	cfg := transferConfig()
	out := exportableConfig(cfg)
	if out.Security != nil || out.BambuCloud != nil {
		t.Fatalf("security/bambu_cloud must not be exported")
	}
	if out.AI == nil || out.AI.APIKey != "" || out.AI.Model != "m" {
		t.Fatalf("ai = %+v, want settings without the key", out.AI)
	}
	if out.Printers[0].LANCode != "81f1aafd" {
		t.Fatalf("printer access codes must be exported for a restore")
	}
	if cfg.AI.APIKey != "sk-secret" || cfg.Security == nil {
		t.Fatalf("export mutated the stored config")
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
	if strings.Contains(body, "cloud-token") || strings.Contains(body, "sk-secret") || strings.Contains(body, `"security"`) {
		t.Fatalf("export leaked an account secret")
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
