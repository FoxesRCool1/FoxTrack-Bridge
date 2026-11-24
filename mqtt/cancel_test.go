package mqtt

import (
	"testing"
	"time"

	"foxtrack-bridge/history"
	"foxtrack-bridge/webhook"
)

// waitResult waits for the history record of printer name and returns its result.
func waitResult(t *testing.T, name string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := history.ForPrinter(name); len(recs) > 0 {
			return recs[len(recs)-1].Result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no history record written")
	return ""
}

// Bambu reports a print the user stopped as gcode_state FAILED with
// print_error 0300_400C. That is a cancelled print: not an error status, not
// a serious alarm, and "cancelled" in the history.
func TestHandler_UserCancelIsCancelledNotError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "cancel-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })

	sent := make(chan webhook.RelayPayload, 16)
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		sent <- p
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })
	last := func() webhook.RelayPayload {
		t.Helper()
		var p webhook.RelayPayload
		select {
		case p = <-sent:
		case <-time.After(2 * time.Second):
			t.Fatal("no relay payload sent")
		}
		for {
			select {
			case p = <-sent:
			case <-time.After(50 * time.Millisecond):
				return p
			}
		}
	}

	handle := makeHandler(Printer{Name: name, Serial: "CANCEL", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	last()

	// 50348044 = 0x0300400C.
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":50348044,"mc_print_error_code":"50348044"}}`})
	st := GetPrinterState(name)
	if st.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", st.Status)
	}
	if st.Error != "" {
		t.Errorf("error = %q, want none for a user cancel", st.Error)
	}
	if len(st.Alarms) != 1 || st.Alarms[0].Severity != webhook.AlarmInfo {
		t.Errorf("alarms = %+v, want one info alarm", st.Alarms)
	}
	if got := last().Print.GcodeState; got != "CANCELLED" {
		t.Errorf("relayed gcode_state = %q, want CANCELLED", got)
	}
	if got := waitResult(t, name); got != "cancelled" {
		t.Errorf("history result = %q, want cancelled", got)
	}

	// The printer keeps saying FAILED after the code is cleared: same print.
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":0,"mc_print_error_code":"0"}}`})
	if got := GetPrinterState(name).Status; got != "cancelled" {
		t.Errorf("status after the code cleared = %q, want cancelled", got)
	}
}

// A real failure stays an error with a serious alarm.
func TestHandler_RealFailureStaysError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "fail-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })

	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error { return nil }
	t.Cleanup(func() { sendRelay = prevSend })

	handle := makeHandler(Printer{Name: name, Serial: "FAIL", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	// 50348041 = 0x03004009, homing XY failed.
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":50348041,"mc_print_error_code":"50348041"}}`})
	st := GetPrinterState(name)
	if st.Status != "error" {
		t.Fatalf("status = %q, want error", st.Status)
	}
	if len(st.Alarms) != 1 || st.Alarms[0].Severity != webhook.AlarmSerious {
		t.Errorf("alarms = %+v, want one serious alarm", st.Alarms)
	}
	if got := waitResult(t, name); got != "error" {
		t.Errorf("history result = %q, want error", got)
	}
	if got := relayGcodeState("cancelled"); got != "CANCELLED" {
		t.Errorf("relayGcodeState(cancelled) = %q", got)
	}
}

// quietRelay swaps the relay sender for one that does nothing.
func quietRelay(t *testing.T) {
	t.Helper()
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error { return nil }
	t.Cleanup(func() { sendRelay = prevSend })
}

// The cancel code can come while the printer still says RUNNING, with a bare
// FAILED after it. The print is cancelled all the same.
func TestHandler_CancelCodeBeforeFailed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "cancel-order-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	quietRelay(t)

	handle := makeHandler(Printer{Name: name, Serial: "CANCELORDER", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"print_error":50348044}}`})
	if got := GetPrinterState(name).Status; got != "printing" {
		t.Fatalf("status with the code and RUNNING = %q, want printing", got)
	}
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	if got := GetPrinterState(name).Status; got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	if got := waitResult(t, name); got != "cancelled" {
		t.Errorf("history result = %q, want cancelled", got)
	}
}

// The cancel outlives a reconnect (the state starts over at "connected" and
// the printer still says FAILED, now with code 0), and ends at a real error
// or the next print.
func TestHandler_CancelSurvivesReconnect(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "cancel-reconnect-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	quietRelay(t)

	handle := makeHandler(Printer{Name: name, Serial: "CANCELRECON", APIKey: "key"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube.gcode","mc_percent":40}}`})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":50348044,"mc_print_error_code":"50348044"}}`})
	waitResult(t, name)

	markConnected(name)
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":0,"mc_print_error_code":"0"}}`})
	if got := GetPrinterState(name).Status; got != "cancelled" {
		t.Fatalf("status after a reconnect = %q, want cancelled", got)
	}

	// A real error while the printer still says FAILED is an error.
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":50348041,"mc_print_error_code":"50348041"}}`})
	if got := GetPrinterState(name).Status; got != "error" {
		t.Fatalf("status with a real error = %q, want error", got)
	}
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","print_error":0,"mc_print_error_code":"0"}}`})
	if got := GetPrinterState(name).Status; got != "error" {
		t.Errorf("status after the real error cleared = %q, want error", got)
	}

	// The next print that fails is a failure, not the old cancel.
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"vase.gcode","mc_percent":5,"print_error":0,"mc_print_error_code":"0"}}`})
	// A bare FAILED waits a moment for a cancel code (see failHoldWait).
	shortFailHold(t, 20*time.Millisecond)
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED"}}`})
	if got := waitStatus(t, name, "error"); got != "error" {
		t.Errorf("status of the next failed print = %q, want error", got)
	}
	waitHandler(name)
	waitRecords(t, name, 2)
}

// waitRecords waits until n history records of the printer are on disk, so
// no background write races the removal of the temp dir.
func waitRecords(t *testing.T, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := history.ForPrinter(name); len(recs) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d history records written", n)
}

// The left external spool of a dual-nozzle printer goes out with the report
// and counts in the print's filaments.
func TestSpoolsAt_IncludesBothExternalSpools(t *testing.T) {
	right := &AmsSlot{Slot: ExternalSlot, Material: "PLA"}
	left := &AmsSlot{Slot: ExternalSlot2, Material: "PETG"}
	got := spoolsAt([]AmsSlot{{Slot: 0}}, right, nil, left)
	if len(got) != 3 || got[1].Slot != ExternalSlot || got[2].Slot != ExternalSlot2 {
		t.Fatalf("spoolsAt = %+v", got)
	}
	prev := &TelemetryData{ExternalSpool2: &AmsSlot{Slot: ExternalSlot2, Material: "PLA"}}
	curr := &TelemetryData{ExternalSpool2: &AmsSlot{Slot: ExternalSlot2, Material: "PETG"}}
	if !filamentChanged(prev, curr) {
		t.Error("a material change on the left external spool was not seen")
	}
	if !filamentChanged(&TelemetryData{}, curr) {
		t.Error("a new left external spool was not seen")
	}
}
