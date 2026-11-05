package main

import (
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

func kinds(es []events.Event) []events.Kind {
	var out []events.Kind
	for _, e := range es {
		out = append(out, e.Kind)
	}
	return out
}

func TestDiffState_Lifecycle(t *testing.T) {
	watchMu.Lock()
	defer watchMu.Unlock()
	watched = map[string]*watchState{}
	p := config.Printer{ID: "p1", Name: "Fox"}
	t0 := time.Unix(1_000_000, 0)
	step := func(status string, at time.Duration, alarms ...webhook.RelayAlarm) []events.Kind {
		return kinds(diffState(p, &mqttpkg.TelemetryData{Status: status, FileName: "cube.3mf", Alarms: alarms}, t0.Add(at)))
	}
	code := "0700_2000_0002_0001"

	if got := step("connected", 0); got != nil {
		t.Fatalf("a connecting printer seeds nothing, got %v", got)
	}
	if got := step("printing", 0); got != nil {
		t.Fatalf("first real report must only seed (restart mid-print), got %v", got)
	}
	if got := step("printing", 3*time.Second); got != nil {
		t.Fatalf("seeded mid-print: no start, got %v", got)
	}
	if got := step("paused", 6*time.Second, webhook.RelayAlarm{Source: "hms", Code: &code, Severity: "serious"}); len(got) != 2 || got[0] != events.PrintPaused || got[1] != events.PrinterError {
		t.Fatalf("pause with alarm: got %v", got)
	}
	if got := step("paused", 9*time.Second, webhook.RelayAlarm{Source: "hms", Code: &code, Severity: "serious"}); got != nil {
		t.Fatalf("same alarm twice: got %v", got)
	}
	if got := step("printing", 12*time.Second); len(got) != 1 || got[0] != events.PrintResumed {
		t.Fatalf("resume: got %v", got)
	}
	if got := step("finished", 15*time.Second); got != nil {
		t.Fatalf("end is sent from history, got %v", got)
	}
	if got := step("printing", 18*time.Second); len(got) != 1 || got[0] != events.PrintStarted {
		t.Fatalf("new print: got %v", got)
	}
	if got := step("disconnected", 20*time.Second); got != nil {
		t.Fatalf("short drop: got %v", got)
	}
	if got := step("disconnected", 20*time.Second+offlineAfter); len(got) != 1 || got[0] != events.PrinterOffline {
		t.Fatalf("offline: got %v", got)
	}
	if got := step("printing", 30*time.Second+offlineAfter); len(got) != 1 || got[0] != events.PrinterOnline {
		t.Fatalf("back online mid-print must not restart the print: got %v", got)
	}
	// Offline again soon after coming back: only after offlineAgainAfter.
	if got := step("disconnected", 40*time.Second+offlineAfter); got != nil {
		t.Fatalf("flap start: got %v", got)
	}
	if got := step("disconnected", 40*time.Second+2*offlineAfter); got != nil {
		t.Fatalf("a quick second drop is not reported: got %v", got)
	}
	if got := step("printing", 50*time.Second+2*offlineAfter); got != nil {
		t.Fatalf("no online without an offline: got %v", got)
	}
	info := "x"
	if got := step("printing", 40*time.Second+offlineAfter, webhook.RelayAlarm{Source: "message", Message: &info, Severity: "info"}); got != nil {
		t.Fatalf("info alarms are not errors: got %v", got)
	}
}

func TestDiffState_AlarmsGroupedAndCleared(t *testing.T) {
	watchMu.Lock()
	defer watchMu.Unlock()
	watched = map[string]*watchState{}
	p := config.Printer{ID: "p1", Name: "Fox"}
	t0 := time.Unix(2_000_000, 0)
	al := func(code, sev string) webhook.RelayAlarm {
		c := code
		m := "text " + code
		return webhook.RelayAlarm{Source: "hms", Code: &c, Message: &m, Severity: sev}
	}
	a, b, c := al("A", "common"), al("B", "fatal"), al("C", "serious")
	step := func(at time.Duration, alarms ...webhook.RelayAlarm) []events.Event {
		return diffState(p, &mqttpkg.TelemetryData{Status: "idle", Alarms: alarms}, t0.Add(at))
	}
	step(0) // seed

	// Three new alarms in one report: one event, worst first.
	es := step(3*time.Second, a, b, c)
	if len(es) != 1 || es[0].Kind != events.PrinterError || len(es[0].Alarms) != 3 ||
		es[0].Alarms[0].Code != "B" || es[0].Alarms[1].Code != "C" || es[0].Code != "B" {
		t.Fatalf("grouped: %+v", es)
	}
	// They flicker off and back within alarmClearAfter: nothing.
	if es := step(6 * time.Second); es != nil {
		t.Fatalf("gone briefly: %+v", es)
	}
	if es := step(9*time.Second, a, b, c); es != nil {
		t.Fatalf("flicker back: %+v", es)
	}
	// A lost link does not count as time away.
	step(12 * time.Second)
	diffState(p, &mqttpkg.TelemetryData{Status: "disconnected"}, t0.Add(time.Minute))
	if es := step(4*time.Minute, a, b, c); es != nil {
		t.Fatalf("after reconnect: %+v", es)
	}
	// Gone for good: one "cleared" after alarmClearAfter, with how long they lasted.
	step(10 * time.Minute)
	if es := step(10*time.Minute + alarmClearAfter - time.Second); es != nil {
		t.Fatalf("too soon to clear: %+v", es)
	}
	es = step(10*time.Minute + alarmClearAfter)
	if len(es) != 1 || es[0].Kind != events.PrinterAlarmCleared || len(es[0].Alarms) != 3 || es[0].Alarms[0].Lasted != 10*time.Minute-3*time.Second {
		t.Fatalf("cleared: %+v", es)
	}
	if es := step(13 * time.Minute); es != nil {
		t.Fatalf("cleared twice: %+v", es)
	}
	// It comes back: news again. It goes again: no second "cleared" within
	// alarmQuietAgain, one once that time is up.
	if es := step(14*time.Minute, b); len(es) != 1 || es[0].Kind != events.PrinterError {
		t.Fatalf("back after cleared: %+v", es)
	}
	step(15 * time.Minute)
	if es := step(20 * time.Minute); es != nil {
		t.Fatalf("second cleared inside the quiet time: %+v", es)
	}
	if es := step(12*time.Minute + alarmQuietAgain); len(es) != 1 || es[0].Kind != events.PrinterAlarmCleared {
		t.Fatalf("cleared after the quiet time: %+v", es)
	}
}
