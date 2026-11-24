package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useLegacyDir points the preferred location at an empty folder and hands back
// the folder the old location lives in (next to the test binary), removed again
// when the test ends. The folder is not created.
func useLegacyDir(t *testing.T) (xdgHome, legacyDir string) {
	t.Helper()
	xdgHome = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgHome)
	legacyDir = filepath.Dir(legacyConfigPath())
	if _, err := os.Lstat(legacyDir); err == nil {
		t.Skipf("%s already exists; not touching it", legacyDir)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(legacyDir, "config.json"), 0600)
		os.RemoveAll(legacyDir)
	})
	return xdgHome, legacyDir
}

func writeLegacyLocation(t *testing.T, legacyDir, body string) string {
	t.Helper()
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(legacyDir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// An older build must not strip what a newer build wrote: a config that needs
// no change in memory is left byte for byte as it was.
func TestLoadConfig_UnchangedFileIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	fbDir := filepath.Join(dir, "FoxTrack-Bridge")
	if err := os.MkdirAll(fbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(fbDir, "config.json")
	body := []byte(`{"api_key":"k","from_a_newer_build":{"keep":true},"printers":[{"id":"p1","name":"Monsieur","newer_field":7}]}`)
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Printers) != 1 || cfg.Printers[0].ID != "p1" {
		t.Fatalf("printers = %+v, want the saved one", cfg.Printers)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("an unchanged config was rewritten on load:\n got %s\nwant %s", got, body)
	}
}

func TestLoadConfig_LegacyLocationIsMigrated(t *testing.T) {
	xdgHome, legacyDir := useLegacyDir(t)
	legacy := writeLegacyLocation(t, legacyDir, `{"api_key":"k","printers":[{"id":"p1","name":"Monsieur"}]}`)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Printers) != 1 || cfg.Printers[0].Name != "Monsieur" {
		t.Fatalf("printers = %+v, want the one from the old location", cfg.Printers)
	}
	migrated, err := os.ReadFile(filepath.Join(xdgHome, "FoxTrack-Bridge", "config.json"))
	if err != nil {
		t.Fatalf("the config was not written to the preferred location: %v", err)
	}
	if !strings.Contains(string(migrated), `"Monsieur"`) || !strings.Contains(string(migrated), `"p1"`) {
		t.Fatalf("migrated config lost the printer: %s", migrated)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("the old config must stay where it was: %v", err)
	}
}

// A corrupt preferred file with a good old one behind it: the old one is used
// and written back to the preferred location, next to the backup.
func TestLoadConfig_CorruptPreferredFallsBackAndMigrates(t *testing.T) {
	xdgHome, legacyDir := useLegacyDir(t)
	writeLegacyLocation(t, legacyDir, `{"api_key":"k","printers":[{"id":"p1","name":"Monsieur"}]}`)
	fbDir := filepath.Join(xdgHome, "FoxTrack-Bridge")
	if err := os.MkdirAll(fbDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	preferred := filepath.Join(fbDir, "config.json")
	if err := os.WriteFile(preferred, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Printers) != 1 || cfg.Printers[0].ID != "p1" {
		t.Fatalf("printers = %+v, want the one from the old location", cfg.Printers)
	}
	data, err := os.ReadFile(preferred)
	if err != nil || !strings.Contains(string(data), `"Monsieur"`) {
		t.Fatalf("preferred config was not rewritten from the old one: %v %s", err, data)
	}
	backups, _ := filepath.Glob(preferred + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatalf("corrupt backups = %v, want exactly one", backups)
	}
	if kept, _ := os.ReadFile(backups[0]); string(kept) != "{not json" {
		t.Fatalf("the corrupt file was not preserved as it was: %q", kept)
	}
}

// An old config that exists but cannot be read is not "no config": reporting
// it as missing starts the Bridge empty, and the next save then creates a new
// file without the user's printers.
func TestLoadConfig_UnreadableLegacyIsNotAFreshStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based read failure is not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits")
	}
	xdgHome, legacyDir := useLegacyDir(t)
	legacy := writeLegacyLocation(t, legacyDir, `{"api_key":"k","printers":[{"id":"p1","name":"Monsieur"}]}`)
	if err := os.Chmod(legacy, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	cfg, err := LoadConfig()
	if err == nil {
		t.Fatalf("LoadConfig = %+v, want an error", cfg)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v: an unreadable old config was reported as no config at all", err)
	}
	if errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v: an unreadable file must not be treated as corrupt (it would be moved aside)", err)
	}
	if !strings.Contains(err.Error(), legacy) {
		t.Errorf("err = %v, want it to name %s", err, legacy)
	}
	if _, statErr := os.Stat(filepath.Join(xdgHome, "FoxTrack-Bridge", "config.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a new config was created although the old one could not be read (stat: %v)", statErr)
	}
}

func TestLoadConfig_NoConfigAnywhereIsAFreshStart(t *testing.T) {
	useLegacyDir(t)
	if _, err := LoadConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

// A file named "config" next to the binary is not an old config folder; the
// Bridge must still start fresh instead of refusing to start.
func TestLoadConfig_FileInPlaceOfLegacyFolderIsAFreshStart(t *testing.T) {
	_, legacyDir := useLegacyDir(t)
	if err := os.WriteFile(legacyDir, []byte("not a folder"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadConfig(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}
