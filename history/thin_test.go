package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func minuteSamples(n int) []TempSample {
	s := make([]TempSample, n)
	for i := range s {
		s[i] = TempSample{T: int64(i * 60), NozzleTemp: 200 + float64(i%40)/4, BedTemp: 60 + float64(i%8)/8}
	}
	return s
}

func TestThinSamples(t *testing.T) {
	for _, n := range []int{0, 1, 2, 299, 300, 301, 450, 1440, 5000} {
		in := minuteSamples(n)
		got := thinSamples(slices.Clone(in), MaxStoredTempSamples)
		want := min(n, MaxStoredTempSamples)
		if len(got) != want {
			t.Fatalf("n=%d: kept %d, want %d", n, len(got), want)
		}
		if n == 0 {
			continue
		}
		if got[0] != in[0] || got[len(got)-1] != in[n-1] {
			t.Fatalf("n=%d: first or last sample lost", n)
		}
		for i := 1; i < len(got); i++ {
			if got[i].T <= got[i-1].T {
				t.Fatalf("n=%d: sample %d at t=%d is not after t=%d", n, i, got[i].T, got[i-1].T)
			}
		}
		if n <= MaxStoredTempSamples && !reflect.DeepEqual(got, in) {
			t.Fatalf("n=%d: samples under the cap must stay as they are", n)
		}
	}
}

// A new day-long print is stored with MaxStoredTempSamples samples spread over
// the whole print.
func TestAppend_ThinsNewRecordSamples(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := minuteSamples(1440)
	if err := Append(Record{PrinterName: "Fox", FileName: "long.gcode", StartTime: 1000, EndTime: 1000 + 1440*60, Result: "finished", TempSamples: in}); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || len(got) != 1 {
		t.Fatalf("Load = %d records, %v", len(got), err)
	}
	s := got[0].TempSamples
	if len(s) != MaxStoredTempSamples {
		t.Fatalf("stored %d samples, want %d", len(s), MaxStoredTempSamples)
	}
	if s[0] != in[0] || s[len(s)-1] != in[len(in)-1] {
		t.Fatalf("first or last sample lost: %+v .. %+v", s[0], s[len(s)-1])
	}
	for i := 1; i < len(s); i++ {
		if s[i].T <= s[i-1].T {
			t.Fatalf("sample %d at t=%d is not after t=%d", i, s[i].T, s[i-1].T)
		}
		if !slices.Contains(in, s[i]) {
			t.Fatalf("sample %d is not one of the recorded ones: %+v", i, s[i])
		}
	}
}

func TestAppend_KeepsShortSampleListAsIs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	in := minuteSamples(250)
	if err := Append(Record{PrinterName: "Fox", StartTime: 1, EndTime: 2, Result: "finished", TempSamples: in}); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || len(got) != 1 {
		t.Fatalf("Load = %d records, %v", len(got), err)
	}
	if !reflect.DeepEqual(got[0].TempSamples, in) {
		t.Fatal("250 samples must be stored unchanged")
	}
}

// oldFixture is a history.json as an older Bridge wrote it: indented, several
// records, one of them with a full day of samples and a note. It is built from
// plain maps, not Record, so it does not follow later changes to Record.
func oldFixture() []map[string]any {
	day := make([]map[string]any, 1440)
	for i := range day {
		day[i] = map[string]any{"t": i * 60, "nozzle_temp": 219.7 + float64(i%5)/10, "bed_temp": 60.25}
	}
	short := make([]map[string]any, 30)
	for i := range short {
		short[i] = map[string]any{"t": i * 60, "nozzle_temp": 250, "bed_temp": 100}
	}
	base := func(name, file string, start int64, result string) map[string]any {
		return map[string]any{
			"printer_name": name, "file_name": file,
			"nozzle_temp": 220, "bed_temp": 60,
			"start_time": start, "end_time": start + 3600, "duration": 3600,
			"result": result,
		}
	}
	a := base("Fox", "benchy.gcode", 1700000000, "finished")
	b := base("Fox", "day-long.gcode", 1700100000, "finished")
	b["end_time"], b["duration"] = 1700100000+86400, 86400
	b["temp_samples"] = day
	b["note"] = "Glue stick, brim 5 mm"
	b["percent_at_end"] = 100
	b["filaments"] = []map[string]any{{"slot": 0, "material": "PLA", "color": "FF6A00FF", "grams": 412.5}}
	b["energy_wh"] = 1834.2
	c := base("Klippy", "vase.gcode", 1700200000, "cancelled")
	c["temp_samples"] = short
	c["filament_mm"] = 1234.5
	c["note"] = "Stopped: layer shift"
	d := base("Klippy", "clip.gcode", 1700300000, "error")
	return []map[string]any{a, b, c, d}
}

// Records already in history.json are written back exactly, however many
// samples they hold: an Append and a SetNote never thin them.
func TestOldRecordsKeepEverySample(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(oldFixture(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := Load()
	if err != nil || len(before) != 4 {
		t.Fatalf("the old file must load: %d records, %v", len(before), err)
	}
	if len(before[1].TempSamples) != 1440 {
		t.Fatalf("fixture day-long record has %d samples", len(before[1].TempSamples))
	}
	var beforeRaw []any
	if err := json.Unmarshal(raw, &beforeRaw); err != nil {
		t.Fatal(err)
	}

	if err := Append(Record{PrinterName: "Fox", FileName: "new.gcode", StartTime: 1700400000, EndTime: 1700486400, Result: "finished", TempSamples: minuteSamples(1440)}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetNote("Fox", 1700400000, "New print"); err != nil {
		t.Fatal(err)
	}
	// A note on the old day-long record rewrites it too.
	if _, err := SetNote("Fox", 1700100000, "Glue stick, brim 8 mm"); err != nil {
		t.Fatal(err)
	}

	after, err := Load()
	if err != nil || len(after) != 5 {
		t.Fatalf("after: %d records, %v", len(after), err)
	}
	want := slices.Clone(before)
	want[1].Note = "Glue stick, brim 8 mm"
	if !reflect.DeepEqual(after[:4], want) {
		t.Fatal("an old record changed")
	}
	if len(after[1].TempSamples) != 1440 {
		t.Fatalf("the old record kept %d samples, want all 1440", len(after[1].TempSamples))
	}
	if len(after[4].TempSamples) != MaxStoredTempSamples || after[4].Note != "New print" {
		t.Fatalf("new record: %d samples, note %q", len(after[4].TempSamples), after[4].Note)
	}

	// The same check on the file itself, value for value.
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var afterRaw []any
	if err := json.Unmarshal(data, &afterRaw); err != nil {
		t.Fatal(err)
	}
	beforeRaw[1].(map[string]any)["note"] = "Glue stick, brim 8 mm"
	if len(afterRaw) != 5 || !reflect.DeepEqual(afterRaw[:4], beforeRaw) {
		t.Fatal("an old record's JSON changed value")
	}
}
