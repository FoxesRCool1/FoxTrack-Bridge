package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A file that parses badly is ErrCorrupt (safe to move aside); an unreadable
// one is not, so the caller never backs up a file that may be fine.
func TestLoadConfig_CorruptVersusUnreadable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p := filepath.Join(dir, "FoxTrack-Bridge", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt file: got %v, want ErrCorrupt", err)
	}

	os.Remove(p)
	if err := os.Mkdir(p, 0755); err != nil { // a directory in the way: read fails
		t.Fatal(err)
	}
	_, err := LoadConfig()
	if err == nil || errors.Is(err, ErrCorrupt) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable file: got %v, want a plain read error", err)
	}
}

func TestBackupFile_NeverOverwritesABackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte("first"), 0600)
	b1, err := backupFile(p)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("second"), 0600)
	b2, err := backupFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if b1 == b2 {
		t.Fatalf("both backups went to %s", b1)
	}
	if got, _ := os.ReadFile(b1); string(got) != "first" {
		t.Fatalf("first backup holds %q", got)
	}
}
