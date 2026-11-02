package lan

import (
	"testing"

	"foxtrack-bridge/webhook"
)

func TestKlipperAlarms_ShutdownIsFatalPrintErrorIsSerious(t *testing.T) {
	a := klipperAlarms(
		map[string]interface{}{"state": "shutdown", "state_message": "MCU 'mcu' shutdown: Timer too close\nThis often indicates the host computer is overloaded."},
		map[string]interface{}{"state": "error", "message": "Lost communication with MCU 'mcu'"},
	)
	if len(a) != 2 || a[0].Severity != webhook.AlarmFatal || a[1].Severity != webhook.AlarmSerious ||
		a[0].Source != webhook.AlarmSourceMessage || a[0].Code != nil ||
		*a[0].Message != "MCU 'mcu' shutdown: Timer too close\nThis often indicates the host computer is overloaded." ||
		*a[1].Message != "Lost communication with MCU 'mcu'" {
		t.Fatalf("alarms = %+v", a)
	}
	if a := klipperAlarms(map[string]interface{}{"state": "ready", "state_message": "Printer is ready"}, map[string]interface{}{"state": "printing"}); a != nil {
		t.Fatalf("ready must give no alarm, got %+v", a)
	}
}

func TestPrusaLinkAlarms_StatusPrinter(t *testing.T) {
	a := prusaLinkAlarms(map[string]interface{}{"state": "ERROR", "status_printer": map[string]interface{}{"ok": false, "message": "Heatbed thermistor failure"}})
	if len(a) != 1 || a[0].Severity != webhook.AlarmSerious || *a[0].Message != "Heatbed thermistor failure" {
		t.Fatalf("alarms = %+v", a)
	}
	a = prusaLinkAlarms(map[string]interface{}{"state": "ATTENTION"})
	if len(a) != 1 || *a[0].Message != "The printer needs attention. Check its screen." {
		t.Fatalf("attention = %+v", a)
	}
	if a := prusaLinkAlarms(map[string]interface{}{"state": "PRINTING", "status_printer": map[string]interface{}{"ok": true, "message": ""}}); a != nil {
		t.Fatalf("ok must give no alarm, got %+v", a)
	}
}

func TestCleanAlarms_NeverNilDropsEmptyCaps20(t *testing.T) {
	if got := webhook.CleanAlarms(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil in = %#v", got)
	}
	empty := ""
	in := []webhook.RelayAlarm{{Source: "hms", Code: &empty, Severity: "serious"}}
	for i := 0; i < 25; i++ {
		in = append(in, webhook.MessageAlarm("weird", "m"))
	}
	got := webhook.CleanAlarms(in)
	if len(got) != 20 || got[0].Severity != webhook.AlarmCommon || got[0].Source != webhook.AlarmSourceMessage {
		t.Fatalf("clean = %d items, first %+v", len(got), got[0])
	}
}
