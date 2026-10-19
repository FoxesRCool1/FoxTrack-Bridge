package mqtt

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/webhook"
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

// Spool identity comes from each AMS tray and the external spool. remain and
// tray_weight arrive as numbers or strings, an odd value reads as 0 without
// costing the report, and an all-zero tag means the spool has none.
func TestHandler_TrayIdentity(t *testing.T) {
	const name = "tray-id-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})

	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","bed_temper":30,"ams":{"tray_now":"0","ams":[{"id":"0","tray":[
		{"id":"0","tray_type":"PLA","tray_color":"FF8800FF","remain":87,"tag_uid":"A1B2C3D4E5F60708","tray_uuid":"0123456789ABCDEF0123456789ABCDEF","tray_sub_brands":"PLA Basic","tray_weight":"1000","tray_info_idx":"GFA00"},
		{"id":"1","tray_type":"PETG","tray_color":"000000FF","remain":"-1","tag_uid":"0000000000000000","tray_uuid":"00000000000000000000000000000000","tray_weight":"1000.0","tray_info_idx":"GFG99"},
		{"id":"2","tray_type":"PLA","tray_color":"FFFFFFFF","remain":50,"tray_weight":"heavy"},
		{"id":"3"}]}]},
		"vt_tray":{"id":"254","tray_type":"TPU","tray_color":"112233FF","remain":"40","tray_weight":250,"tag_uid":"0000000000000000","tray_sub_brands":"TPU 95A","tray_info_idx":"GFU01"}}}`})

	st := GetPrinterState(name)
	if st.BedTemp != 30 {
		t.Fatalf("report lost: %+v", st)
	}
	want := []AmsSlot{
		{Slot: 0, Color: "FF8800", Material: "PLA", Active: true, Remaining: 87, TagUID: "A1B2C3D4E5F60708", TrayUUID: "0123456789ABCDEF0123456789ABCDEF", TraySubBrands: "PLA Basic", TrayWeight: 1000, TrayInfoIdx: "GFA00"},
		{Slot: 1, Color: "000000", Material: "PETG", Remaining: -1, TrayWeight: 1000, TrayInfoIdx: "GFG99"},
		{Slot: 2, Color: "FFFFFF", Material: "PLA", Remaining: 50},
		{Slot: 3},
	}
	if !reflect.DeepEqual(st.AMS, want) {
		t.Fatalf("ams\n got %+v\nwant %+v", st.AMS, want)
	}
	if ext := st.ExternalSpool; ext == nil || *ext != (AmsSlot{Slot: 254, Color: "112233", Material: "TPU", Remaining: 40, TraySubBrands: "TPU 95A", TrayWeight: 250, TrayInfoIdx: "GFU01"}) {
		t.Fatalf("external spool = %+v", ext)
	}

	for slot, wantJSON := range map[int]string{
		0: `{"slot":0,"color":"FF8800","material":"PLA","remaining":87,"active":true,"tag_uid":"A1B2C3D4E5F60708","tray_uuid":"0123456789ABCDEF0123456789ABCDEF","tray_sub_brands":"PLA Basic","tray_weight":1000,"tray_info_idx":"GFA00"}`,
		3: `{"slot":3,"color":"","material":"","remaining":0,"active":false}`,
	} {
		if b, _ := json.Marshal(relaySlot(st.AMS[slot])); string(b) != wantJSON {
			t.Fatalf("relay slot %d\n got %s\nwant %s", slot, b, wantJSON)
		}
	}
}

func TestPairFilaments(t *testing.T) {
	pla := AmsSlot{Slot: 0, Material: "PLA", Color: "FF8800", TagUID: "AA", TrayWeight: 1000, Remaining: 80}
	remain := func(s AmsSlot, r int) AmsSlot { s.Remaining = r; return s }
	plaUse := func(start, end int) webhook.HistoryFilament {
		return webhook.HistoryFilament{Slot: 0, Material: "PLA", Color: "FF8800", TagUID: "AA", TrayWeight: 1000, RemainStart: start, RemainEnd: end}
	}
	swapped := remain(pla, 100)
	swapped.TagUID = "BB"
	ext := AmsSlot{Slot: ExternalSlot, Material: "TPU"}
	unknown := AmsSlot{Slot: 1, Material: "PETG", Remaining: -1}

	cases := []struct {
		name       string
		start, end []AmsSlot
		want       []webhook.HistoryFilament
	}{
		{"used", []AmsSlot{pla}, []AmsSlot{remain(pla, 72)}, []webhook.HistoryFilament{plaUse(80, 72)}},
		{"unchanged is still sent", []AmsSlot{pla}, []AmsSlot{pla}, []webhook.HistoryFilament{plaUse(80, 80)}},
		{"gone at the end", []AmsSlot{pla}, nil, []webhook.HistoryFilament{plaUse(80, -1)}},
		{"new at the end", nil, []AmsSlot{pla}, []webhook.HistoryFilament{plaUse(-1, 80)}},
		{"tagged spool swapped", []AmsSlot{pla}, []AmsSlot{swapped}, []webhook.HistoryFilament{plaUse(80, -1)}},
		{"unknown remain, external spool, slot order", []AmsSlot{ext, unknown}, []AmsSlot{unknown, ext},
			[]webhook.HistoryFilament{
				{Slot: 1, Material: "PETG", RemainStart: -1, RemainEnd: -1},
				{Slot: ExternalSlot, Material: "TPU", RemainStart: -1, RemainEnd: -1},
			}},
	}
	for _, c := range cases {
		if got := pairFilaments(c.start, c.end); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// A print's history pairs each spool at the start with the same spool at the
// end. A pause and a short reconnect stay inside the print; a different file
// after a reconnect starts over.
func TestHandler_FilamentUseHistory(t *testing.T) {
	const name = "filament-use-test"
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // history.Append writes here
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
	send := func(payload string) { handle(nil, fakeMessage{payload}) }
	ams := func(remain int) string {
		return `"ams":{"tray_now":"0","ams":[{"id":"0","tray":[{"id":"0","tray_type":"PLA","tray_color":"FF8800FF","remain":` +
			strconv.Itoa(remain) + `,"tag_uid":"AA00000000000001","tray_weight":"1000"},{"id":"1","tray_type":"PETG","tray_color":"000000FF","remain":-1}]}]}`
	}
	session := func() *printSession {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		return activeSessions[name]
	}
	history := func() webhook.HistoryPayload {
		t.Helper()
		select {
		case p := <-got:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no history sent")
			return webhook.HistoryPayload{}
		}
	}

	send(`{"print":{"gcode_state":"IDLE",` + ams(80) + `,"vt_tray":{"id":"254","tray_type":"TPU","tray_color":"112233FF","remain":0}}}`)
	send(`{"print":{"gcode_state":"RUNNING","subtask_name":"cube","mc_percent":1}}`)
	start := session()
	if start == nil || len(start.Spools) != 3 || start.Spools[0].Remaining != 80 {
		t.Fatalf("start snapshot = %+v", start)
	}
	send(`{"print":{"mc_percent":40,` + ams(76) + `}}`)
	send(`{"print":{"gcode_state":"PAUSE"}}`)
	send(`{"print":{"gcode_state":"RUNNING"}}`)
	UpdatePrinterState(name, TelemetryData{Status: "connected"}) // the MQTT connection dropped and came back
	send(`{"print":{"gcode_state":"RUNNING","subtask_name":"cube","mc_percent":60,` + ams(74) + `}}`)
	if session() != start {
		t.Fatal("pause or reconnect started a new session")
	}
	send(`{"print":{"gcode_state":"FINISH","mc_percent":100}}`)

	h := history()
	wantFil := []webhook.HistoryFilament{
		{Slot: 0, Material: "PLA", Color: "FF8800", TagUID: "AA00000000000001", TrayWeight: 1000, RemainStart: 80, RemainEnd: 74},
		{Slot: 1, Material: "PETG", Color: "000000", RemainStart: -1, RemainEnd: -1},
		{Slot: ExternalSlot, Material: "TPU", Color: "112233", RemainStart: -1, RemainEnd: -1},
	}
	if h.Result != "finished" || h.FileName != "cube" || h.PercentAtEnd != 100 || !reflect.DeepEqual(h.Filaments, wantFil) {
		t.Fatalf("history = %+v", h)
	}

	send(`{"print":{"gcode_state":"RUNNING","subtask_name":"plate","mc_percent":5}}`)
	first := session()
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	send(`{"print":{"gcode_state":"RUNNING","subtask_name":"other","mc_percent":40,` + ams(70) + `}}`)
	if s := session(); s == nil || s == first || s.FileName != "other" {
		t.Fatalf("a different file after a reconnect kept the old session: %+v", s)
	}
	send(`{"print":{"gcode_state":"IDLE"}}`)
	if h := history(); h.Result != "cancelled" || h.PercentAtEnd != 40 || h.FileName != "other" || len(h.Filaments) != 2 || h.Filaments[0].RemainStart != 70 {
		t.Fatalf("cancelled history = %+v", h)
	}
	select {
	case p := <-got:
		t.Fatalf("extra history sent: %+v", p)
	default:
	}
}
