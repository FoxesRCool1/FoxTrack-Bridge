package mqtt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The external spool comes from vt_tray, survives partial reports, follows
// tray_now, and an odd vt_tray never costs the rest of the report.
func TestHandler_ExternalSpool(t *testing.T) {
	const name = "ext-spool-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})

	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","vt_tray":{"id":"254","tray_type":"PETG","tray_color":"FF8800FF","remain":-1}}}`})
	ext := GetPrinterState(name).ExternalSpool
	if ext == nil || *ext != (AmsSlot{Slot: 254, Color: "FF8800", Material: "PETG"}) {
		t.Fatalf("external spool = %+v", ext)
	}

	handle(nil, fakeMessage{`{"print":{"nozzle_temper":200,"ams":{"tray_now":"254"}}}`}) // partial: keep it, now loaded
	if ext := GetPrinterState(name).ExternalSpool; ext == nil || ext.Material != "PETG" || !ext.Active {
		t.Fatalf("after partial report: %+v", ext)
	}

	handle(nil, fakeMessage{`{"print":{"vt_tray":{"remain":55}}}`}) // only some keys: the rest stays
	if ext := GetPrinterState(name).ExternalSpool; ext == nil || *ext != (AmsSlot{Slot: 254, Color: "FF8800", Material: "PETG", Remaining: 55, Active: true}) {
		t.Fatalf("after partial vt_tray: %+v", ext)
	}

	handle(nil, fakeMessage{`{"print":{"vt_tray":{"tray_type":"","tray_color":"00000000"}}}`}) // emptied; only vt_tray in the message
	if ext := GetPrinterState(name).ExternalSpool; ext == nil || ext.Material != "" || ext.Color != "" || !ext.Active {
		t.Fatalf("empty spool: %+v", ext)
	}

	handle(nil, fakeMessage{`{"print":{"bed_temper":60,"vt_tray":{"tray_type":7}}}`}) // odd type: spool ignored, bed kept
	if st := GetPrinterState(name); st.BedTemp != 60 {
		t.Fatalf("odd vt_tray lost the report: %+v", st)
	}
}

func TestFilamentChanged(t *testing.T) {
	base := TelemetryData{AMS: []AmsSlot{{Slot: 0, Color: "FF0000", Material: "PLA", Remaining: 80}}, ExternalSpool: &AmsSlot{Slot: 254, Material: "PLA"}}
	same := base
	same.AMS = []AmsSlot{{Slot: 0, Color: "FF0000", Material: "PLA", Remaining: 79}} // remaining only
	same.ExternalSpool = &AmsSlot{Slot: 254, Material: "PLA"}
	if filamentChanged(&base, &same) {
		t.Fatal("a remaining change counted as a filament change")
	}
	color := same
	color.AMS = []AmsSlot{{Slot: 0, Color: "00FF00", Material: "PLA"}}
	ext := same
	ext.ExternalSpool = &AmsSlot{Slot: 254, Material: "PETG"}
	for _, c := range []TelemetryData{color, ext} {
		if !filamentChanged(&base, &c) || !UrgentChange(&base, &c) {
			t.Fatalf("missed change: %+v", c)
		}
	}
}

func TestBuildFilamentSetting_Golden(t *testing.T) {
	got := string(buildFilamentSetting("7", 0, 2, 2, "PETG", "FF8800", filamentPresets["PETG"]))
	want := `{"print":{"ams_id":0,"command":"ams_filament_setting","nozzle_temp_max":270,"nozzle_temp_min":220,"sequence_id":"7","slot_id":2,"tray_color":"FF8800FF","tray_id":2,"tray_info_idx":"GFG99","tray_sub_brands":"","tray_type":"PETG"}}`
	if got != want {
		t.Fatalf("payload\n got %s\nwant %s", got, want)
	}
}

// setupFilament fakes a connected printer with two AMS units; publish runs
// printer (nil = the printer ignores the command).
func setupFilament(t *testing.T, name string, printer func(payload map[string]any)) *[]map[string]any {
	t.Helper()
	var sent []map[string]any
	prevPub, prevPush, prevWait, prevPoll := publishRequestFn, requestPushall, filamentVerifyWait, filamentVerifyPoll
	t.Cleanup(func() {
		publishRequestFn, requestPushall, filamentVerifyWait, filamentVerifyPoll = prevPub, prevPush, prevWait, prevPoll
		RemovePrinterState(name)
		forgetPrintFileState(name)
	})
	filamentVerifyWait, filamentVerifyPoll = 300*time.Millisecond, 5*time.Millisecond
	requestPushall = func(string) {}
	publishRequestFn = func(_, _ string, payload []byte) error {
		var m struct {
			Print map[string]any `json:"print"`
		}
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, m.Print)
		if printer != nil {
			printer(m.Print)
		}
		return nil
	}
	RemovePrinterState(name)
	UpdatePrinterState(name, TelemetryData{Status: "idle", AMS: []AmsSlot{{Slot: 0}, {Slot: 4, Material: "PLA", Color: "FFFFFF"}}})
	setTrayRefs(name, []trayRef{{Slot: 0, Unit: 0, Tray: 0}, {Slot: 4, Unit: 1, Tray: 0}, {Slot: 8, Unit: 128, Tray: 0}})
	return &sent
}

func TestSetFilament_AMSSlotIsVerified(t *testing.T) {
	const name = "fil-ams"
	sent := setupFilament(t, name, func(p map[string]any) {
		go UpdatePrinterState(name, TelemetryData{Status: "idle", AMS: []AmsSlot{{Slot: 0}, {Slot: 4, Material: p["tray_type"].(string), Color: p["tray_color"].(string)[:6]}}})
	})
	if err := SetFilament(name, "01P00A123", 4, " petg ", "#ff8800"); err != nil {
		t.Fatalf("SetFilament: %v", err)
	}
	p := (*sent)[0]
	if p["ams_id"] != 1.0 || p["tray_id"] != 0.0 || p["slot_id"] != 0.0 || p["tray_color"] != "FF8800FF" || p["tray_type"] != "PETG" || p["tray_info_idx"] != "GFG99" {
		t.Fatalf("payload = %v", p)
	}
}

func TestSetFilament_ExternalSpoolIds(t *testing.T) {
	const name = "fil-ext"
	sent := setupFilament(t, name, func(p map[string]any) {
		go UpdatePrinterState(name, TelemetryData{Status: "idle", ExternalSpool: &AmsSlot{Slot: ExternalSlot, Material: "TPU", Color: "000000"}})
	})
	if err := SetFilament(name, "01P00A123", ExternalSlot, "TPU", "000000"); err != nil {
		t.Fatalf("SetFilament: %v", err)
	}
	if p := (*sent)[0]; p["ams_id"] != 255.0 || p["tray_id"] != 254.0 || p["slot_id"] != 0.0 {
		t.Fatalf("payload = %v", p)
	}
}

func TestSetFilament_PrinterRefusesOrIgnores(t *testing.T) {
	const name = "fil-refuse"
	setupFilament(t, name, func(map[string]any) {
		noteCommandReply(name, []byte(`{"print":{"command":"ams_filament_setting","result":"failed","reason":"mqtt message verify failed"}}`))
	})
	err := SetFilament(name, "01P00A123", 0, "PLA", "FF0000")
	if err == nil || err.Error() != "The printer refused the change (mqtt message verify failed). Check that Developer Mode is on, then try again." {
		t.Fatalf("err = %v", err)
	}

	setupFilament(t, "fil-ignore", nil) // "success" without storing it
	if err := SetFilament("fil-ignore", "01P00A123", 0, "PLA", "FF0000"); err == nil || !strings.Contains(err.Error(), "did not take the change") {
		t.Fatalf("err = %v", err)
	}

	setupFilament(t, "fil-gone", func(map[string]any) {
		UpdatePrinterState("fil-gone", TelemetryData{Status: "disconnected"})
	})
	if err := SetFilament("fil-gone", "01P00A123", 0, "PLA", "FF0000"); err == nil || !strings.Contains(err.Error(), "went offline") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetFilament_RefusedBeforeSending(t *testing.T) {
	const name = "fil-bad"
	sent := setupFilament(t, name, nil)
	cases := []struct {
		serial, material, color string
		slot                    int
		want                    string
	}{
		{"01P00A123", "WOOD", "FF0000", 0, "does not know that material"},
		{"01P00A123", "PLA", "FF00", 0, "6 hex digits"},
		{"01P00A123", "PLA", "GG0000", 0, "6 hex digits"},
		{"01P00A123", "PLA", "FF0000", 12, "AMS slot 13 is not on the printer"},
		{"01P00A123", "PLA", "FF0000", 8, "AMS HT"},
		{"09400A123", "PLA", "FF0000", ExternalSlot, "not supported on this printer model"},
	}
	for _, c := range cases {
		if err := SetFilament(name, c.serial, c.slot, c.material, c.color); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: err = %v", c, err)
		}
	}
	if err := setFilamentFromArgs(name, "01P00A123", map[string]interface{}{"material": "PLA", "color": "FF0000"}); err == nil {
		t.Fatal("missing slot accepted")
	}
	if len(*sent) != 0 {
		t.Fatalf("sent %v", *sent)
	}
}
