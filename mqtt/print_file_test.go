package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/ftps"
)

func TestModelFromSerial(t *testing.T) {
	for serial, want := range map[string]string{
		"00M09A123456789": "Bambu Lab X1 Carbon",
		"00W0AA":          "Bambu Lab X1",
		"03W1":            "Bambu Lab X1E",
		"01S00C":          "Bambu Lab P1P",
		"01P00A":          "Bambu Lab P1S",
		"0309AB":          "Bambu Lab A1 mini",
		"03900X":          "Bambu Lab A1",
		"094ABC":          "Bambu Lab H2D",
		"239ABC":          "Bambu Lab H2D Pro",
		"093ABC":          "Bambu Lab H2S",
		"22EABC":          "Bambu Lab P2S",
		"31BABC":          "Bambu Lab H2C",
		"20PABC":          "Bambu Lab X2D",
		"26AABC":          "Bambu Lab A2L",
		"01p00a":          "Bambu Lab P1S",
		"999":             "",
		"01":              "",
		"":                "",
	} {
		if got := ModelFromSerial(serial); got != want {
			t.Errorf("ModelFromSerial(%q) = %q, want %q", serial, got, want)
		}
	}
}

func TestNewJobID_UniqueNonZeroBelow2e31(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := newJobID()
		if id == "0" || id == "" || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
		var n int64
		if err := json.Unmarshal([]byte(id), &n); err != nil || n <= 0 || n >= 1<<31 {
			t.Fatalf("id %q out of range", id)
		}
	}
}

func TestBuildProjectFile_Golden(t *testing.T) {
	b, err := buildProjectFile(projectFileInput{
		SeqID: "7", ID: "123456", Plate: 2, RemoteName: "Benchy.gcode.3mf", TaskName: "Benchy",
		MD5: "ABCDEF0123456789ABCDEF0123456789", Timelapse: true, BedLeveling: true, UseAMS: true,
		AMSMapping:  []int{-1, 5},
		AMSMapping2: []amsRef{{255, 255}, {1, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Print map[string]any `json:"print"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"sequence_id": "7", "command": "project_file", "param": "Metadata/plate_2.gcode",
		"url": "ftp://Benchy.gcode.3mf", "file": "Benchy.gcode.3mf",
		"md5": "abcdef0123456789abcdef0123456789", "bed_type": "auto", "timelapse": true,
		"bed_leveling": true, "auto_bed_leveling": 1.0, "flow_cali": false, "vibration_cali": true,
		"layer_inspect": false, "use_ams": true, "extrude_cali_flag": 2.0, "extrude_cali_manual_mode": 0.0,
		"nozzle_offset_cali": 0.0, "subtask_name": "Benchy", "profile_id": "0",
		"project_id": "123456", "subtask_id": "123456", "task_id": "123456",
		"ams_mapping":  []any{-1.0, 5.0},
		"ams_mapping2": []any{map[string]any{"ams_id": 255.0, "slot_id": 255.0}, map[string]any{"ams_id": 1.0, "slot_id": 1.0}},
	}
	if !reflect.DeepEqual(got.Print, want) {
		t.Fatalf("payload mismatch\n got: %v\nwant: %v", got.Print, want)
	}
	if !strings.Contains(string(b), `"bed_leveling"`) || strings.Contains(string(b), "bed_levelling") {
		t.Fatal("wire spelling must be bed_leveling")
	}
}

func TestBuildProjectFile_Variants(t *testing.T) {
	get := func(in projectFileInput) map[string]any {
		b, err := buildProjectFile(in)
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Print map[string]any `json:"print"`
		}
		json.Unmarshal(b, &o)
		return o.Print
	}
	p := get(projectFileInput{ID: "1", Plate: 1, RemoteName: "a.gcode.3mf", P2S: true, DualNozzle: true})
	if p["vibration_cali"] != false || p["nozzle_offset_cali"] != 2.0 {
		t.Errorf("p2s/dual: vibration_cali=%v nozzle_offset_cali=%v", p["vibration_cali"], p["nozzle_offset_cali"])
	}
	if p["bed_leveling"] != false || p["auto_bed_leveling"] != 2.0 || p["md5"] != "" {
		t.Errorf("defaults: %v %v md5=%q", p["bed_leveling"], p["auto_bed_leveling"], p["md5"])
	}
	if _, ok := p["ams_mapping"]; ok {
		t.Error("empty mapping must be omitted")
	}
}

var testTrays = []trayRef{
	{0, 0, 0}, {1, 0, 1}, {2, 0, 2}, {3, 0, 3}, // AMS 0
	{4, 1, 0}, {5, 1, 1}, {6, 1, 2}, {7, 1, 3}, // AMS 1
	{8, 128, 0}, // AMS HT
}

func TestBuildAMSMapping_PositionalToAbsolute(t *testing.T) {
	flat, ref2, useAMS, err := buildAMSMapping(amsRequest{
		// project has 5 filaments; the plate uses positions 0, 1, 3 and 4
		Requested: []int{0, 5, -1, 8, 3}, Count: 5, Used: []int{0, 1, 3, 4},
		HasAMS: true, Trays: testTrays,
	})
	if err != nil {
		t.Fatal(err)
	}
	// position 0: AMS 0 tray 0 -> 0; 1: AMS 1 tray 1 -> 5; 2 unused; 3: HT -> 128; 4: AMS 0 tray 3 -> 3
	if want := []int{0, 5, -1, 128, 3}; !reflect.DeepEqual(flat, want) {
		t.Errorf("flat = %v, want %v", flat, want)
	}
	want2 := []amsRef{{0, 0}, {1, 1}, {255, 255}, {128, 0}, {0, 3}}
	if !reflect.DeepEqual(ref2, want2) {
		t.Errorf("ams_mapping2 = %v, want %v", ref2, want2)
	}
	if !useAMS {
		t.Error("use_ams must be true")
	}
}

func TestBuildAMSMapping_PadsToProjectCountAndBlanksUnusedPositions(t *testing.T) {
	// FoxTrack sent two entries, the project has four filaments, and the
	// plate uses only the first. Position 1 was given a slot but is unused.
	flat, ref2, useAMS, err := buildAMSMapping(amsRequest{
		Requested: []int{4, 2}, Count: 4, Used: []int{0}, HasAMS: true, Trays: testTrays,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{4, -1, -1, -1}; !reflect.DeepEqual(flat, want) {
		t.Errorf("flat = %v, want %v", flat, want)
	}
	if len(ref2) != 4 || ref2[0] != (amsRef{1, 0}) || ref2[3] != (amsRef{255, 255}) || !useAMS {
		t.Errorf("ref2 = %v useAMS=%v", ref2, useAMS)
	}
}

func TestBuildAMSMapping_ExternalSpool(t *testing.T) {
	// Chosen explicitly (254), printer has an AMS.
	flat, ref2, useAMS, err := buildAMSMapping(amsRequest{Requested: []int{254}, Count: 1, HasAMS: true, Trays: testTrays})
	if err != nil || useAMS || !reflect.DeepEqual(flat, []int{-1}) || !reflect.DeepEqual(ref2, []amsRef{{255, 0}}) {
		t.Fatalf("explicit: flat=%v ref2=%v useAMS=%v err=%v", flat, ref2, useAMS, err)
	}
	// No mapping, no AMS on the printer.
	flat, ref2, useAMS, err = buildAMSMapping(amsRequest{Count: 2})
	if err != nil || useAMS || len(flat) != 2 || ref2[1] != (amsRef{255, 0}) {
		t.Fatalf("no AMS: flat=%v ref2=%v useAMS=%v err=%v", flat, ref2, useAMS, err)
	}
	// Dual nozzle keeps 254 (deputy), single nozzle turns it into 255.
	_, ref2, _, _ = buildAMSMapping(amsRequest{Requested: []int{254}, Count: 1, DualNozzle: true})
	if ref2[0] != (amsRef{254, 0}) {
		t.Errorf("dual nozzle ref2 = %v", ref2)
	}
}

func TestBuildAMSMapping_Errors(t *testing.T) {
	cases := map[string]struct {
		r    amsRequest
		want error
	}{
		"no mapping but AMS present":  {amsRequest{Count: 1, HasAMS: true, Trays: testTrays}, errPrintPickSlots},
		"used filament left unmapped": {amsRequest{Requested: []int{0, -1}, Count: 2, HasAMS: true, Trays: testTrays}, errPrintPickSlots},
		"AMS and external mixed":      {amsRequest{Requested: []int{0, 254}, Count: 2, HasAMS: true, Trays: testTrays}, errPrintMixed},
	}
	for name, c := range cases {
		if _, _, _, err := buildAMSMapping(c.r); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// A slot the printer does not report.
	if _, _, _, err := buildAMSMapping(amsRequest{Requested: []int{9}, Count: 1, HasAMS: true, Trays: testTrays}); err == nil ||
		!strings.Contains(err.Error(), "AMS slot 10 is not on the printer") {
		t.Errorf("unknown slot: err = %v", err)
	}
	// "No AMS" chosen in the dialog lets a printer with an AMS use the spool.
	if _, _, useAMS, err := buildAMSMapping(amsRequest{Count: 1, HasAMS: true, ForceExt: true}); err != nil || useAMS {
		t.Errorf("ForceExt: useAMS=%v err=%v", useAMS, err)
	}
}

func TestNewTrayRef(t *testing.T) {
	if got := newTrayRef(9, 2, 1, "128", "0"); got != (trayRef{9, 128, 0}) {
		t.Errorf("got %+v", got)
	}
	if got := newTrayRef(5, 1, 1, "", "x"); got != (trayRef{5, 1, 1}) { // unparsable ids fall back to the position
		t.Errorf("got %+v", got)
	}
}

// The report handler keeps the raw unit and tray ids per positional slot.
func TestHandler_KeepsRawAMSIds(t *testing.T) {
	const name = "print-file-ams-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })
	handle := makeHandler(Printer{Name: name, Serial: "TEST"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","ams":{"tray_now":"255","ams":[
		{"id":"1","tray":[{"id":"0","tray_color":"FF0000FF","tray_type":"PLA"},{"id":"1","tray_color":"00FF00FF","tray_type":"PLA"}]},
		{"id":"128","tray":[{"id":"0","tray_color":"0000FFFF","tray_type":"PETG"}]}]}}}`})
	want := []trayRef{{0, 1, 0}, {1, 1, 1}, {4, 128, 0}}
	if got := getTrayRefs(name); !reflect.DeepEqual(got, want) {
		t.Fatalf("tray refs = %+v, want %+v", got, want)
	}
	// What FoxTrack sees is unchanged: positional slots 0, 1, 4.
	st := GetPrinterState(name)
	if len(st.AMS) != 3 || st.AMS[2].Slot != 4 {
		t.Fatalf("AMS slots = %+v", st.AMS)
	}
}

func TestNoteReportedError(t *testing.T) {
	const name = "print-err-test"
	t.Cleanup(func() { forgetPrintFileState(name) })
	noteReportedError(name, []byte(`{"print":{"print_error":50348044}}`), "")
	if got := lastReportedError(name); got != "50348044" {
		t.Fatalf("got %q", got)
	}
	noteReportedError(name, []byte(`{"print":{"nozzle_temper":20}}`), "") // nothing about errors: keep
	if got := lastReportedError(name); got != "50348044" {
		t.Fatalf("kept error lost: %q", got)
	}
	noteReportedError(name, []byte(`{"print":{"mc_print_error_code":"0","print_error":0}}`), "0")
	if got := lastReportedError(name); got != "" {
		t.Fatalf("error not cleared: %q", got)
	}
	noteReportedError(name, nil, "12345")
	if got := lastReportedError(name); got != "12345" {
		t.Fatalf("got %q", got)
	}
}

// ---- PrintProjectFile end to end, with fakes ----

type printHarness struct {
	uploads   []ftps.Config
	remote    string
	published []byte
	// uploadErrs is consumed one per upload call.
	uploadErrs []error
	// onPublish runs when the command is published.
	onPublish func()
}

func setupPrint(t *testing.T, name, serial string, status string) (*printHarness, Printer, string) {
	t.Helper()
	h := &printHarness{}
	prevUp, prevPub, prevConn, prevWait, prevPoll, prevGrace := ftpsUpload, publishProject, printerClientConnected, startWait, startPoll, startErrGrace
	t.Cleanup(func() {
		ftpsUpload, publishProject, printerClientConnected, startWait, startPoll, startErrGrace = prevUp, prevPub, prevConn, prevWait, prevPoll, prevGrace
		RemovePrinterState(name)
	})
	ftpsUpload = func(_ context.Context, cfg ftps.Config, _, remote string) error {
		h.uploads = append(h.uploads, cfg)
		h.remote = remote
		if len(h.uploadErrs) > 0 {
			err := h.uploadErrs[0]
			h.uploadErrs = h.uploadErrs[1:]
			return err
		}
		return nil
	}
	publishProject = func(_, _ string, payload []byte) error {
		h.published = payload
		if h.onPublish != nil {
			h.onPublish()
		}
		return nil
	}
	printerClientConnected = func(string) bool { return true }
	startWait, startPoll, startErrGrace = 400*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond

	RemovePrinterState(name)
	UpdatePrinterState(name, TelemetryData{Status: status, AMS: []AmsSlot{{Slot: 0}, {Slot: 1}, {Slot: 2}, {Slot: 3}}})
	setTrayRefs(name, testTrays[:4])
	path := make3MF(t, map[string]string{
		"Metadata/plate_1.gcode":           "G1",
		"Metadata/plate_1.gcode.md5":       "ABCDEF0123456789ABCDEF0123456789",
		"Metadata/project_settings.config": `{"filament_colour":["#FFFFFF","#000000"]}`,
		"Metadata/slice_info.config":       `<config><plate><metadata key="index" value="1"/><filament id="2"/></plate></config>`,
	})
	return h, Printer{Name: name, IP: "192.0.2.1", Serial: serial, LANCode: "12345678"}, path
}

func TestPrintProjectFile_Success(t *testing.T) {
	const name = "pf-ok"
	h, p, path := setupPrint(t, name, "01P00A123", "idle")
	h.onPublish = func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			UpdatePrinterState(name, TelemetryData{Status: "PREPARE"})
		}()
	}
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 2}, Timelapse: true, TaskName: "My Vase (v2)"})
	if err != nil {
		t.Fatalf("PrintProjectFile: %v", err)
	}
	// One fixed name on the printer's SD card, whatever the user's file is called.
	if h.remote != "foxtrack-print.gcode.3mf" || len(h.uploads) != 1 || h.uploads[0].ClearData || h.uploads[0].User != "bblp" || h.uploads[0].Password != "12345678" {
		t.Fatalf("upload: remote=%q cfgs=%+v", h.remote, h.uploads)
	}
	var cmd struct {
		Print struct {
			Param       string   `json:"param"`
			MD5         string   `json:"md5"`
			UseAMS      bool     `json:"use_ams"`
			AMSMapping  []int    `json:"ams_mapping"`
			AMSMapping2 []amsRef `json:"ams_mapping2"`
			Subtask     string   `json:"subtask_name"`
			File        string   `json:"file"`
			URL         string   `json:"url"`
			TaskID      string   `json:"task_id"`
		} `json:"print"`
	}
	if err := json.Unmarshal(h.published, &cmd); err != nil {
		t.Fatal(err)
	}
	c := cmd.Print
	if c.Param != "Metadata/plate_1.gcode" || c.MD5 != "" || !c.UseAMS ||
		!reflect.DeepEqual(c.AMSMapping, []int{-1, 2}) || !reflect.DeepEqual(c.AMSMapping2, []amsRef{{255, 255}, {0, 2}}) ||
		c.Subtask != "My Vase (v2)" || c.File != "foxtrack-print.gcode.3mf" || c.URL != "ftp://foxtrack-print.gcode.3mf" || c.TaskID == "" || c.TaskID == "0" {
		t.Fatalf("published %s", h.published)
	}
}

func TestPrintProjectFile_BusyPrinterNeverUploads(t *testing.T) {
	h, p, path := setupPrint(t, "pf-busy", "01P00A123", "printing")
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if !errors.Is(err, errPrintBusy) || len(h.uploads) != 0 || h.published != nil {
		t.Fatalf("err = %v, uploads=%d published=%v", err, len(h.uploads), h.published != nil)
	}
}

func TestPrintProjectFile_BusyAfterUploadNeverPublishes(t *testing.T) {
	const name = "pf-busy2"
	h, p, path := setupPrint(t, name, "01P00A123", "idle")
	ftpsUpload = func(context.Context, ftps.Config, string, string) error {
		UpdatePrinterState(name, TelemetryData{Status: "printing"}) // started from the printer screen meanwhile
		return nil
	}
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if !errors.Is(err, errPrintBusy) || h.published != nil {
		t.Fatalf("err = %v, published=%v", err, h.published != nil)
	}
}

func TestPrintProjectFile_NotConnected(t *testing.T) {
	h, p, path := setupPrint(t, "pf-conn", "01P00A123", "idle")
	printerClientConnected = func(string) bool { return false }
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if !errors.Is(err, errPrintNoClient) || len(h.uploads) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintProjectFile_PicksSlotsWhenAMSPresent(t *testing.T) {
	h, p, path := setupPrint(t, "pf-pick", "01P00A123", "idle")
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{})
	if !errors.Is(err, errPrintPickSlots) || len(h.uploads) != 0 {
		t.Fatalf("err = %v uploads=%d", err, len(h.uploads))
	}
}

func TestPrintProjectFile_NeverStarts(t *testing.T) {
	_, p, path := setupPrint(t, "pf-nostart", "01P00A123", "idle")
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if !errors.Is(err, errPrintNoStart) {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintProjectFile_PrinterRefuses(t *testing.T) {
	const name = "pf-refuse"
	h, p, path := setupPrint(t, name, "01P00A123", "idle")
	noteReportedError(name, nil, "0") // earlier error cleared
	h.onPublish = func() { noteReportedError(name, []byte(`{"print":{"print_error":83935249}}`), "") }
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if err == nil || err.Error() != "The printer refused the print (error 83935249). Check the printer screen." {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintProjectFile_OldErrorIsNotARefusal(t *testing.T) {
	const name = "pf-olderr"
	_, p, path := setupPrint(t, name, "01P00A123", "error")
	noteReportedError(name, nil, "50348044") // left over from the last failed print
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}})
	if !errors.Is(err, errPrintNoStart) {
		t.Fatalf("err = %v, want errPrintNoStart", err)
	}
}

func TestPrintProjectFile_A1RetriesWithClearDataChannel(t *testing.T) {
	const name = "pf-a1"
	h, p, path := setupPrint(t, name, "03900X123", "idle")
	h.uploadErrs = []error{ftps.ErrStalled}
	h.onPublish = func() { UpdatePrinterState(name, TelemetryData{Status: "printing"}) }
	if err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 1}}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(h.uploads) != 2 || h.uploads[0].ClearData || !h.uploads[1].ClearData {
		t.Fatalf("uploads = %+v", h.uploads)
	}
}

func TestPrintProjectFile_OtherModelsDoNotFallBackToClearText(t *testing.T) {
	h, p, path := setupPrint(t, "pf-p1s", "01P00A123", "idle")
	h.uploadErrs = []error{ftps.ErrStalled}
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 1}})
	if !errors.Is(err, errPrintUpload) || len(h.uploads) != 1 {
		t.Fatalf("err = %v uploads=%d", err, len(h.uploads))
	}
}

func TestPrintProjectFile_WrongAccessCode(t *testing.T) {
	h, p, path := setupPrint(t, "pf-login", "03900X123", "idle")
	h.uploadErrs = []error{ftps.ErrLogin}
	err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 1}})
	if !errors.Is(err, errPrintAccess) || len(h.uploads) != 1 { // no clear-text retry for a bad code
		t.Fatalf("err = %v uploads=%d", err, len(h.uploads))
	}
}

// The printer reports an error code while it starts normally: the state is
// what decides.
func TestPrintProjectFile_ErrorCodeButPrinterStartsIsSuccess(t *testing.T) {
	const name = "pf-errstart"
	h, p, path := setupPrint(t, name, "01P00A123", "idle")
	noteReportedError(name, nil, "0")
	h.onPublish = func() {
		UpdatePrinterState(name, TelemetryData{Status: "PREPARE"})
		noteReportedError(name, []byte(`{"print":{"print_error":50348044}}`), "")
	}
	if err := PrintProjectFile(context.Background(), p, path, ProjectPrintOptions{AMSMapping: []int{-1, 0}}); err != nil {
		t.Fatalf("err = %v, want success", err)
	}
}

func TestWaitForStart_NewErrorWhileStillIdleFails(t *testing.T) {
	const name = "pf-wait-idle"
	t.Cleanup(func() { RemovePrinterState(name); forgetPrintFileState(name) })
	prevWait, prevPoll, prevGrace := startWait, startPoll, startErrGrace
	startWait, startPoll, startErrGrace = time.Second, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { startWait, startPoll, startErrGrace = prevWait, prevPoll, prevGrace })
	for _, status := range []string{"idle", "finished", "error"} {
		UpdatePrinterState(name, TelemetryData{Status: status})
		noteReportedError(name, nil, "0")
		noteReportedError(name, nil, "777")
		err := waitForStart(context.Background(), name, "")
		if err == nil || !strings.Contains(err.Error(), "error 777") {
			t.Fatalf("status %s: err = %v", status, err)
		}
	}
}

// A code that shows up one report before the state moves on is not a refusal.
func TestWaitForStart_PassingErrorThenStartSucceeds(t *testing.T) {
	const name = "pf-wait-passing"
	t.Cleanup(func() { RemovePrinterState(name); forgetPrintFileState(name) })
	prevWait, prevPoll, prevGrace := startWait, startPoll, startErrGrace
	startWait, startPoll, startErrGrace = time.Second, 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { startWait, startPoll, startErrGrace = prevWait, prevPoll, prevGrace })
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	noteReportedError(name, nil, "777")
	timer := time.AfterFunc(50*time.Millisecond, func() { UpdatePrinterState(name, TelemetryData{Status: "PREPARE"}) })
	defer timer.Stop()
	if err := waitForStart(context.Background(), name, ""); err != nil {
		t.Fatalf("err = %v, want a normal start", err)
	}
}

func TestWaitForStart_BusyStatesSucceedEvenWithNewError(t *testing.T) {
	const name = "pf-wait-busy"
	t.Cleanup(func() { RemovePrinterState(name); forgetPrintFileState(name) })
	noteReportedError(name, nil, "777")
	for _, status := range []string{"PREPARE", "SLICING", "printing"} {
		UpdatePrinterState(name, TelemetryData{Status: status})
		if err := waitForStart(context.Background(), name, ""); err != nil {
			t.Fatalf("status %s: %v", status, err)
		}
	}
}

func TestWaitForStart_TimesOutWithOldMessage(t *testing.T) {
	const name = "pf-wait-timeout"
	t.Cleanup(func() { RemovePrinterState(name) })
	prevWait, prevPoll := startWait, startPoll
	startWait, startPoll = 60*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { startWait, startPoll = prevWait, prevPoll })
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	if err := waitForStart(context.Background(), name, ""); !errors.Is(err, errPrintNoStart) {
		t.Fatalf("err = %v", err)
	}
}
