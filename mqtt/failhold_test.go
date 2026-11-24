package mqtt

import (
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/history"
	"foxtrack-bridge/webhook"
)

// shortFailHold sets how long a bare FAILED waits for its error code.
func shortFailHold(t *testing.T, d time.Duration) {
	t.Helper()
	prev := failHoldWait
	failHoldWait = d
	t.Cleanup(func() { failHoldWait = prev })
}

// waitStatus waits up to 2 s for the printer to reach want and returns the
// status it has then.
func waitStatus(t *testing.T, name, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if GetPrinterState(name).Status == want {
			return want
		}
		time.Sleep(5 * time.Millisecond)
	}
	return GetPrinterState(name).Status
}

// waitHandler waits for a hold's timer to finish its run of the handler, so
// no relay is queued after the test restores sendRelay.
func waitHandler(name string) {
	lock := handlerLock(name)
	lock.Lock()
	lock.Unlock() //nolint:staticcheck // only waiting for the holder
}

// relayStates captures the gcode_state of every relay post.
func relayStates(t *testing.T) (states func() []string) {
	t.Helper()
	sent := make(chan string, 64)
	prev := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		sent <- p.Print.GcodeState
		return nil
	}
	t.Cleanup(func() { sendRelay = prev })
	var all []string
	return func() []string {
		for {
			select {
			case s := <-sent:
				all = append(all, s)
			case <-time.After(100 * time.Millisecond):
				return all
			}
		}
	}
}

// FAILED can come one message before the cancel code. The print is cancelled
// all the same: in the status, in the history, and FoxTrack never gets a
// FAILED post (which would fire its failed-print alert).
func TestHandler_BareFailedThenCancelCode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "failed-first-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	shortFailHold(t, time.Minute) // the code decides here, not the timeout
	states := relayStates(t)

	handle := makeHandler(Printer{Name: name, Serial: "FAILEDFIRST", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	if got := GetPrinterState(name).Status; got != "printing" {
		t.Fatalf("status while the FAILED is held = %q, want printing", got)
	}
	// Other reports in between do not end the hold.
	handle(nil, fakeMessage{`{"print":{"nozzle_temper":180}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	if recs, _ := history.ForPrinter(name); len(recs) != 0 {
		t.Fatalf("history written while the FAILED is held: %+v", recs)
	}

	handle(nil, fakeMessage{`{"print":{"print_error":50348044}}`}) // 0x0300400C
	if got := GetPrinterState(name).Status; got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	if got := waitResult(t, name); got != "cancelled" {
		t.Errorf("history result = %q, want cancelled", got)
	}
	got := states()
	for _, s := range got {
		if s == relayGcodeState("error") {
			t.Errorf("a FAILED relay went out: %v", got)
		}
	}
	if len(got) == 0 || got[len(got)-1] != "CANCELLED" {
		t.Errorf("relayed states = %v, want CANCELLED last", got)
	}
	if failHeld(name) {
		t.Error("the hold is still running after the code came")
	}
	if recs, _ := history.ForPrinter(name); len(recs) != 1 {
		t.Errorf("history records = %d, want 1", len(recs))
	}
}

// No code within the wait: the print failed.
func TestHandler_BareFailedTimesOutToError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "failed-timeout-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	shortFailHold(t, 30*time.Millisecond)
	states := relayStates(t)

	handle := makeHandler(Printer{Name: name, Serial: "FAILEDTIMEOUT", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	if got := waitStatus(t, name, "error"); got != "error" {
		t.Fatalf("status after the wait = %q, want error", got)
	}
	if got := waitResult(t, name); got != "error" {
		t.Errorf("history result = %q, want error", got)
	}
	waitHandler(name)
	got := states()
	if len(got) == 0 || got[len(got)-1] != relayGcodeState("error") {
		t.Errorf("relayed states = %v, want %s last", got, relayGcodeState("error"))
	}
	if recs, _ := history.ForPrinter(name); len(recs) != 1 {
		t.Errorf("history records = %d, want 1", len(recs))
	}
}

// A real error code ends the hold at once, as an error. A FAILED that carries
// a real code is never held.
func TestHandler_BareFailedThenRealCode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "failed-real-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	shortFailHold(t, time.Minute)
	quietRelay(t)

	handle := makeHandler(Printer{Name: name, Serial: "FAILEDREAL", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	handle(nil, fakeMessage{`{"print":{"print_error":50348041}}`}) // 0x03004009
	if got := GetPrinterState(name).Status; got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if got := waitResult(t, name); got != "error" {
		t.Errorf("history result = %q, want error", got)
	}

	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"vase.gcode","mc_percent":5,"print_error":0}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":50348041}}`})
	if got := GetPrinterState(name).Status; got != "error" {
		t.Fatalf("status of a FAILED with a real code = %q, want error at once", got)
	}
	if failHeld(name) {
		t.Error("a FAILED with a real code was held")
	}
	waitRecords(t, name, 2)
}

// The printer moves on while a FAILED is held: the print ends as an error
// first, then the new state applies.
func TestHandler_HeldFailedThenNextState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "failed-next-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	shortFailHold(t, time.Minute)
	quietRelay(t)

	handle := makeHandler(Printer{Name: name, Serial: "FAILEDNEXT", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE"}}`})
	if got := GetPrinterState(name).Status; got != "idle" {
		t.Fatalf("status = %q, want idle", got)
	}
	if got := waitResult(t, name); got != "error" {
		t.Errorf("history result = %q, want error", got)
	}
	if failHeld(name) {
		t.Error("the hold is still running")
	}
}

// A printer removed with a relay waiting its turn gets no post after the
// removal, and nothing brings its state back until it is connected again.
func TestRemovePrinter_NoLatePostOrState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "removed-test"
	resetPrinterState(name)
	t.Cleanup(func() { revivePrinter(name); resetPrinterState(name) })

	var posts atomic.Int32
	started, release := make(chan struct{}, 8), make(chan struct{})
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, _ webhook.RelayPayload) error {
		posts.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })

	p := Printer{Name: name, Serial: "REMOVED", APIKey: "key"}
	build := func() webhook.RelayPayload { return offlineRelayPayload(p) }
	queueRelay(p, true, build) // in flight: blocks in the send
	<-started
	queueRelay(p, true, build) // waits its turn behind the first
	relayOrderMu.Lock()
	second := relayTurn[name]
	relayOrderMu.Unlock()

	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	RemovePrinterState(name)
	close(release)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued relay never finished")
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("posts = %d, want only the one already in flight", n)
	}

	// Late writers: a status write, an offline report, a report message.
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	markDisconnected(name)
	reportOffline(p, time.Now().Unix())
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	time.Sleep(100 * time.Millisecond)
	if n := posts.Load(); n != 1 {
		t.Errorf("posts after the removal = %d, want 1", n)
	}
	stateMutex.RLock()
	_, has := printerStates[name]
	stateMutex.RUnlock()
	if has {
		t.Error("the removed printer has a state again")
	}
	sessionMu.Lock()
	_, hasSession := activeSessions[name]
	sessionMu.Unlock()
	if hasSession {
		t.Error("the removed printer has a print session")
	}

	// Connected again (ConnectPrinter and the cloud subscribe do this).
	revivePrinter(name)
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	if got := GetPrinterState(name).Status; got != "idle" {
		t.Errorf("status after the printer came back = %q, want idle", got)
	}
}

// A rename removes the old name only: the new name keeps its state and posts.
func TestRemovePrinter_RenameKeepsNewName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const oldName, newName = "removed-rename-old", "removed-rename-new"
	for _, n := range []string{oldName, newName} {
		resetPrinterState(n)
		t.Cleanup(func() { revivePrinter(n); resetPrinterState(n) })
	}
	sent := captureRelays(t)

	UpdatePrinterState(oldName, TelemetryData{Status: "idle"})
	RenameSession(oldName, newName)
	RemovePrinterState(oldName)

	UpdatePrinterState(newName, TelemetryData{Status: "idle"})
	if got := GetPrinterState(newName).Status; got != "idle" {
		t.Fatalf("status under the new name = %q, want idle", got)
	}
	p := Printer{Name: newName, Serial: "RENAMED", APIKey: "key"}
	queueRelay(p, true, func() webhook.RelayPayload { return offlineRelayPayload(p) })
	nextRelay(t, sent)
	if !printerRemoved(oldName) || printerRemoved(newName) {
		t.Errorf("removed: old=%v new=%v, want true false", printerRemoved(oldName), printerRemoved(newName))
	}
}
