package mqtt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/ftps"
)

const skipSliceInfo = `<config>
  <plate>
    <metadata key="index" value="1"/>
    <object identify_id="1065" name="Cube" skipped="false"/>
    <object identify_id="1066" name="Ball" skipped="true"/>
  </plate>
  <plate>
    <metadata key="index" value="2"/>
    <object identify_id="2000" name="Vase" skipped="false"/>
  </plate>
</config>`

const skipPlate1JSON = `{"bbox_all":[0,0,200,100],"bbox_objects":[{"name":"Cube","id":1065,"area":123.4,"bbox":[10,20,60,70]},{"name":"Ball","id":1066,"area":50,"bbox":[100,20,150,70]}]}`
const skipPlate2JSON = `{"bbox_objects":[{"name":"Vase","id":2000,"bbox":[0,0,50,50]}]}`

func skip3MF(t *testing.T) string {
	t.Helper()
	return make3MF(t, map[string]string{
		"Metadata/slice_info.config": skipSliceInfo,
		"Metadata/plate_1.json":      skipPlate1JSON,
		"Metadata/plate_2.json":      skipPlate2JSON,
		"Metadata/plate_1.gcode":     "G1",
		"Metadata/plate_2.gcode":     "G1",
	})
}

func TestRead3MFObjects(t *testing.T) {
	p := skip3MF(t)

	objs, err := read3MFObjects(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].ID != "1065" || objs[0].Name != "Cube" || objs[0].Skipped ||
		objs[1].ID != "1066" || objs[1].Name != "Ball" || !objs[1].Skipped ||
		!reflect.DeepEqual(objs[0].BBox, []float64{10, 20, 60, 70}) ||
		!reflect.DeepEqual(objs[1].BBox, []float64{100, 20, 150, 70}) {
		t.Fatalf("plate 1: %+v", objs)
	}

	// Plate unknown and the project has two sliced plates: refuse, do not guess.
	var unsup *SkipUnsupported
	if _, err = read3MFObjects(p, 0); !errors.As(err, &unsup) {
		t.Fatalf("plate 0 on two plates: err = %v, want *SkipUnsupported", err)
	}

	// Plate unknown and only one plate sliced: that plate is used.
	one := make3MF(t, map[string]string{
		"Metadata/slice_info.config": skipSliceInfo,
		"Metadata/plate_2.json":      skipPlate2JSON,
		"Metadata/plate_2.gcode":     "G1",
	})
	objs, err = read3MFObjects(one, 0)
	if err != nil || len(objs) != 1 || objs[0].ID != "2000" {
		t.Fatalf("one plate: %+v err=%v", objs, err)
	}

	// Non-numeric identify_id values are dropped.
	bad := make3MF(t, map[string]string{
		"Metadata/slice_info.config": `<config><plate><metadata key="index" value="1"/>` +
			`<object identify_id="abc" name="Bad"/><object identify_id="12" name="Good"/></plate></config>`,
		"Metadata/plate_1.gcode": "G1",
	})
	objs, err = read3MFObjects(bad, 0)
	if err != nil || len(objs) != 1 || objs[0].ID != "12" {
		t.Fatalf("non-numeric id: %+v err=%v", objs, err)
	}

	objs, err = read3MFObjects(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].ID != "2000" || objs[0].Name != "Vase" ||
		!reflect.DeepEqual(objs[0].BBox, []float64{0, 0, 50, 50}) {
		t.Fatalf("plate 2: %+v", objs)
	}

	// No slice_info: the objects come from the plate json.
	pj := make3MF(t, map[string]string{
		"Metadata/plate_1.json":  skipPlate1JSON,
		"Metadata/plate_1.gcode": "G1",
	})
	objs, err = read3MFObjects(pj, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].ID != "1065" || objs[0].Skipped || objs[1].ID != "1066" {
		t.Fatalf("from json: %+v", objs)
	}

	// Neither: unsupported.
	empty := make3MF(t, map[string]string{"Metadata/plate_1.gcode": "G1"})
	_, err = read3MFObjects(empty, 0)
	if !errors.As(err, &unsup) {
		t.Fatalf("err = %v, want *SkipUnsupported", err)
	}

	// Not a zip.
	notZip := filepath.Join(t.TempDir(), "x.3mf")
	if err := os.WriteFile(notZip, []byte("no zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := read3MFObjects(notZip, 0); !errors.Is(err, errNotA3MF) {
		t.Fatalf("err = %v, want errNotA3MF", err)
	}
}

func TestBuildSkipObjects(t *testing.T) {
	if got := string(buildSkipObjects("7", []int{1065, 1066})); got != `{"print":{"sequence_id":"7","command":"skip_objects","obj_list":[1065,1066]}}` {
		t.Fatalf("got %s", got)
	}
	if got := string(buildSkipObjects("7", []int{})); got != `{"print":{"sequence_id":"7","command":"skip_objects","obj_list":[]}}` {
		t.Fatalf("got %s", got)
	}
}

func TestPickSkipFile(t *testing.T) {
	files := []PrinterFile{
		{Name: "cache/Cube.gcode.3mf"},
		{Name: "Cube.3mf"},
		{Name: "foxtrack-print.gcode.3mf"},
		{Name: "cache/Foo.gcode.3mf"},
	}
	cases := []struct {
		name      string
		gcodeFile string
		subtask   string
		bridgeJob bool
		want      string
		ok        bool
	}{
		{"gcode file wins", "/sdcard/cache/Foo.gcode.3mf", "Cube", false, "cache/Foo.gcode.3mf", true},
		{"subtask gcode.3mf", "Metadata/plate_1.gcode", "Cube", false, "cache/Cube.gcode.3mf", true},
		{"bridge job", "Metadata/plate_1.gcode", "Cube", true, "foxtrack-print.gcode.3mf", true},
		{"no match", "Metadata/plate_1.gcode", "Nope", false, "", false},
	}
	for _, c := range cases {
		got, ok := pickSkipFile(files, c.gcodeFile, c.subtask, c.bridgeJob)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %q ok=%v, want %q ok=%v", c.name, got, ok, c.want, c.ok)
		}
	}
	// Same name in two folders: the newest wins.
	dup := []PrinterFile{
		{Name: "old/Cube.gcode.3mf", ModifiedAt: "2026-01-01T00:00:00Z"},
		{Name: "cache/Cube.gcode.3mf", ModifiedAt: "2026-03-01T00:00:00Z"},
		{Name: "sdcard/Cube.gcode.3mf", ModifiedAt: "2026-02-01T00:00:00Z"},
	}
	if got, ok := pickSkipFile(dup, "Metadata/plate_1.gcode", "Cube", false); !ok || got != "cache/Cube.gcode.3mf" {
		t.Errorf("newest: got %q ok=%v", got, ok)
	}
	// Without Cube.gcode.3mf the plain .3mf is used.
	if got, ok := pickSkipFile(files[1:], "Metadata/plate_1.gcode", "Cube", false); !ok || got != "Cube.3mf" {
		t.Errorf("subtask .3mf: got %q ok=%v", got, ok)
	}
}

func TestNoteSkipFields(t *testing.T) {
	const name = "skip-fields"
	t.Cleanup(func() {
		skipJobsMu.Lock()
		delete(skipJobs, name)
		skipJobsMu.Unlock()
	})
	job := jobFor(name)
	st := func() (string, map[string]bool, map[string]bool, string, bool) {
		job.mu.Lock()
		defer job.mu.Unlock()
		return job.gcodeFile, job.reported, job.sent, job.bridgeTask, job.idle
	}

	// s_obj [1,2] then [3] replaces.
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_1.gcode","s_obj":[1,2]}}`))
	if _, rep, _, _, idle := st(); idle || !rep["1"] || !rep["2"] || len(rep) != 2 {
		t.Fatalf("after [1,2]: %+v", rep)
	}
	noteSkipFields(name, []byte(`{"print":{"s_obj":[3]}}`))
	if _, rep, _, _, _ := st(); !rep["3"] || rep["1"] || len(rep) != 1 {
		t.Fatalf("after [3]: %+v", rep)
	}

	// Numeric strings work.
	noteSkipFields(name, []byte(`{"print":{"s_obj":["1065"]}}`))
	if _, rep, _, _, _ := st(); !rep["1065"] || len(rep) != 1 {
		t.Fatalf("numeric string: %+v", rep)
	}

	// FINISH resets; a following s_obj is ignored until RUNNING arrives, but
	// a gcode_file seen meanwhile is kept (reports are partial).
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	if gf, rep, _, _, idle := st(); !idle || gf != "" || len(rep) != 0 {
		t.Fatalf("after FINISH: idle=%v gf=%q rep=%+v", idle, gf, rep)
	}
	noteSkipFields(name, []byte(`{"print":{"gcode_file":"Metadata/plate_2.gcode","s_obj":[9]}}`))
	if gf, rep, _, _, idle := st(); !idle || gf != "Metadata/plate_2.gcode" || len(rep) != 0 {
		t.Fatalf("idle leftovers: idle=%v gf=%q rep=%+v", idle, gf, rep)
	}
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_2.gcode","s_obj":[9]}}`))
	if gf, rep, _, _, idle := st(); idle || gf != "Metadata/plate_2.gcode" || !rep["9"] {
		t.Fatalf("after RUNNING: idle=%v gf=%q rep=%+v", idle, gf, rep)
	}

	// gcode_file is kept across a message without it.
	noteSkipFields(name, []byte(`{"print":{"s_obj":[9,10]}}`))
	if gf, rep, _, _, _ := st(); gf != "Metadata/plate_2.gcode" || !rep["10"] {
		t.Fatalf("kept: gf=%q rep=%+v", gf, rep)
	}

	// A payload without the keys changes nothing.
	beforeGF, beforeRep, _, _, _ := st()
	noteSkipFields(name, []byte(`{"print":{"nozzle_temper":20}}`))
	afterGF, afterRep, _, _, _ := st()
	if beforeGF != afterGF || !reflect.DeepEqual(beforeRep, afterRep) {
		t.Fatalf("changed: %q/%+v -> %q/%+v", beforeGF, beforeRep, afterGF, afterRep)
	}

	// noteBridgeJob sets the task name.
	noteBridgeJob(name, "Cube")
	if _, _, _, task, _ := st(); task != "Cube" {
		t.Fatalf("bridgeTask = %q", task)
	}
}

func TestNoteCommandReply_SkipObjects(t *testing.T) {
	const name = "skip-reply"
	t.Cleanup(func() { forgetPrintFileState(name) })
	if noteCommandReply(name, []byte(`{"print":{"command":"skip_objects","result":"fail","reason":"busy"}}`)) != true {
		t.Fatal("skip_objects answer not recognised")
	}
	reason, refused := commandRefusal(name, "skip_objects")
	if !refused || reason != "busy" {
		t.Fatalf("refused=%v reason=%q", refused, reason)
	}
}

// skipHarness wires the seams the flow tests need.
type skipHarness struct {
	downloads int
	published []byte
	dlErrs    []error
	src       string // 3mf file copied to the download target
}

func setupSkip(t *testing.T, name string, status string) (*skipHarness, Printer) {
	t.Helper()
	h := &skipHarness{}
	prevList, prevDL, prevPub, prevConn := ftpsList, ftpsDownload, publishRequestFn, printerClientConnected
	t.Cleanup(func() {
		ftpsList, ftpsDownload, publishRequestFn, printerClientConnected = prevList, prevDL, prevPub, prevConn
		RemovePrinterState(name)
		skipJobsMu.Lock()
		delete(skipJobs, name)
		skipJobsMu.Unlock()
	})
	ftpsList = func(_ context.Context, _ ftps.Config, dir string) ([]ftps.Entry, error) {
		if dir == "/" {
			return []ftps.Entry{{Name: "Cube.gcode.3mf", Size: 10, Modified: time.Now()}}, nil
		}
		return nil, ftps.ErrNotFound
	}
	ftpsDownload = func(_ context.Context, _ ftps.Config, _, dst string, _ int64) error {
		h.downloads++
		if len(h.dlErrs) > 0 {
			err := h.dlErrs[0]
			h.dlErrs = h.dlErrs[1:]
			return err
		}
		b, err := os.ReadFile(h.src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
	publishRequestFn = func(_, _ string, payload []byte) error {
		h.published = payload
		return nil
	}
	printerClientConnected = func(string) bool { return true }
	UpdatePrinterState(name, TelemetryData{Status: status, FileName: "Cube"})
	return h, Printer{Name: name, IP: "192.0.2.1", Serial: "01P00A123", LANCode: "12345678"}
}

func TestListSkipObjects_IdlePrinter(t *testing.T) {
	h, p := setupSkip(t, "skip-idle", "idle")
	_, err := ListSkipObjects(context.Background(), p)
	var unsup *SkipUnsupported
	if !errors.As(err, &unsup) {
		t.Fatalf("err = %v, want *SkipUnsupported", err)
	}
	if h.downloads != 0 || h.published != nil {
		t.Fatalf("seams touched: downloads=%d published=%v", h.downloads, h.published != nil)
	}
}

func TestSkipObjects_IdleAndCloud(t *testing.T) {
	h, p := setupSkip(t, "skip-idle2", "idle")
	_, err := SkipObjects(context.Background(), p, []string{"1065"})
	var unsup *SkipUnsupported
	if !errors.As(err, &unsup) {
		t.Fatalf("err = %v, want *SkipUnsupported", err)
	}
	if h.downloads != 0 || h.published != nil {
		t.Fatalf("seams touched: downloads=%d published=%v", h.downloads, h.published != nil)
	}

	h, p = setupSkip(t, "skip-cloud", "printing")
	p.Cloud = true
	_, err = ListSkipObjects(context.Background(), p)
	if !errors.As(err, &unsup) {
		t.Fatalf("cloud: err = %v, want *SkipUnsupported", err)
	}
	if h.downloads != 0 || h.published != nil {
		t.Fatalf("cloud: seams touched")
	}
}

func TestSkipFlow(t *testing.T) {
	const name = "skip-flow"
	prevWait := skipConfirmWait
	skipConfirmWait = 50 * time.Millisecond
	t.Cleanup(func() { skipConfirmWait = prevWait })

	h, p := setupSkip(t, name, "printing")
	// Nothing is skipped in the file itself here (skip3MF skips Ball).
	h.src = make3MF(t, map[string]string{
		"Metadata/slice_info.config": strings.Replace(skipSliceInfo, `skipped="true"`, `skipped="false"`, 1),
		"Metadata/plate_1.json":      skipPlate1JSON,
		"Metadata/plate_1.gcode":     "G1",
	})
	noteSkipFields(name, []byte(`{"print":{"gcode_file":"Metadata/plate_1.gcode","gcode_state":"RUNNING"}}`))

	list, err := ListSkipObjects(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Objects) != 2 || list.Objects[0].ID != "1065" || list.Objects[1].ID != "1066" ||
		!reflect.DeepEqual(list.Objects[0].BBox, []float64{10, 20, 60, 70}) {
		t.Fatalf("list = %+v", list.Objects)
	}
	if list.Objects[0].Skipped || list.Objects[1].Skipped {
		t.Fatal("nothing must be skipped yet")
	}

	// A second call does not download again.
	if _, err := ListSkipObjects(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if h.downloads != 1 {
		t.Fatalf("downloads = %d, want 1", h.downloads)
	}

	// The printer confirms by reporting the object in s_obj.
	confirm := publishRequestFn
	publishRequestFn = func(n, s string, payload []byte) error {
		noteSkipFields(name, []byte(`{"print":{"s_obj":[1065]}}`))
		return confirm(n, s, payload)
	}
	res, err := SkipObjects(context.Background(), p, []string{"1065"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h.published), "skip_objects") || !strings.Contains(string(h.published), "[1065]") {
		t.Fatalf("payload = %s", h.published)
	}
	byID := map[string]SkipObject{}
	for _, o := range res.Objects {
		byID[o.ID] = o
	}
	if !byID["1065"].Skipped || byID["1066"].Skipped {
		t.Fatalf("result = %+v", res.Objects)
	}

	// Skipping 1065 again: already skipped.
	_, err = SkipObjects(context.Background(), p, []string{"1065"})
	if err == nil || !strings.Contains(err.Error(), "already skipped") {
		t.Fatalf("err = %v, want already skipped", err)
	}

	// An unknown id.
	_, err = SkipObjects(context.Background(), p, []string{"9999"})
	if err == nil || !strings.Contains(err.Error(), "not on the printer's plate") {
		t.Fatalf("err = %v, want not on the plate", err)
	}

	// A refusal: the printer answers fail with a reason.
	setRefusal := func() {
		printErrMu.Lock()
		commandFail[name+"/skip_objects"] = "busy"
		printErrMu.Unlock()
	}
	pub := confirm
	publishRequestFn = func(n, s string, payload []byte) error {
		setRefusal()
		return pub(n, s, payload)
	}
	_, err = SkipObjects(context.Background(), p, []string{"1066"})
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want a refusal mentioning busy", err)
	}
	res, err = ListSkipObjects(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]SkipObject{}
	for _, o := range res.Objects {
		byID[o.ID] = o
	}
	if byID["1066"].Skipped {
		t.Fatal("1066 must stay not skipped after the refusal")
	}
}

func TestSkipObjects_NoConfirm(t *testing.T) {
	const name = "skip-noconfirm"
	prevWait := skipConfirmWait
	skipConfirmWait = 50 * time.Millisecond
	t.Cleanup(func() { skipConfirmWait = prevWait })

	h, p := setupSkip(t, name, "printing")
	h.src = make3MF(t, map[string]string{
		"Metadata/slice_info.config": strings.Replace(skipSliceInfo, `skipped="true"`, `skipped="false"`, 1),
		"Metadata/plate_1.json":      skipPlate1JSON,
		"Metadata/plate_1.gcode":     "G1",
	})
	noteSkipFields(name, []byte(`{"print":{"gcode_file":"Metadata/plate_1.gcode","gcode_state":"RUNNING"}}`))

	// The printer never reports the object: an error, and the object is not shown as skipped.
	_, err := SkipObjects(context.Background(), p, []string{"1065"})
	if err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("err = %v, want did not confirm", err)
	}
	list, err := ListSkipObjects(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Objects {
		if o.Skipped {
			t.Fatalf("%s must stay not skipped: %+v", o.ID, list.Objects)
		}
	}
}

// A multi-plate project whose gcode_file arrived while the printer still said
// FINISH must still use that plate, not the lowest one.
func TestSkipObjects_GcodeFileBeforeState(t *testing.T) {
	const name = "skip-early-gcode"
	h, p := setupSkip(t, name, "printing")
	h.src = skip3MF(t)
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_file":"Metadata/plate_2.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING"}}`))

	list, err := ListSkipObjects(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Objects) != 1 || list.Objects[0].ID != "2000" {
		t.Fatalf("list = %+v, want plate 2", list.Objects)
	}

	// Without any gcode_file the plate is unknown: refuse on a multi-plate file.
	const name2 = "skip-no-gcode"
	h2, p2 := setupSkip(t, name2, "printing")
	h2.src = skip3MF(t)
	noteSkipFields(name2, []byte(`{"print":{"gcode_state":"RUNNING"}}`))
	_, err = ListSkipObjects(context.Background(), p2)
	var unsup *SkipUnsupported
	if !errors.As(err, &unsup) {
		t.Fatalf("err = %v, want *SkipUnsupported", err)
	}
}

func TestNoteSkipFields_IdleKeepsOnlyANewFile(t *testing.T) {
	const name = "skip-fields-done"
	t.Cleanup(func() {
		skipJobsMu.Lock()
		delete(skipJobs, name)
		skipJobsMu.Unlock()
	})
	job := jobFor(name)
	file := func() string {
		job.mu.Lock()
		defer job.mu.Unlock()
		return job.gcodeFile
	}
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"a/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH","gcode_file":"a/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH","gcode_file":"a/plate_1.gcode"}}`))
	if f := file(); f != "" {
		t.Fatalf("the ended print's file was kept: %q", f)
	}
	noteSkipFields(name, []byte(`{"print":{"gcode_file":"b/plate_2.gcode"}}`))
	if f := file(); f != "b/plate_2.gcode" {
		t.Fatalf("next print's file = %q", f)
	}
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING"}}`))
	if f := file(); f != "b/plate_2.gcode" {
		t.Fatalf("after RUNNING = %q", f)
	}
}
