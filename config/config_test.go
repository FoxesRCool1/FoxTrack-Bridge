package config

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeLegacyConfig writes a config.json at the preferred (XDG) path so
// LoadConfig reads it directly, without touching the real legacy-path fallback.
func writeLegacyConfig(t *testing.T, xdgHome string, printers []map[string]string) string {
	t.Helper()
	fbDir := filepath.Join(xdgHome, "FoxTrack-Bridge")
	if err := os.MkdirAll(fbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(fbDir, "config.json")
	body := map[string]interface{}{
		"api_key":  "k",
		"printers": printers,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return cfgPath
}

func TestLoadConfig_BackfillsMissingIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeLegacyConfig(t, dir, []map[string]string{
		{"name": "Monsieur"},
		{"name": "Sherlock"},
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Printers) != 2 {
		t.Fatalf("printers = %d, want 2", len(cfg.Printers))
	}
	if cfg.Printers[0].ID == "" || cfg.Printers[1].ID == "" {
		t.Fatalf("expected non-empty IDs, got %+v", cfg.Printers)
	}
	if cfg.Printers[0].ID == cfg.Printers[1].ID {
		t.Fatalf("expected distinct IDs, both got %q", cfg.Printers[0].ID)
	}

	// Confirm the backfill was persisted to disk, not just held in memory.
	onDisk, err := LoadConfig()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if onDisk.Printers[0].ID != cfg.Printers[0].ID || onDisk.Printers[1].ID != cfg.Printers[1].ID {
		t.Fatalf("backfilled IDs were not persisted: got %+v, want %+v", onDisk.Printers, cfg.Printers)
	}
}

func TestLoadConfig_PreservesExistingIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeLegacyConfig(t, dir, []map[string]string{
		{"name": "Monsieur", "id": "already-here"},
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Printers[0].ID != "already-here" {
		t.Fatalf("ID = %q, want unchanged %q", cfg.Printers[0].ID, "already-here")
	}
}

func TestLoadConfig_PreviousNamesRoundTripsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeLegacyConfig(t, dir, []map[string]string{
		{"name": "Monsieur"},
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Printers[0].PreviousNames) != 0 {
		t.Fatalf("PreviousNames = %+v, want empty", cfg.Printers[0].PreviousNames)
	}

	data, err := os.ReadFile(filepath.Join(dir, "FoxTrack-Bridge", "config.json"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if strings.Contains(string(data), "previous_names") {
		t.Errorf("expected previous_names to be omitted from disk when empty, got: %s", data)
	}
}

// TestLoadConfig_BackfillSaveFailureIsLogged simulates a read-only config
// directory (e.g. a read-only container filesystem): the initial read
// succeeds, but persisting the backfilled IDs fails, and that failure must be
// logged loudly rather than swallowed.
func TestLoadConfig_BackfillSaveFailureIsLogged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based write failure is not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits")
	}

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeLegacyConfig(t, dir, []map[string]string{
		{"name": "Monsieur"},
	})

	fbDir := filepath.Join(dir, "FoxTrack-Bridge")
	if err := os.Chmod(fbDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(fbDir, 0755) // restore before t.TempDir() cleanup runs

	var buf bytes.Buffer
	prevOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOutput)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Printers[0].ID == "" {
		t.Fatalf("expected an ID to be assigned in memory even though the save failed")
	}
	if !strings.Contains(buf.String(), "WARNING: failed to persist backfilled printer IDs") {
		t.Fatalf("expected a loud warning about the failed backfill save, got log output: %s", buf.String())
	}
}

func TestKindLabel(t *testing.T) {
	cases := map[string]Printer{
		"Bambu Lab":      {Name: "a", Serial: "01P00A"},
		"Klipper":        {Name: "b", MoonrakerURL: "http://k:7125"},
		"PrusaLink":      {Name: "c", PrusaLinkURL: "http://p"},
		"Home Assistant": {Name: "d", HomeAssistantURL: "http://h:8123"},
	}
	for want, p := range cases {
		if got := p.KindLabel(); got != want {
			t.Errorf("%s: KindLabel = %q, want %q", p.Name, got, want)
		}
	}
}
