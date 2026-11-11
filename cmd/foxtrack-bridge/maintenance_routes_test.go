//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/history"
	"foxtrack-bridge/maintenance"
)

func maintTestConfig() *config.Config {
	return &config.Config{Printers: []config.Printer{
		{ID: "p1", Name: "Fox", Serial: "S1", LANCode: "12345678"},
		{ID: "p2", Name: "Voron", MoonrakerURL: "http://127.0.0.1:1"},
	}}
}

type maintList struct {
	Printers []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Tasks   []maintenance.View
		Presets []maintenance.Preset
	} `json:"printers"`
}

func getMaintenance(t *testing.T) maintList {
	t.Helper()
	rec := serveHandler(handleMaintenance, httptest.NewRequest("GET", "/api/maintenance", nil))
	var l maintList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || rec.Code != 200 {
		t.Fatalf("GET: %d %s %v", rec.Code, rec.Body, err)
	}
	return l
}

func postMaint(t *testing.T, body string) (*httptest.ResponseRecorder, []maintenance.View) {
	t.Helper()
	rec := serveHandler(handleMaintenance, jsonPost("/api/maintenance", body))
	var out struct {
		Tasks []maintenance.View `json:"tasks"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out.Tasks
}

func TestMaintenanceFlow(t *testing.T) {
	cfg := maintTestConfig()
	withBridgeConfig(t, cfg)

	l := getMaintenance(t)
	if len(l.Printers) != 2 || len(l.Printers[0].Presets) != 6 || len(l.Printers[1].Presets) != 4 || l.Printers[0].Tasks == nil {
		t.Fatalf("empty state wrong: %+v", l)
	}

	// The suggested set is added once; asking again adds nothing.
	if rec, added := postMaint(t, `{"printer_id":"p1","presets":true}`); rec.Code != 200 || len(added) != 6 {
		t.Fatalf("presets: %d, %d tasks", rec.Code, len(added))
	}
	if _, added := postMaint(t, `{"printer_id":"p1","presets":true}`); len(added) != 0 {
		t.Fatalf("presets added twice: %d", len(added))
	}
	l = getMaintenance(t)
	if len(l.Printers[0].Tasks) != 6 || len(l.Printers[0].Presets) != 0 || len(l.Printers[1].Tasks) != 0 {
		t.Fatalf("after presets: %+v", l)
	}

	// A custom task, then an edit that keeps its clock.
	rec, added := postMaint(t, `{"printer_id":"p2","name":" Oil the Z axis ","interval_hours":50,"last_done_at":1000}`)
	if rec.Code != 200 || len(added) != 1 || added[0].Name != "Oil the Z axis" || added[0].LastDoneAt != 1000 {
		t.Fatalf("custom add: %d %s", rec.Code, rec.Body)
	}
	id := added[0].ID
	if _, edited := postMaint(t, `{"id":"`+id+`","printer_id":"p2","name":"Oil the Z rod","interval_hours":75}`); len(edited) != 1 || edited[0].Name != "Oil the Z rod" || edited[0].IntervalHours != 75 || edited[0].LastDoneAt != 1000 {
		t.Fatalf("edit: %+v", edited)
	}
	if n := len(cfg.Maintenance.Tasks); n != 7 {
		t.Fatalf("an edit must not add a task, have %d", n)
	}

	// Done restarts the clock.
	before := time.Now().Unix()
	rec = serveHandler(handleMaintenanceTask, httptest.NewRequest("POST", "/api/maintenance/"+id+"/done", nil))
	var done maintenance.View
	json.Unmarshal(rec.Body.Bytes(), &done)
	if rec.Code != 200 || done.LastDoneAt < before || done.Hours != 0 || done.Status != maintenance.StatusOK {
		t.Fatalf("done: %d %s", rec.Code, rec.Body)
	}

	// Delete returns the task; posting it back restores it under the same id.
	rec = serveHandler(handleMaintenanceTask, httptest.NewRequest("DELETE", "/api/maintenance/"+id, nil))
	var gone config.MaintenanceTask
	json.Unmarshal(rec.Body.Bytes(), &gone)
	if rec.Code != 200 || gone.ID != id || gone.Name != "Oil the Z rod" || len(cfg.Maintenance.Tasks) != 6 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := serveHandler(handleMaintenanceTask, httptest.NewRequest("DELETE", "/api/maintenance/"+id, nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", rec.Code)
	}
	body, _ := json.Marshal(gone)
	if rec, back := postMaint(t, string(body)); rec.Code != 200 || len(back) != 1 || back[0].ID != id || back[0].LastDoneAt != gone.LastDoneAt || len(cfg.Maintenance.Tasks) != 7 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if rec := serveHandler(handleMaintenanceTask, httptest.NewRequest("POST", "/api/maintenance/nope/done", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("done on a missing task: %d, want 404", rec.Code)
	}
}

func TestMaintenanceRefusesBadInput(t *testing.T) {
	cfg := maintTestConfig()
	withBridgeConfig(t, cfg)
	long := strings.Repeat("x", 81)
	// An unknown printer is 404, as in the rest of the API.
	if rec, _ := postMaint(t, `{"printer_id":"zzz","name":"A","interval_hours":10}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown printer: status %d, want 404", rec.Code)
	}
	for name, body := range map[string]string{
		"no printer":    `{"name":"A","interval_hours":10}`,
		"empty name":    `{"printer_id":"p1","name":"  ","interval_hours":10}`,
		"long name":     `{"printer_id":"p1","name":"` + long + `","interval_hours":10}`,
		"zero interval": `{"printer_id":"p1","name":"A","interval_hours":0}`,
		"huge interval": `{"printer_id":"p1","name":"A","interval_hours":1e9}`,
		"bad id":        `{"id":"../x","printer_id":"p1","name":"A","interval_hours":5}`,
		"not json":      `{`,
	} {
		if rec, _ := postMaint(t, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if cfg.Maintenance != nil && len(cfg.Maintenance.Tasks) != 0 {
		t.Errorf("a refused request saved a task: %+v", cfg.Maintenance.Tasks)
	}
	if rec := serveHandler(handleMaintenanceTask, httptest.NewRequest("GET", "/api/maintenance/abc", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a task: %d, want 405", rec.Code)
	}
	if rec := serveHandler(handleMaintenanceTask, httptest.NewRequest("POST", "/api/maintenance/abc/other", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d, want 404", rec.Code)
	}
}

func TestMaintenanceAnnouncesOncePerDueTask(t *testing.T) {
	now := time.Now().Unix()
	cfg := maintTestConfig()
	cfg.Printers[0].PreviousNames = []string{"Old fox"}
	cfg.Maintenance = &config.Maintenance{Tasks: []config.MaintenanceTask{
		{ID: "t1", PrinterID: "p1", Name: "Lubricate the rods", IntervalHours: 100, LastDoneAt: now - 86400},
		{ID: "t2", PrinterID: "p1", Name: "Check belts", IntervalHours: 300, LastDoneAt: now - 86400},
	}}
	withBridgeConfig(t, cfg)
	old := maintAnnouncer
	maintAnnouncer = maintenance.NewAnnouncer(filepath.Join(config.ConfigDir(), "maintenance_state.json"))
	t.Cleanup(func() { maintAnnouncer = old })

	add := func(name string, hours int64, ended int64) {
		t.Helper()
		if err := history.Append(history.Record{PrinterName: name, EndTime: ended, Duration: hours * 3600, Result: "finished"}); err != nil {
			t.Fatal(err)
		}
	}
	add("Old fox", 60, now-7200) // filed under the old name
	if got := dueMaintenance(); len(got) != 0 {
		t.Fatalf("60 of 100 h is not due: %+v", got)
	}
	add("Fox", 44, now-3600)
	got := dueMaintenance()
	if len(got) != 1 || got[0].Kind != events.MaintenanceDue || got[0].PrinterID != "p1" || got[0].Printer != "Fox" ||
		got[0].Message != "Lubricate the rods: 104 h since it was last done (every 100 h)" {
		t.Fatalf("first announcement: %+v", got)
	}
	if got := dueMaintenance(); len(got) != 0 {
		t.Fatalf("announced twice: %+v", got)
	}

	// Done, then due again: announced again.
	serveHandler(handleMaintenanceTask, httptest.NewRequest("POST", "/api/maintenance/t1/done", nil))
	if got := dueMaintenance(); len(got) != 0 {
		t.Fatalf("just done, yet announced: %+v", got)
	}
	add("Fox", 101, time.Now().Unix()+5)
	if got := dueMaintenance(); len(got) != 1 || !strings.Contains(got[0].Message, "101 h") {
		t.Fatalf("second round: %+v", got)
	}
}
