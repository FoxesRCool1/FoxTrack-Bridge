package mqtt

import (
	"reflect"
	"testing"
	"time"

	"foxtrack-bridge/webhook"
)

// An explicit 0 is a real value: the targets after a cooldown and progress at
// the start of a print. Only an absent field keeps the previous one.
func TestHandler_ExplicitZeroIsApplied(t *testing.T) {
	const name = "zero-target-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})

	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","mc_percent":50,"nozzle_target_temper":220,"bed_target_temper":60}}`})
	handle(nil, fakeMessage{`{"print":{"nozzle_temper":200}}`}) // absent: targets stay
	if st := GetPrinterState(name); st.NozzleTarget != 220 || st.BedTarget != 60 || st.Progress != 50 {
		t.Fatalf("absent fields changed: %+v", st)
	}
	handle(nil, fakeMessage{`{"print":{"nozzle_target_temper":0,"bed_target_temper":0}}`}) // only zeros
	if st := GetPrinterState(name); st.NozzleTarget != 0 || st.BedTarget != 0 {
		t.Fatalf("explicit 0 targets not applied: %+v", st)
	}
	handle(nil, fakeMessage{`{"print":{"mc_percent":0}}`})
	if st := GetPrinterState(name); st.Progress != 0 {
		t.Fatalf("explicit 0 progress not applied: %+v", st)
	}
}

// Deltas omit mc_print_error_code. The last one stays, and "0" means no error.
func TestHandler_ErrorCodeSurvivesDeltas(t *testing.T) {
	const name = "error-code-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})

	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","mc_print_error_code":"0"}}`})
	if st := GetPrinterState(name); st.Error != "" {
		t.Fatalf("error %q, want none for \"0\"", st.Error)
	}
	handle(nil, fakeMessage{`{"print":{"nozzle_temper":30}}`})
	if st := GetPrinterState(name); st.Error != "" {
		t.Fatalf("a delta set error %q", st.Error)
	}
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FAILED","mc_print_error_code":"50348044"}}`})
	handle(nil, fakeMessage{`{"print":{"nozzle_temper":31}}`})
	if st := GetPrinterState(name); st.Error != "50348044" {
		t.Fatalf("a delta wiped the error: %q", st.Error)
	}
	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","mc_print_error_code":"0"}}`})
	if st := GetPrinterState(name); st.Error != "" {
		t.Fatalf("error %q not cleared by \"0\"", st.Error)
	}
}

// A print that ends while the link is down is recorded by the first real
// report after the reconnect, with the last progress seen.
func TestHandler_PrintEndedWhileLinkDown(t *testing.T) {
	const name = "ended-offline-test"
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	snapLastTMu.Lock()
	snapLastT[name] = time.Now().Unix() + 3600 // no camera snapshot
	snapLastTMu.Unlock()

	got := make(chan webhook.HistoryPayload, 4)
	prevHistory, prevRelay := sendHistory, sendRelay
	sendHistory = func(_, _ string, p webhook.HistoryPayload) { got <- p }
	sendRelay = func(_, _, _, _ string, _ webhook.RelayPayload) error { return nil }
	t.Cleanup(func() { sendHistory, sendRelay = prevHistory, prevRelay })

	handle := makeHandler(Printer{Name: name, Serial: "TEST", FoxTrack2APIKey: "key"})
	run := func(end string, wantResult string, wantPercent int) {
		t.Helper()
		handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"cube","mc_percent":70}}`})
		UpdatePrinterState(name, TelemetryData{Status: "disconnected"})
		UpdatePrinterState(name, TelemetryData{Status: "connected"})
		handle(nil, fakeMessage{`{"print":{"nozzle_temper":30}}`}) // delta before the pushall reply: nothing ends yet
		sessionMu.Lock()
		open := activeSessions[name] != nil
		sessionMu.Unlock()
		if !open {
			t.Fatal("session closed before the printer said how the print ended")
		}
		handle(nil, fakeMessage{`{"print":{"gcode_state":"` + end + `"}}`})
		select {
		case h := <-got:
			if h.Result != wantResult || h.PercentAtEnd != wantPercent || h.FileName != "cube" {
				t.Fatalf("%s: history = %+v", end, h)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: print never recorded", end)
		}
		sessionMu.Lock()
		left := activeSessions[name]
		sessionMu.Unlock()
		if left != nil {
			t.Fatalf("%s: session left behind", end)
		}
	}
	run("FINISH", "finished", 100)
	run("IDLE", "cancelled", 70)
}

// A delta that lists some trays merges by unit id + tray id: the other slots,
// the omitted fields and the active tray all stay.
func TestHandler_PartialAMSMerges(t *testing.T) {
	const name = "ams-merge-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})

	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","ams":{"tray_now":"1","ams":[
		{"id":"0","tray":[{"id":"0","tray_type":"PLA","tray_color":"FF0000FF","remain":90},{"id":"1","tray_type":"PETG","tray_color":"00FF00FF","remain":80}]},
		{"id":"1","tray":[{"id":"0","tray_type":"ABS","tray_color":"0000FFFF","remain":70}]}]}}}`})
	refs := getTrayRefs(name)

	handle(nil, fakeMessage{`{"print":{"ams":{"ams":[{"id":"1","tray":[{"id":"0","remain":42}]}]}}}`})
	want := []AmsSlot{
		{Slot: 0, Color: "FF0000", Material: "PLA", Remaining: 90},
		{Slot: 1, Color: "00FF00", Material: "PETG", Remaining: 80, Active: true},
		{Slot: 4, Color: "0000FF", Material: "ABS", Remaining: 42},
	}
	if got := GetPrinterState(name).AMS; !reflect.DeepEqual(got, want) {
		t.Fatalf("ams\n got %+v\nwant %+v", got, want)
	}
	if got := getTrayRefs(name); !reflect.DeepEqual(got, refs) {
		t.Fatalf("tray refs = %+v, want %+v", got, refs)
	}
}

// Right after a connect the state is blank: a delta ahead of the pushall reply
// must not post an empty snapshot to FoxTrack.
func TestHandler_NoRelayBeforeFullReport(t *testing.T) {
	const name = "relay-blank-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	sent := make(chan webhook.RelayPayload, 4)
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		sent <- p
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })

	handle := makeHandler(Printer{Name: name, Serial: "TEST", APIKey: "key"})
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	handle(nil, fakeMessage{`{"print":{"nozzle_temper":30}}`})
	select {
	case p := <-sent:
		t.Fatalf("blank snapshot posted: %+v", p.Print)
	case <-time.After(300 * time.Millisecond):
	}
	if st := GetPrinterState(name); st.NozzleTemp != 30 {
		t.Fatalf("dashboard state not updated: %+v", st)
	}
	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","nozzle_temper":31}}`})
	select {
	case p := <-sent:
		if p.Print.GcodeState != "IDLE" {
			t.Fatalf("gcode_state = %q, want IDLE", p.Print.GcodeState)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full report not posted")
	}
}
