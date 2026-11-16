package mqtt

import (
	"testing"
	"time"

	"foxtrack-bridge/history"
	"foxtrack-bridge/webhook"
)

// A rename mid-print moves the session, the saved session and the last
// report's baseline to the new name; a plain removal still forgets them.
func TestRenameSession_KeepsSavedSessionAndLastReport(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := Printer{Name: "ren-old", Serial: "REN"}
	RemovePrinterState(p.Name)
	t.Cleanup(func() {
		RemovePrinterState("ren-new")
		RemovePrinterState("ren-old")
	})

	serialMutex.Lock()
	printerSerials["ren-old"] = "REN"
	serialMutex.Unlock()

	// Start a print: this saves the session under the serial.
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":10}}`})

	webhookLastSentMu.Lock()
	lastRelaySent["ren-old"] = webhook.RelayPayload{Print: webhook.RelayPrint{McPercent: 47}}
	webhookLastSentMu.Unlock()

	RenameSession("ren-old", "ren-new")
	RemovePrinterState("ren-old")

	if !history.LoadSession(sessionKey(p), &savedSession{}) {
		t.Error("the saved session was lost by the rename")
	}
	sessionMu.Lock()
	s := activeSessions["ren-new"]
	sessionMu.Unlock()
	if s == nil {
		t.Error("the in-memory session was not moved to the new name")
	}
	webhookLastSentMu.Lock()
	lp, ok := lastRelaySent["ren-new"]
	webhookLastSentMu.Unlock()
	if !ok || lp.Print.McPercent != 47 {
		t.Errorf("the last report's baseline was not moved: %+v", lp)
	}

	// A plain removal (no rename) still forgets the saved session.
	serialMutex.Lock()
	printerSerials["ren-new"] = "REN"
	serialMutex.Unlock()
	RemovePrinterState("ren-new")
	if history.LoadSession(sessionKey(p), &savedSession{}) {
		t.Error("a plain removal must forget the saved session")
	}
}

// A saved session of a print that ended while the Bridge was down is dropped
// on the first real report; a paused print of the same file keeps it.
func TestStaleSavedSession_DroppedOnFirstIdleReport(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "stale-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	p := Printer{Name: name, Serial: "STL"}
	if err := history.SaveSession(sessionKey(p), savedSession{
		FileName: "cube.gcode", StartTime: 1, LastSeen: time.Now().Unix(), LastPercent: 10,
	}); err != nil {
		t.Fatal(err)
	}

	// The first real report of the run is idle: the print ended while the
	// Bridge was down, so the saved copy must go.
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","nozzle_temper":25}}`})
	if history.LoadSession(sessionKey(p), &savedSession{}) {
		t.Error("the stale saved session must be dropped on the first idle report")
	}

	// A paused print of the same file is the print carrying on: keep it.
	RemovePrinterState(name)
	if err := history.SaveSession(sessionKey(p), savedSession{
		FileName: "cube.gcode", StartTime: 1, LastSeen: time.Now().Unix(), LastPercent: 10,
	}); err != nil {
		t.Fatal(err)
	}
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"PAUSE","subtask_name":"cube.gcode","mc_percent":12}}`})
	if !history.LoadSession(sessionKey(p), &savedSession{}) {
		t.Error("a paused print of the same file must keep the saved session")
	}
}

// After a restart the OFFLINE report has no last report in memory: it falls
// back to the saved print session for the file and progress.
func TestReportOffline_BaselineFromSavedSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "base-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	sent := captureRelays(t)

	p := Printer{Name: name, Serial: "BAS", FoxTrack2APIKey: "v2"}
	if err := history.SaveSession(sessionKey(p), savedSession{
		FileName: "cube.gcode", LastSeen: time.Now().Unix(), LastPercent: 33,
	}); err != nil {
		t.Fatal(err)
	}

	reportOffline(p, 5000)
	pr := nextRelay(t, sent).payload.Print
	if pr.GcodeState != "OFFLINE" {
		t.Fatalf("OFFLINE report = %+v", pr)
	}
	if pr.SubTaskName != "cube.gcode" || pr.McPercent != 33 {
		t.Errorf("the saved session was not used as the baseline: %+v", pr)
	}
}

// A cloud printer that stays unreachable is reported again until the session
// comes back; reportOffline drops the repeats inside the heartbeat window.
func TestCloudOffline_RepeatsUntilSessionReturns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sent := captureRelays(t)
	prev := offlineRepeat
	offlineRepeat = 30 * time.Millisecond
	t.Cleanup(func() { offlineRepeat = prev })

	const name = "cloud-off"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	p := Printer{Name: name, Serial: "COFF", FoxTrack2APIKey: "v2"}
	r := newCloudRunner(CloudSession{}, []Printer{p}, nil)
	UpdatePrinterState(name, TelemetryData{Status: "disconnected", PrinterID: name})

	r.mu.Lock()
	r.armOfflineLocked(p, 0)
	r.mu.Unlock()
	if got := nextRelay(t, sent); got.payload.Print.GcodeState != "OFFLINE" {
		t.Fatalf("first OFFLINE report = %+v", got.payload.Print)
	}

	// reportOffline drops a report inside pace.HeartbeatSec: clear the last
	// send so the re-armed timer's report goes out.
	webhookLastSentMu.Lock()
	webhookLastSent[name] = 0
	webhookLastSentMu.Unlock()
	if got := nextRelay(t, sent); got.payload.Print.GcodeState != "OFFLINE" {
		t.Fatalf("a still-disconnected cloud printer must be reported again, got %+v", got.payload.Print)
	}

	// The session is back: nothing more is reported.
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	webhookLastSentMu.Lock()
	webhookLastSent[name] = 0
	webhookLastSentMu.Unlock()
	select {
	case extra := <-sent:
		t.Fatalf("the printer is connected again but another OFFLINE arrived: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// A second outage signal for the same printer (a failed subscribe, a dropped
// session) replaces the running chain instead of adding one: the old chain
// stops at its next tick without reporting.
func TestCloudOffline_OneChainPerPrinter(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sent := captureRelays(t)

	const name = "cloud-one-chain"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	p := Printer{Name: name, Serial: "CONE", FoxTrack2APIKey: "v2"}
	r := newCloudRunner(CloudSession{}, []Printer{p}, nil)
	UpdatePrinterState(name, TelemetryData{Status: "disconnected", PrinterID: name})

	r.mu.Lock()
	r.armOfflineLocked(p, time.Hour)
	r.armOfflineLocked(p, time.Hour)
	gen := r.offlineGen[p.Serial]
	r.mu.Unlock()
	if gen != 2 {
		t.Fatalf("offlineGen = %d, want 2", gen)
	}

	// The superseded chain's tick reports nothing.
	r.reportOfflineIfStill(p, 1)
	select {
	case extra := <-sent:
		t.Fatalf("a superseded chain reported: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}
