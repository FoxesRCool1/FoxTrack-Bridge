package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func corruptBackups(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(historyPath() + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A history file that cannot be read (here: no read permission) may be fine.
// Append must leave it exactly as it is and report the error.
func TestAppend_UnreadableFileIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not block reads here")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := []byte(`[{"printer_name":"A","file_name":"a.gcode","start_time":1,"end_time":2,"result":"finished"}]`)
	if err := os.WriteFile(p, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })

	err := Append(Record{PrinterName: "A", FileName: "b.gcode", StartTime: 3, Result: "finished"})
	if err == nil {
		t.Fatal("Append on an unreadable file returned no error")
	}
	if errors.Is(err, ErrCorrupt) {
		t.Errorf("a permission error was treated as corruption: %v", err)
	}
	if b := corruptBackups(t); len(b) != 0 {
		t.Errorf("unreadable file was moved aside: %v", b)
	}
	os.Chmod(p, 0o600)
	got, rerr := os.ReadFile(p)
	if rerr != nil || !bytes.Equal(got, orig) {
		t.Errorf("unreadable file changed: %q (%v)", got, rerr)
	}
}

// A file that does not parse is real corruption: it is backed up, never
// deleted, and a new file is started.
func TestAppend_BrokenJSONIsBackedUp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`[{"printer_name":"A","start_time":`)
	if err := os.WriteFile(p, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load of broken JSON: err = %v, want ErrCorrupt", err)
	}
	if err := Append(Record{PrinterName: "B", StartTime: 9, Result: "finished"}); err != nil {
		t.Fatal(err)
	}
	b := corruptBackups(t)
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if got, _ := os.ReadFile(b[0]); !bytes.Equal(got, broken) {
		t.Errorf("backup = %q, want the original bytes", got)
	}
	recs, err := Load()
	if err != nil || len(recs) != 1 || recs[0].PrinterName != "B" {
		t.Errorf("new file = %+v (%v), want only the new record", recs, err)
	}
}

// A history.json written by an older Bridge (indented) loads, takes a new
// record, and keeps every old record exactly. The new file is compact JSON
// that the same reader, and so every older Bridge, still parses.
func TestAppend_OldIndentedFileKeepsEveryRecord(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	wh := 41.5
	old := []Record{
		{
			PrinterName: "X1C", FileName: "benchy.3mf", NozzleTemp: 220, BedTemp: 60,
			StartTime: 1700000000, EndTime: 1700003600, Duration: 3600, Result: "finished",
			TempSamples:  []TempSample{{T: 0, NozzleTemp: 25.5, BedTemp: 24}, {T: 60, NozzleTemp: 219.9, BedTemp: 59.8}},
			PercentAtEnd: 100,
			Filaments:    []FilamentUse{{Slot: 0, Material: "PLA", Color: "FF0000FF", TagUID: "A1B2", Grams: 12.25}, {Slot: 3, Material: "PETG"}},
			EnergyWh:     &wh,
			Note:         "first try, \"quoted\" <ok>",
		},
		{PrinterName: "Voron", FileName: "cube.gcode", StartTime: 1700010000, EndTime: 1700010500, Duration: 500, Result: "cancelled", PercentAtEnd: 12, FilamentMM: 1234.5},
		{PrinterName: "MK4", FileName: "clip.bgcode", StartTime: 1700020000, EndTime: 1700020100, Duration: 100, Result: "error"},
		{PrinterName: "Octo", FileName: "ü-ñ.gcode", StartTime: 1700030000, Result: "finished"},
	}
	data, err := json.MarshalIndent(old, "", "  ") // how every earlier version wrote it
	if err != nil {
		t.Fatal(err)
	}
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	next := Record{PrinterName: "HA", FileName: "n.gcode", StartTime: 1700040000, Result: "finished"}
	if err := Append(next); err != nil {
		t.Fatal(err)
	}
	if b := corruptBackups(t); len(b) != 0 {
		t.Fatalf("an old indented file was treated as corrupt: %v", b)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("\n")) {
		t.Error("history.json is still indented")
	}
	var plain []Record // the decoder every Bridge version uses
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatalf("new file does not parse: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, plain) {
		t.Error("Load and a plain decode disagree")
	}
	want := append(append([]Record{}, old...), next)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records after append differ:\n got %+v\nwant %+v", got, want)
	}

	// A note set afterwards keeps the rest of the record and the others too.
	if _, err := SetNote("Voron", 1700010000, "layer shift"); err != nil {
		t.Fatal(err)
	}
	got, _ = Load()
	want[1].Note = "layer shift"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records after SetNote differ:\n got %+v\nwant %+v", got, want)
	}
}

// SetNote on an unreadable file reports the error and writes nothing.
func TestSetNote_UnreadableFileIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not block reads here")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := []byte(`[{"printer_name":"A","start_time":1,"result":"finished"}]`)
	if err := os.WriteFile(p, orig, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	if _, err := SetNote("A", 1, "x"); err == nil {
		t.Fatal("SetNote on an unreadable file returned no error")
	}
	os.Chmod(p, 0o600)
	if got, _ := os.ReadFile(p); !bytes.Equal(got, orig) || len(corruptBackups(t)) != 0 {
		t.Errorf("unreadable file changed: %q", got)
	}
}
