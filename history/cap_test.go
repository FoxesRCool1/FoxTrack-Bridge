package history

import (
	"os"
	"testing"
)

// The file is rewritten whole on every append, so it keeps the newest records
// and the newest temperature samples of each.
func TestAppend_CapsRecordsAndSamples(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Seed the file with MaxRecords old records in one write, as a long-running
	// Bridge would have, instead of 5000 appends.
	old := make([]Record, MaxRecords)
	for i := range old {
		old[i] = Record{PrinterName: "Fox", StartTime: int64(i), EndTime: int64(i) + 1, Result: "finished"}
	}
	if err := save(old); err != nil {
		t.Fatal(err)
	}

	samples := make([]TempSample, MaxTempSamples+10)
	for i := range samples {
		samples[i] = TempSample{T: int64(i * 60), NozzleTemp: float64(i)}
	}
	if err := Append(Record{PrinterName: "Fox", StartTime: 99999, EndTime: 100000, Result: "finished", TempSamples: samples}); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxRecords {
		t.Fatalf("kept %d records, want %d", len(got), MaxRecords)
	}
	if got[0].StartTime != 1 {
		t.Errorf("the oldest record should be dropped; first start = %d, want 1", got[0].StartTime)
	}
	last := got[len(got)-1]
	if last.StartTime != 99999 {
		t.Fatalf("the new record must be last, got start %d", last.StartTime)
	}
	if len(last.TempSamples) != MaxTempSamples {
		t.Fatalf("kept %d samples, want %d", len(last.TempSamples), MaxTempSamples)
	}
	if last.TempSamples[0].NozzleTemp != 10 || last.TempSamples[MaxTempSamples-1].NozzleTemp != float64(MaxTempSamples+9) {
		t.Errorf("the oldest samples should be the ones dropped: first=%v last=%v",
			last.TempSamples[0].NozzleTemp, last.TempSamples[MaxTempSamples-1].NozzleTemp)
	}
}

type testSession struct {
	FileName  string `json:"file_name"`
	StartTime int64  `json:"start_time"`
}

// A saved session comes back whole, a second printer's does not disturb it, and
// deleting leaves no file behind.
func TestSessions_RoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := SaveSession("a", testSession{"cube.gcode", 1000}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSession("b", testSession{"vase.gcode", 2000}); err != nil {
		t.Fatal(err)
	}
	var got testSession
	if !LoadSession("a", &got) || got != (testSession{"cube.gcode", 1000}) {
		t.Fatalf("session a = %+v", got)
	}
	if LoadSession("missing", &got) {
		t.Error("a printer with no session must report none")
	}

	if err := DeleteSession("a"); err != nil {
		t.Fatal(err)
	}
	if LoadSession("a", &got) {
		t.Error("a deleted session came back")
	}
	if !LoadSession("b", &got) || got.StartTime != 2000 {
		t.Errorf("deleting a must leave b: %+v", got)
	}
	if err := DeleteSession("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionsPath()); !os.IsNotExist(err) {
		t.Errorf("the file should be gone with no sessions left: %v", err)
	}
	if err := DeleteSession("b"); err != nil {
		t.Errorf("deleting nothing is not an error: %v", err)
	}
}

func TestResumable(t *testing.T) {
	const now = 100000
	cases := []struct {
		name string
		ok   bool
		got  bool
	}{
		{"same file, seen a minute ago, further along", true, Resumable("a.gcode", now-60, 40, "a.gcode", 42, now)},
		{"another file", false, Resumable("a.gcode", now-60, 40, "b.gcode", 42, now)},
		{"no file name yet", false, Resumable("", now-60, 40, "", 42, now)},
		{"unseen for too long", false, Resumable("a.gcode", now-RestoreMaxGap-1, 40, "a.gcode", 42, now)},
		{"progress went back: a new print of the same file", false, Resumable("a.gcode", now-60, 95, "a.gcode", 3, now)},
	}
	for _, c := range cases {
		if c.got != c.ok {
			t.Errorf("%s: Resumable = %v, want %v", c.name, c.got, c.ok)
		}
	}
}
