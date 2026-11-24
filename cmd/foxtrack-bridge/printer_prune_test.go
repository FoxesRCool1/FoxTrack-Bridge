//go:build headless

package main

import (
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// Deleting a printer takes its ID out of everything that names printers by
// ID. A list that would become empty ("all printers") turns the feature off
// instead of widening it.
func TestForgetPrinterIDs(t *testing.T) {
	cfg := &config.Config{
		Notifications: &config.Notifications{Channels: []config.NotifyChannel{
			{ID: "all", Enabled: true},
			{ID: "both", Enabled: true, Printers: []string{"a", "b"}},
			{ID: "only-a", Enabled: true, Printers: []string{"a"}},
		}},
		Timelapse:   &config.Timelapse{Enabled: true, Printers: []string{"a"}},
		Security:    &config.Security{Shares: []config.ShareLink{{ID: "s1", PrinterID: "a"}, {ID: "s2", PrinterID: "b"}}},
		Maintenance: &config.Maintenance{Tasks: []config.MaintenanceTask{{ID: "t1", PrinterID: "a"}, {ID: "t2", PrinterID: "b"}}},
	}
	forgetPrinterIDs(cfg, map[string]bool{"a": true})

	ch := cfg.Notifications.Channels
	if !ch[0].Enabled || len(ch[0].Printers) != 0 {
		t.Errorf("a channel for all printers changed: %+v", ch[0])
	}
	if !ch[1].Enabled || len(ch[1].Printers) != 1 || ch[1].Printers[0] != "b" {
		t.Errorf("channel with two printers = %+v, want only b left", ch[1])
	}
	if ch[2].Enabled || len(ch[2].Printers) == 0 {
		t.Errorf("channel whose last printer went = %+v, want off and not widened to all", ch[2])
	}
	if cfg.Timelapse.Enabled || len(cfg.Timelapse.Printers) == 0 {
		t.Errorf("timelapse = %+v, want off and not widened to all", cfg.Timelapse)
	}
	if s := cfg.Security.Shares; len(s) != 1 || s[0].ID != "s2" {
		t.Errorf("shares = %+v, want only s2", s)
	}
	if m := cfg.Maintenance.Tasks; len(m) != 1 || m[0].ID != "t2" {
		t.Errorf("tasks = %+v, want only t2", m)
	}

	forgetPrinterIDs(nil, map[string]bool{"a": true})              // no panic
	forgetPrinterIDs(&config.Config{}, map[string]bool{"a": true}) // no panic
}

// A sign-in check that never ran (the Bridge was busy) is not a wrong password.
func TestUndoLoginTry(t *testing.T) {
	const ip = "192.0.2.77"
	now := time.Now()
	t.Cleanup(func() { noteLogin(ip, true, now) })
	for i := 0; i < 20; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("try %d locked out though every try was taken back", i)
		}
		undoLoginTry(ip)
	}
	// Real wrong passwords still lock.
	locked := false
	for i := 0; i < 6; i++ {
		if beginLogin(ip, now) > 0 {
			locked = true
		}
	}
	if !locked {
		t.Fatal("six wrong passwords did not lock the address")
	}
}

// A try taken back at the edge of the limit leaves no lock behind.
func TestUndoLoginTry_AtTheLimit(t *testing.T) {
	const ip = "192.0.2.78"
	now := time.Now()
	t.Cleanup(func() { noteLogin(ip, true, now) })
	for i := 0; i < 4; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("try %d locked out too early", i)
		}
	}
	for i := 0; i < 3; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("locked out by a try that was taken back (round %d)", i)
		}
		undoLoginTry(ip)
	}
}

func TestSafePrinterID(t *testing.T) {
	for id, want := range map[string]bool{
		"":                                 true,
		"0f3a9c":                           true,
		"fallback-ab_1.2":                  true,
		"a'b":                              false,
		"a b":                              false,
		"a/b":                              false,
		"<x>":                              false,
		string(make([]byte, 65)):           false,
		"ééé":                              false,
		"0123456789abcdef0123456789abcdef": true,
	} {
		if got := safePrinterID(id); got != want {
			t.Errorf("safePrinterID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestPrinterNameFor(t *testing.T) {
	configMutex.Lock()
	old := configStore
	configStore = &config.Config{Printers: []config.Printer{{ID: "id-1", Name: "Voron"}}}
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
	})
	if got := printerNameFor("id-1"); got != "Voron" {
		t.Errorf("by id = %q, want Voron", got)
	}
	if got := printerNameFor("Voron"); got != "Voron" {
		t.Errorf("by name = %q, want Voron", got)
	}
	if got := printerNameFor(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}
