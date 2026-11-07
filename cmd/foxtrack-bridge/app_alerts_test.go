//go:build headless

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// resetAppAlerts clears the package state so each test starts fresh.
func resetAppAlerts() {
	appAlerts.mu.Lock()
	appAlerts.m = nil
	appAlerts.at = time.Time{}
	appAlerts.mu.Unlock()
}

func TestNotifyHeader_EnabledOnly(t *testing.T) {
	n := &config.Notifications{Channels: []config.NotifyChannel{
		{Kind: "telegram", Enabled: true, Events: []string{"print_finished"}},
		{Kind: "discord", Enabled: false, Events: []string{"print_finished"}},
	}}
	got := notifyHeader(n)
	if strings.Contains(got, "discord") {
		t.Fatalf("disabled channel leaked into header: %s", got)
	}
	if !strings.Contains(got, `"telegram":["print_finished"]`) {
		t.Fatalf("header = %s, want telegram with print_finished", got)
	}
}

func TestNotifyHeader_EmptyEventsUsesDefault(t *testing.T) {
	n := &config.Notifications{Channels: []config.NotifyChannel{
		{Kind: "telegram", Enabled: true},
	}}
	got := notifyHeader(n)
	for _, want := range []string{"print_finished", "print_error", "print_paused", "printer_error"} {
		if !strings.Contains(got, want) {
			t.Errorf("header = %s, want it to contain %s", got, want)
		}
	}
	for _, absent := range []string{"maintenance_due", "print_failure_suspected"} {
		if strings.Contains(got, absent) {
			t.Errorf("header = %s, must not contain %s", got, absent)
		}
	}
}

func TestNotifyHeader_SameKindMergesSortedDeduped(t *testing.T) {
	n := &config.Notifications{Channels: []config.NotifyChannel{
		{Kind: "telegram", Enabled: true, Events: []string{"print_finished", "print_failed"}},
		{Kind: "telegram", Enabled: true, Events: []string{"print_failed", "printer_offline"}},
	}}
	got := notifyHeader(n)
	want := `{"telegram":["print_error","print_finished","printer_silent"]}`
	if got != want {
		t.Fatalf("header = %s, want %s", got, want)
	}
}

func TestNotifyHeader_UnmappedKindLeftOut(t *testing.T) {
	n := &config.Notifications{Channels: []config.NotifyChannel{
		{Kind: "telegram", Enabled: true, Events: []string{"print_started"}},
	}}
	if got := notifyHeader(n); got != "{}" {
		t.Fatalf("header = %s, want {}", got)
	}
}

func TestNotifyHeader_NilAndNoChannels(t *testing.T) {
	if got := notifyHeader(nil); got != "{}" {
		t.Fatalf("header = %s, want {}", got)
	}
	if got := notifyHeader(&config.Notifications{}); got != "{}" {
		t.Fatalf("header = %s, want {}", got)
	}
}

func TestNotifyHeader_NeverContainsSecrets(t *testing.T) {
	n := &config.Notifications{Channels: []config.NotifyChannel{
		{Kind: "telegram", Enabled: true, Events: []string{"print_finished"},
			URL: "https://hooks.example/SECRET", Token: "TOKENVALUE", Target: "12345", Name: "My Chat"},
	}}
	got := notifyHeader(n)
	for _, secret := range []string{"https://hooks.example/SECRET", "TOKENVALUE", "12345", "My Chat"} {
		if strings.Contains(got, secret) {
			t.Fatalf("header = %s, must not contain %q", got, secret)
		}
	}
}

func TestFoxtrackAlertsView_StoreAndExpiry(t *testing.T) {
	resetAppAlerts()
	now := time.Now()
	storeAppAlerts(map[string][]string{"telegram": {"print_error", "print_finished"}}, now)

	got := foxtrackAlertsView(now.Add(9 * time.Minute))
	if got == nil {
		t.Fatalf("view at 9 min = nil, want the stored map")
	}
	want := []string{"print_failed", "print_finished"}
	if len(got["telegram"]) != 2 || got["telegram"][0] != want[0] || got["telegram"][1] != want[1] {
		t.Fatalf("view = %v, want telegram %v", got["telegram"], want)
	}

	if got := foxtrackAlertsView(now.Add(11 * time.Minute)); got != nil {
		t.Fatalf("view at 11 min = %v, want nil", got)
	}
}

func TestFoxtrackAlertsView_EmptyMapAndBeforeStore(t *testing.T) {
	resetAppAlerts()
	if got := foxtrackAlertsView(time.Now()); got != nil {
		t.Fatalf("view before any store = %v, want nil", got)
	}
	storeAppAlerts(map[string][]string{}, time.Now())
	got := foxtrackAlertsView(time.Now())
	if got == nil {
		t.Fatalf("view of an empty stored map = nil, want a non-nil empty map")
	}
	if len(got) != 0 {
		t.Fatalf("view = %v, want empty", got)
	}
}

func TestRedactNotifications_FoxtrackAlerts(t *testing.T) {
	resetAppAlerts()
	t.Cleanup(resetAppAlerts)

	body, err := json.Marshal(redactNotifications(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"foxtrack_alerts":null`) {
		t.Fatalf("body = %s, want \"foxtrack_alerts\":null", body)
	}

	storeAppAlerts(map[string][]string{"telegram": {"print_error"}}, time.Now())
	v := redactNotifications(nil)
	if v.FoxTrackAlerts == nil {
		t.Fatalf("FoxTrackAlerts = nil, want the stored map")
	}
	if len(v.FoxTrackAlerts["telegram"]) != 1 || v.FoxTrackAlerts["telegram"][0] != "print_failed" {
		t.Fatalf("FoxTrackAlerts = %v, want telegram [print_failed]", v.FoxTrackAlerts)
	}
}
