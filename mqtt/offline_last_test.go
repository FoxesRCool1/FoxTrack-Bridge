package mqtt

import "testing"

// The OFFLINE report for a Bambu printer carries its last real report's
// progress and file, with temperatures unknown.
func TestReportOffline_KeepsLastPrint(t *testing.T) {
	const name = "offline-keep"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	sent := captureRelays(t)

	p := Printer{Name: name, Serial: "OFFK", FoxTrack2APIKey: "v2"}
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","mc_percent":47,"subtask_name":"benchy","nozzle_temper":210,"bed_temper":60}}`})
	if got := nextRelay(t, sent); got.payload.Print.McPercent != 47 {
		t.Fatalf("report = %+v", got.payload.Print)
	}
	reportOffline(p, 5000)
	pr := nextRelay(t, sent).payload.Print
	if pr.GcodeState != "OFFLINE" || pr.McRemainingTime != -1 || pr.Alarms != nil {
		t.Errorf("OFFLINE report = %+v", pr)
	}
	if pr.McPercent != 47 || pr.SubTaskName != "benchy" {
		t.Errorf("the last print was not kept: %+v", pr)
	}
	if pr.NozzleTemper != 0 || pr.BedTemper != 0 {
		t.Errorf("temperatures are unknown while offline: %+v", pr)
	}
}
