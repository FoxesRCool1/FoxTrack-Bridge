package lan

import (
	"testing"

	configpkg "foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// Renaming a printer mid-print must keep the saved session (the key is the
// stable config ID) and move the last-report baseline with the print.
func TestRenamePrinter_KeepsSavedSessionAndLastReport(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	printer := configpkg.Printer{ID: "id-voron", Name: "voron", MoonrakerURL: "http://v.local"}
	c := NewController()
	c.printers["voron"] = printer
	printing := func(file string, pct int) *mqttpkg.TelemetryData {
		return &mqttpkg.TelemetryData{Status: "printing", FileName: file, Progress: pct, FilamentUsedMM: float64(pct)}
	}
	c.applyPoll("voron", printing("cube.gcode", 10), nil, 1000)
	c.applyPoll("voron", printing("cube.gcode", 20), nil, 1100) // a minute on: saved

	c.relayLastTMu.Lock()
	c.lastRelay["voron"] = webhook.RelayPayload{Print: webhook.RelayPrint{SubTaskName: "cube.gcode", McPercent: 20}}
	c.relayLastTMu.Unlock()

	c.RenamePrinter("voron", "shop")
	c.RemovePrinter("voron")

	var sv savedSession
	if !history.LoadSession("id:id-voron", &sv) {
		t.Fatal("the saved session must survive a rename, it was deleted")
	}
	if c.sessions["shop"] == nil {
		t.Error("the in-memory session must follow the rename")
	}
	if got := c.lastRelay["shop"].Print.McPercent; got != 20 {
		t.Errorf("last-report baseline after rename = %d, want 20", got)
	}
}

// The first good poll of a run drops a saved session whose print ended while
// the Bridge was down (idle), but keeps one the printer is still in (paused).
func TestStaleSavedSessionDroppedOnFirstIdlePoll(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	printer := configpkg.Printer{ID: "id-voron", Name: "voron", MoonrakerURL: "http://v.local"}
	newController := func() *Controller {
		c := NewController()
		c.printers["voron"] = printer
		return c
	}
	printing := func(file string, pct int) *mqttpkg.TelemetryData {
		return &mqttpkg.TelemetryData{Status: "printing", FileName: file, Progress: pct, FilamentUsedMM: float64(pct)}
	}
	save := func() {
		c := newController()
		c.applyPoll("voron", printing("cube.gcode", 10), nil, 1000)
		c.applyPoll("voron", printing("cube.gcode", 20), nil, 1100) // a minute on: saved
	}

	save()
	c := newController() // the restart
	c.applyPoll("voron", &mqttpkg.TelemetryData{Status: "idle"}, nil, 1200)
	if history.LoadSession("id:id-voron", &savedSession{}) {
		t.Error("a print that ended while the Bridge was down left its saved session")
	}

	save()
	c2 := newController()
	c2.applyPoll("voron", &mqttpkg.TelemetryData{Status: "paused", FileName: "cube.gcode", Progress: 20}, nil, 1200)
	if !history.LoadSession("id:id-voron", &savedSession{}) {
		t.Error("a paused print still in progress must keep its saved session")
	}
}

// After a Bridge restart the OFFLINE baseline falls back to the saved print
// session's file and progress.
func TestOfflineBaseline_FromSavedSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	printer := configpkg.Printer{ID: "id-voron", Name: "voron", MoonrakerURL: "http://v.local"}
	c := NewController()
	c.printers["voron"] = printer
	printing := func(file string, pct int) *mqttpkg.TelemetryData {
		return &mqttpkg.TelemetryData{Status: "printing", FileName: file, Progress: pct, FilamentUsedMM: float64(pct)}
	}
	c.applyPoll("voron", printing("cube.gcode", 10), nil, 1000)
	c.applyPoll("voron", printing("cube.gcode", 20), nil, 1100) // a minute on: saved

	c2 := NewController() // the restart
	c2.printers["voron"] = printer
	lp := c2.lastReport("voron")
	if lp == nil || lp.Print.SubTaskName != "cube.gcode" || lp.Print.McPercent != 20 {
		t.Fatalf("baseline after a restart = %+v, want the saved session", lp)
	}

	c2.relayLastTMu.Lock()
	c2.lastRelay["voron"] = webhook.RelayPayload{Print: webhook.RelayPrint{SubTaskName: "cube.gcode", McPercent: 77}}
	c2.relayLastTMu.Unlock()
	if lp := c2.lastReport("voron"); lp == nil || lp.Print.McPercent != 77 {
		t.Fatalf("the last real report wins over the saved session: %+v", lp)
	}

	c3 := NewController()
	if lp := c3.lastReport("nobody"); lp != nil {
		t.Fatalf("no report, no saved session: %+v, want nil", lp)
	}
}
