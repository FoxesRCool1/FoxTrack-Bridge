package history

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foxtrack-bridge/internal/testhome"
)

func TestMain(m *testing.M) { testhome.Main(m) }

func TestSetNote(t *testing.T) {
	isolate := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", isolate)

	records := []Record{
		{PrinterName: "Fox", FileName: "a.gcode", StartTime: 100, EndTime: 200, Result: "finished"},
		{PrinterName: "Fox", FileName: "b.gcode", StartTime: 300, EndTime: 400, Result: "cancelled"},
	}
	for _, r := range records {
		if err := Append(r); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := SetNote("Fox", 300, "second print"); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d records, want 2", len(got))
	}
	if got[0].Note != "" {
		t.Errorf("first record got note %q, want none", got[0].Note)
	}
	if got[1].Note != "second print" {
		t.Errorf("second record note = %q, want %q", got[1].Note, "second print")
	}
}

func TestSetNoteNotFound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Append(Record{PrinterName: "Fox", StartTime: 100, EndTime: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetNote("Fox", 999, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetNote unknown start time = %v, want ErrNotFound", err)
	}
	if _, err := SetNote("Other", 100, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetNote unknown printer = %v, want ErrNotFound", err)
	}
}

func TestSetNoteCutsLongNote(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Append(Record{PrinterName: "Fox", StartTime: 100, EndTime: 200}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 600)
	if _, err := SetNote("Fox", 100, long); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Note) != 500 || got[0].Note != strings.Repeat("a", 500) {
		t.Fatalf("note length = %d, want 500", len(got[0].Note))
	}
}

func TestSetNoteTrimsWhitespace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Append(Record{PrinterName: "Fox", StartTime: 100, EndTime: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetNote("Fox", 100, "  padded  "); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Note != "padded" {
		t.Fatalf("note = %q, want %q", got[0].Note, "padded")
	}
}

// A history.json written before notes existed must load unchanged.
func TestLoadOldFileWithoutNotes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	old := `[
  {
    "printer_name": "Fox",
    "file_name": "a.gcode",
    "nozzle_temp": 210,
    "bed_temp": 60,
    "start_time": 100,
    "end_time": 200,
    "duration": 100,
    "result": "finished"
  }
]`
	if err := os.MkdirAll(filepath.Join(dir, "FoxTrack-Bridge"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "FoxTrack-Bridge", "history.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PrinterName != "Fox" || got[0].Note != "" {
		t.Fatalf("loaded %+v, want one record with no note", got)
	}
	// And a note can still be added to it.
	if _, err := SetNote("Fox", 100, "hello"); err != nil {
		t.Fatal(err)
	}
	got, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Note != "hello" {
		t.Fatalf("note = %q, want hello", got[0].Note)
	}
}
