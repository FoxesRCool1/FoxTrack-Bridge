package lan

import (
	"testing"
	"time"

	configpkg "foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

type relayCall struct {
	key, url, serial, name string
	payload                webhook.RelayPayload
}

func captureSends(t *testing.T) chan relayCall {
	t.Helper()
	calls := make(chan relayCall, 8)
	old := sendRelay
	sendRelay = func(key, url, serial, name string, p webhook.RelayPayload) error {
		calls <- relayCall{key, url, serial, name, p}
		return nil
	}
	t.Cleanup(func() { sendRelay = old })
	return calls
}

func nextCall(t *testing.T, calls chan relayCall) relayCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no relay post")
		return relayCall{}
	}
}

// A Klipper printer is posted under its config ID, with its name (and earlier
// names) as the legacy id, current project first. The legacy project keeps the
// name as its key and gets no legacy id.
func TestQueueRelay_KlipperIdentity(t *testing.T) {
	calls := captureSends(t)
	p := configpkg.Printer{ID: "a1b2c3", Name: "Shop Klipper", MoonrakerURL: "http://k.local", PreviousNames: []string{"Old Name", "Shop Klipper"}}
	serial, legacy := p.RelayIdentity()
	if serial != "a1b2c3" || legacy != "Shop Klipper,Old Name" {
		t.Fatalf("RelayIdentity = %q, %q", serial, legacy)
	}

	c := NewController()
	c.queueRelay(p.Name, relayJob{foxAPIKey: "legacy", fox2APIKey: "v2", serial: serial, legacy: legacy})

	v2 := nextCall(t, calls)
	if v2.url != webhook.RelayURLV2 || v2.serial != "a1b2c3" || v2.name != "Shop Klipper" || v2.payload.LegacyID != "Shop Klipper,Old Name" {
		t.Errorf("current project post = %+v", v2)
	}
	old := nextCall(t, calls)
	if old.url != webhook.URL || old.serial != "Shop Klipper" || old.payload.LegacyID != "" {
		t.Errorf("legacy project post = %+v", old)
	}
}

func TestRelayIdentity(t *testing.T) {
	bambu := configpkg.Printer{ID: "x", Name: "P1S", Serial: "01P00A", LANCode: "1234"}
	if s, l := bambu.RelayIdentity(); s != "01P00A" || l != "" {
		t.Errorf("Bambu = %q, %q: keeps its serial, no legacy id", s, l)
	}
	noID := configpkg.Printer{Name: "Voron", MoonrakerURL: "http://v.local"}
	if s, l := noID.RelayIdentity(); s != "Voron" || l != "" {
		t.Errorf("no ID yet = %q, %q: falls back to the name", s, l)
	}
}

// Two failed polls in a row make one OFFLINE report; a third does not repeat it;
// the first good poll afterwards goes out at once even if nothing changed.
func TestOfflineAfterTwoFailedPolls(t *testing.T) {
	c := NewController()
	if c.noteFailedPoll("Voron", 1000) {
		t.Fatal("one failed poll is not an outage")
	}
	if !c.noteFailedPoll("Voron", 1005) {
		t.Fatal("the second failed poll in a row must report OFFLINE")
	}
	if c.noteFailedPoll("Voron", 1010) {
		t.Fatal("OFFLINE is sent once per outage")
	}

	idle := &mqttpkg.TelemetryData{Status: "idle", NozzleTemp: 24, BedTemp: 24}
	if !c.shouldRelay("Voron", idle, idle, 1011) {
		t.Fatal("the first good poll after OFFLINE must be urgent")
	}
	if c.shouldRelay("Voron", idle, idle, 1012) {
		t.Fatal("only the first one is urgent")
	}
	// A new outage is reported again, and a good poll between resets the count.
	if c.noteFailedPoll("Voron", 1020) || !c.noteFailedPoll("Voron", 1025) {
		t.Fatal("a later outage must be reported once more")
	}
	c.shouldRelay("Voron", idle, idle, 1030)
	if c.noteFailedPoll("Voron", 1035) {
		t.Fatal("a good poll in between restarts the count")
	}

	p := mqttpkg.OfflinePayload(nil, configpkg.KindKlipper, "Klipper", nil).Print
	if p.GcodeState != "OFFLINE" || p.Alarms != nil || p.McRemainingTime != -1 {
		t.Errorf("OFFLINE payload = %+v", p)
	}
}

func TestRelayFor_RemainingTimeAndAlarmsOfAFullReport(t *testing.T) {
	printing := relayFor(mqttpkg.TelemetryData{Status: "printing"}, configpkg.KindPrusaLink, nil).Print
	if printing.McRemainingTime != -1 {
		t.Errorf("a print with no estimate sends %d, want -1", printing.McRemainingTime)
	}
	if printing.Alarms == nil {
		t.Error("a full report sends alarms as [] when there are none, never nil")
	}
	idle := relayFor(mqttpkg.TelemetryData{Status: "idle"}, configpkg.KindPrusaLink, nil).Print
	if idle.McRemainingTime != 0 {
		t.Errorf("an idle printer sends %d, want 0", idle.McRemainingTime)
	}
	est := relayFor(mqttpkg.TelemetryData{Status: "printing", TimeRemaining: 30}, configpkg.KindPrusaLink, nil).Print
	if est.McRemainingTime != 30 {
		t.Errorf("remaining = %d, want 30", est.McRemainingTime)
	}
}

// A Bridge restart in the middle of a print keeps its start time when the
// printer is still printing the same file; another file starts clean, and an
// ended print leaves nothing to take back.
func TestSession_SurvivesBridgeRestart(t *testing.T) {
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

	c := newController()
	c.applyPoll("voron", printing("cube.gcode", 10), nil, 1000)
	c.applyPoll("voron", printing("cube.gcode", 20), nil, 1100) // a minute on: saved

	c2 := newController() // the restart
	c2.applyPoll("voron", printing("cube.gcode", 22), nil, 1200)
	if got := c2.sessions["voron"].StartTime; got != 1000 {
		t.Errorf("start after a restart = %d, want the original 1000", got)
	}
	if len(c2.sessions["voron"].TempSamples) < 2 {
		t.Errorf("temperature samples not kept: %+v", c2.sessions["voron"].TempSamples)
	}

	c3 := newController()
	c3.applyPoll("voron", printing("vase.gcode", 22), nil, 1300)
	if got := c3.sessions["voron"].StartTime; got != 1300 {
		t.Errorf("another file must start a new session, start = %d", got)
	}

	end := c3.applyPoll("voron", &mqttpkg.TelemetryData{Status: "finished"}, nil, 1400)
	if end == nil {
		t.Fatal("the print should have ended")
	}
	var left savedSession
	if history.LoadSession(c3.sessionKey("voron"), &left) {
		t.Errorf("an ended print left a saved session: %+v", left)
	}
	c4 := newController()
	c4.applyPoll("voron", printing("vase.gcode", 5), nil, 1500)
	if got := c4.sessions["voron"].StartTime; got != 1500 {
		t.Errorf("a new run of the same file after it ended must start clean, start = %d", got)
	}
}
