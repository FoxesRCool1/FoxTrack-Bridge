package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
)

// readSavedPrinters returns the printer names in the config file on disk.
func readSavedPrinters(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	names := make([]string, len(cfg.Printers))
	for i, p := range cfg.Printers {
		names[i] = p.Name
	}
	return names
}

// An older snapshot saved after a newer one must not drop the newer changes.
func TestSaveConfig_OlderSnapshotNeverOverwritesNewer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	live := &Config{APIKey: "k", Printers: []Printer{{Name: "A"}}}
	older := live.Clone()
	live.Printers = append(live.Printers, Printer{Name: "B"})
	newer := live.Clone()

	if err := SaveConfig(newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}
	if err := SaveConfig(older); err != nil {
		t.Fatalf("save older: %v", err)
	}

	if got := readSavedPrinters(t); len(got) != 2 {
		t.Fatalf("printers on disk = %v, want [A B]", got)
	}

	// A config that did not come from Clone is always written.
	if err := SaveConfig(&Config{APIKey: "k", Printers: []Printer{{Name: "C"}}}); err != nil {
		t.Fatalf("save uncloned: %v", err)
	}
	if got := readSavedPrinters(t); len(got) != 1 || got[0] != "C" {
		t.Fatalf("printers on disk = %v, want [C]", got)
	}
}

// Mirrors the handlers: lock, add a printer, clone, unlock, save. However the
// saves interleave, the file must end up with every printer. Run with -race.
func TestSaveConfig_ConcurrentSavesKeepNewest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const n = 64
	var mu sync.Mutex
	live := &Config{APIKey: "k", Printers: []Printer{}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			mu.Lock()
			live.Printers = append(live.Printers, Printer{Name: fmt.Sprintf("P%d", i)})
			snap := live.Clone()
			mu.Unlock()
			errs <- SaveConfig(snap)
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	if got := readSavedPrinters(t); len(got) != n {
		t.Fatalf("printers on disk = %d, want %d", len(got), n)
	}
}

// A nil config must be refused, not written as "null" over a good file.
func TestSaveConfig_RefusesNil(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := SaveConfig(&Config{APIKey: "k", Printers: []Printer{{Name: "A"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := SaveConfig(nil); err == nil {
		t.Fatal("SaveConfig(nil) returned no error")
	}
	if got := readSavedPrinters(t); len(got) != 1 {
		t.Fatalf("printers on disk = %v, want [A]", got)
	}
}
