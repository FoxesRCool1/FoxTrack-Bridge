package maintenance

import (
	"os"
	"path/filepath"
	"testing"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

func TestPresets(t *testing.T) {
	b := Presets(config.KindBambu)
	if len(b) != 6 {
		t.Fatalf("bambu presets: got %d, want 6", len(b))
	}
	if b[0].Name != "Clean and lubricate the rods" || b[0].IntervalHours != 100 {
		t.Errorf("first bambu preset: got %+v", b[0])
	}
	if b[5].Name != "Replace the carbon or HEPA filter" || b[5].IntervalHours != 500 {
		t.Errorf("last bambu preset: got %+v", b[5])
	}

	k := Presets("klipper")
	if len(k) != 4 {
		t.Fatalf("klipper presets: got %d, want 4", len(k))
	}

	// A new slice each call: mutating one must not affect the next.
	first := Presets("klipper")
	first[0].Name = "mutated"
	second := Presets("klipper")
	if second[0].Name != "Lubricate the rails" {
		t.Errorf("second call affected by first mutation: got %q", second[0].Name)
	}
}

func TestStatusFor(t *testing.T) {
	cases := []struct {
		hours, interval float64
		status          string
		percent         int
	}{
		{89.9, 100, StatusOK, 89},
		{90, 100, StatusSoon, 90},
		{99.9, 100, StatusSoon, 99},
		{100, 100, StatusDue, 100},
		{104, 100, StatusDue, 104},
		{5, 0, StatusOK, 0},
	}
	for _, c := range cases {
		status, percent := StatusFor(c.hours, c.interval)
		if status != c.status || percent != c.percent {
			t.Errorf("StatusFor(%v, %v) = (%q, %d), want (%q, %d)", c.hours, c.interval, status, percent, c.status, c.percent)
		}
	}
}

func TestHours(t *testing.T) {
	recs := []history.Record{
		{PrinterName: "Fox", EndTime: 1000, Duration: 3600},
		{PrinterName: "Fox", EndTime: 2000, Duration: 7200},
		{PrinterName: "Old", EndTime: 2500, Duration: 1800},
		{PrinterName: "Other", EndTime: 2500, Duration: 3600},
	}
	names := []string{"Fox", "Old"}
	if got := Hours(recs, names, 1000); got != 2.5 {
		t.Errorf("Hours since 1000: got %v, want 2.5", got)
	}
	if got := Hours(recs, names, 0); got != 3.5 {
		t.Errorf("Hours since 0: got %v, want 3.5", got)
	}
}

func TestEvaluate(t *testing.T) {
	printers := []config.Printer{{ID: "p1", Name: "Fox", PreviousNames: []string{"Old"}}}
	tasks := []config.MaintenanceTask{
		{ID: "t1", PrinterID: "p1", Name: "Lubricate the rods", IntervalHours: 3, LastDoneAt: 1000},
		{ID: "t2", PrinterID: "gone", Name: "Ghost task", IntervalHours: 10},
	}
	recs := []history.Record{
		{PrinterName: "Fox", EndTime: 2000, Duration: 7200},
		{PrinterName: "Old", EndTime: 2500, Duration: 1800},
	}

	views := Evaluate(tasks, printers, recs)
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	v := views[0]
	if v.PrinterName != "Fox" || v.Hours != 2.5 || v.Percent != 83 || v.Status != StatusOK {
		t.Errorf("view: got %+v", v)
	}

	tasks[0].IntervalHours = 2.5
	views = Evaluate(tasks, printers, recs)
	if views[0].Status != StatusDue {
		t.Errorf("interval 2.5: got status %q, want %q", views[0].Status, StatusDue)
	}
}

func TestMessage(t *testing.T) {
	v := View{
		MaintenanceTask: config.MaintenanceTask{Name: "Lubricate the rods", IntervalHours: 100},
		Hours:           104,
	}
	want := "Lubricate the rods: 104 h since it was last done (every 100 h)"
	if got := v.Message(); got != want {
		t.Errorf("Message: got %q, want %q", got, want)
	}
}

func TestAnnouncer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := NewAnnouncer(path)

	due := View{MaintenanceTask: config.MaintenanceTask{ID: "t1", LastDoneAt: 1000}, Status: StatusDue}
	soon := View{MaintenanceTask: config.MaintenanceTask{ID: "t2", LastDoneAt: 1000}, Status: StatusSoon}

	got := a.NewlyDue([]View{due, soon})
	if len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("first call: got %+v, want only t1", got)
	}
	if got := a.NewlyDue([]View{due, soon}); len(got) != 0 {
		t.Fatalf("second call: got %+v, want none", got)
	}

	// Marked done and due again: a different LastDoneAt is announced again.
	due2 := View{MaintenanceTask: config.MaintenanceTask{ID: "t1", LastDoneAt: 2000}, Status: StatusDue}
	if got := a.NewlyDue([]View{due2}); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("redone task: got %+v, want t1", got)
	}

	// A task that is due, then ok (forgotten), then due again is announced again.
	due3 := View{MaintenanceTask: config.MaintenanceTask{ID: "t3", LastDoneAt: 1000}, Status: StatusDue}
	if got := a.NewlyDue([]View{due3}); len(got) != 1 || got[0].ID != "t3" {
		t.Fatalf("t3 first due: got %+v, want t3", got)
	}
	ok := View{MaintenanceTask: config.MaintenanceTask{ID: "t3", LastDoneAt: 1000}, Status: StatusOK}
	if got := a.NewlyDue([]View{ok}); len(got) != 0 {
		t.Fatalf("t3 ok: got %+v, want none", got)
	}
	if got := a.NewlyDue([]View{due3}); len(got) != 1 || got[0].ID != "t3" {
		t.Fatalf("t3 due again: got %+v, want t3", got)
	}

	// A new announcer on the same file does not re-announce.
	b := NewAnnouncer(path)
	if got := b.NewlyDue([]View{due2}); len(got) != 0 {
		t.Fatalf("fresh announcer: got %+v, want none", got)
	}
}

func TestAnnouncerCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	a := NewAnnouncer(path)
	due := View{MaintenanceTask: config.MaintenanceTask{ID: "t1", LastDoneAt: 1000}, Status: StatusDue}
	if got := a.NewlyDue([]View{due}); len(got) != 1 {
		t.Fatalf("corrupt file: got %+v, want t1", got)
	}
}
