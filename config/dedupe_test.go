package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func names(ps []Printer) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func TestDedupePrinterNames(t *testing.T) {
	ps := []Printer{
		{ID: "a", Name: "Ender 3"},
		{ID: "b", Name: "ender 3 "},
		{ID: "c", Name: "Ender 3 (2)"},
		{ID: "d", Name: "Ender 3"},
		{ID: "e", Name: "Prusa", PreviousNames: []string{"Ender 3 (3)"}},
		{ID: "f", Name: ""},
		{ID: "g", Name: ""},
	}
	if !dedupePrinterNames(ps) {
		t.Fatal("nothing changed")
	}
	// (2) is a printer's name and (3) another's old name: both are skipped.
	want := []string{"Ender 3", "ender 3 (4)", "Ender 3 (2)", "Ender 3 (5)", "Prusa", "", ""}
	if got := names(ps); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("names = %q, want %q", got, want)
	}
	if dedupePrinterNames(ps) {
		t.Error("a second pass changed names again")
	}
	if dedupePrinterNames([]Printer{{Name: "A"}, {Name: "B"}}) {
		t.Error("unique names were changed")
	}
}

func TestDedupePrinterNames_LongNameStaysInLimit(t *testing.T) {
	long := strings.Repeat("x", maxPrinterNameRunes)
	ps := []Printer{{Name: long}, {Name: long}}
	dedupePrinterNames(ps)
	if got := len([]rune(ps[1].Name)); got > maxPrinterNameRunes || ps[1].Name == long {
		t.Fatalf("renamed name has %d runes: %q", got, ps[1].Name)
	}
}

// A config from before the uniqueness check, with two printers under one
// name, loads with the second renamed, keeps both printers and their relay
// identity, and is saved that way.
func TestLoadConfig_RenamesDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	before := []Printer{
		{ID: "id-1", Name: "Ender 3", MoonrakerURL: "http://10.0.0.1"},
		{ID: "id-2", Name: "Ender 3", MoonrakerURL: "http://10.0.0.2"},
		{ID: "id-3", Name: "P1S", Serial: "01P00A000000001", IP: "10.0.0.3", LANCode: "12345678"},
		{ID: "id-4", Name: "p1s", Serial: "01P00A000000002", IP: "10.0.0.4", LANCode: "12345678"},
	}
	data, _ := json.Marshal(Config{Printers: before})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Ender 3", "Ender 3 (2)", "P1S", "p1s (2)"}
	if got := names(cfg.Printers); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("names = %q, want %q", got, want)
	}
	for i, p := range cfg.Printers {
		gotSerial, _ := p.RelayIdentity()
		wantSerial, _ := before[i].RelayIdentity()
		if gotSerial != wantSerial || p.ID != before[i].ID {
			t.Errorf("printer %d: relay identity %q id %q, want %q %q", i, gotSerial, p.ID, wantSerial, before[i].ID)
		}
		if p.MoonrakerURL != before[i].MoonrakerURL || p.LANCode != before[i].LANCode {
			t.Errorf("printer %d lost settings: %+v", i, p)
		}
	}

	// On disk too, and a second load changes nothing more.
	again, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(again.Printers); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("names after a second load = %q, want %q", got, want)
	}
	var onDisk Config
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := names(onDisk.Printers); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("names on disk = %q, want %q", got, want)
	}
}
