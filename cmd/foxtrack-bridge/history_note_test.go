//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

func TestHistoryNoteRoute(t *testing.T) {
	cfg := &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Fox"}}}
	withBridgeConfig(t, cfg)
	if err := history.Append(history.Record{PrinterName: "Fox", FileName: "a.gcode", StartTime: 100, EndTime: 200, Result: "finished"}); err != nil {
		t.Fatal(err)
	}

	rec := serveHandler(handleHistoryNote, jsonPost("/api/history/note", `{"printer_name":"Fox","start_time":100,"note":"  nice print "}`))
	if rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	var ok struct {
		OK   bool   `json:"ok"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil || !ok.OK || ok.Note != "nice print" {
		t.Fatalf("body = %s (err %v), want ok with trimmed note", rec.Body, err)
	}

	for name, body := range map[string]string{
		"bad json":       `nope`,
		"no printer":     `{"printer_name":"","start_time":100,"note":"x"}`,
		"zero start":     `{"printer_name":"Fox","start_time":0,"note":"x"}`,
		"negative start": `{"printer_name":"Fox","start_time":-5,"note":"x"}`,
	} {
		if rec := serveHandler(handleHistoryNote, jsonPost("/api/history/note", body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}

	rec = serveHandler(handleHistoryNote, jsonPost("/api/history/note", `{"printer_name":"Fox","start_time":999,"note":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown record: status %d, want 404", rec.Code)
	}
	rec = serveHandler(handleHistoryNote, jsonPost("/api/history/note", `{"printer_name":"Other","start_time":100,"note":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown printer: status %d, want 404", rec.Code)
	}
}
