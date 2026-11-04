package mqtt

import (
	"strings"
	"testing"
	"unicode/utf8"

	"foxtrack-bridge/webhook"
)

func TestHMSTable_LoadsAndFitsFoxTrack(t *testing.T) {
	loadHMSTable()
	if len(hmsTable.HMS) < 1000 || len(hmsTable.Err) < 300 {
		t.Fatalf("table has %d HMS and %d error texts: the embedded file did not load", len(hmsTable.HMS), len(hmsTable.Err))
	}
	for code, text := range hmsTable.HMS {
		if len(code) != 16 || text == "" || utf8.RuneCountInString(clipAlarmText(text)) > alarmTextMax {
			t.Fatalf("HMS %q: bad key or text %q", code, text)
		}
	}
	for code, text := range hmsTable.Err {
		if len(code) != 8 || text == "" || utf8.RuneCountInString(clipAlarmText(text)) > alarmTextMax {
			t.Fatalf("print error %q: bad key or text %q", code, text)
		}
	}
}

func TestHMSText_ExactLookup(t *testing.T) {
	// 0C00_0300_0003_0008: Bambu's spaghetti detection.
	if got := hmsText(0x0C000300, 0x00030008); !strings.Contains(strings.ToLower(got), "spaghetti") {
		t.Fatalf("hmsText = %q, want the spaghetti text", got)
	}
	// 0300_400C: the print was cancelled.
	if got := printErrorText(0x0300400C); !strings.Contains(strings.ToLower(got), "cancel") {
		t.Fatalf("printErrorText = %q, want the cancelled text", got)
	}
}

func TestHMSText_UnknownStaysEmpty(t *testing.T) {
	if got := hmsText(0x0C000300, 0x0003FFFF); got != "" {
		t.Fatalf("unknown HMS code got text %q", got)
	}
	if got := printErrorText(0x0300FFFF); got != "" {
		t.Fatalf("unknown print error got text %q", got)
	}
	// The two tables are separate: an 8 digit error is not an HMS entry.
	if got := hmsText(0, 0x0300400C); got != "" {
		t.Fatalf("HMS lookup read the error table: %q", got)
	}
}

func TestMergeAlarms_CarriesBambuText(t *testing.T) {
	// attr 0x0C000300 = 201327360, code 0x00030008 = 196616; print_error 0x0300400C = 50348044.
	got := mergeAlarms(nil, []byte(`{"print":{"hms":[{"attr":201327360,"code":196616},{"attr":201327360,"code":262143}],"print_error":50348044}}`))
	if len(got) != 3 {
		t.Fatalf("alarms = %v", codes(got))
	}
	byCode := map[string]webhook.RelayAlarm{}
	for _, a := range got {
		byCode[strPtr(a.Code)] = a
	}
	if m := byCode["0C00_0300_0003_0008"].Message; m == nil || !strings.Contains(strings.ToLower(*m), "spaghetti") {
		t.Fatalf("known HMS alarm message = %v", m)
	}
	if m := byCode["0300_400C"].Message; m == nil || *m == "" {
		t.Fatalf("print error alarm has no message")
	}
	if m := byCode["0C00_0300_0003_FFFF"].Message; m != nil {
		t.Fatalf("unknown code got message %q, want null", *m)
	}
}

func TestClipAlarmText(t *testing.T) {
	if got := clipAlarmText("Short text."); got != "Short text." {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("Check the filament path. ", 20) // 500 chars
	if got := clipAlarmText(long); utf8.RuneCountInString(got) > alarmTextMax || !strings.HasSuffix(got, ".") {
		t.Fatalf("long text clipped to %d runes, %q", utf8.RuneCountInString(got), got[len(got)-10:])
	}
	noStops := strings.Repeat("word ", 100)
	if got := clipAlarmText(noStops); utf8.RuneCountInString(got) > alarmTextMax || !strings.HasSuffix(got, "...") {
		t.Fatalf("text without sentences clipped to %d runes, ends %q", utf8.RuneCountInString(got), got[len(got)-6:])
	}
	multi := strings.Repeat("é", 400)
	if got := clipAlarmText(multi); !utf8.ValidString(got) || utf8.RuneCountInString(got) > alarmTextMax {
		t.Fatalf("multi-byte text broken by the clip")
	}
}
