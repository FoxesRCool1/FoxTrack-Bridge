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
	alarmSentAt = map[string]time.Time{}
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
	info := "x"
	if got := step("printing", 40*time.Second+offlineAfter, webhook.RelayAlarm{Source: "message", Message: &info, Severity: "info"}); got != nil {
		t.Fatalf("info alarms are not errors: got %v", got)
	}
}
