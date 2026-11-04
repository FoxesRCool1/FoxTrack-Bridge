//go:build headless

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	"foxtrack-bridge/spoolman"
)

// fakeSpoolman answers like a Spoolman server and records the /use calls.
type fakeSpoolman struct {
	*httptest.Server
	mu   sync.Mutex
	uses []string // "<id> <body>"
}

func newFakeSpoolman(t *testing.T) *fakeSpoolman {
	f := &fakeSpoolman{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.22.1"}`))
	})
	mux.HandleFunc("/api/v1/spool", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"remaining_weight":800,"filament":{"name":"Black","material":"PLA","color_hex":"111111"},"extra":{"tag":"\"04AABBCC\""}},
			{"id":7,"remaining_weight":500,"filament":{"name":"Red","material":"PETG","color_hex":"FF0000"}}]`))
	})
	mux.HandleFunc("PUT /api/v1/spool/{id}/use", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.uses = append(f.uses, r.PathValue("id")+" "+strings.TrimSpace(string(b)))
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSpoolman) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uses...)
}

func spoolmanTestConfig(url string) *config.Config {
	return &config.Config{
		Printers: []config.Printer{
			{ID: "p1", Name: "Fox", Serial: "S1", LANCode: "12345678"},
			{ID: "p2", Name: "Voron", MoonrakerURL: "http://127.0.0.1:1"},
		},
		Spoolman: &config.Spoolman{Enabled: true, URL: url, Slots: map[string]int{"p1/0": 7, "p2/0": 1}},
	}
}

func TestSpoolmanSettingsValidation(t *testing.T) {
	f := newFakeSpoolman(t)
	cfg := &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Fox", Serial: "S1"}}}
	withBridgeConfig(t, cfg)
	post := func(body string) *httptest.ResponseRecorder {
		return serveHandler(handleSpoolman, jsonPost("/api/spoolman", body))
	}

	// Turning it on needs an address, and a refusal changes nothing.
	if rec := post(`{"enabled":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without a URL: %d, want 400", rec.Code)
	}
	if cfg.Spoolman != nil && (cfg.Spoolman.Enabled || cfg.Spoolman.URL != "") {
		t.Fatalf("refused request changed the config: %+v", cfg.Spoolman)
	}
	for name, body := range map[string]string{
		"password in url": `{"url":"http://user:pw@host:7912"}`,
		"wrong scheme":    `{"url":"ftp://host"}`,
		"bad slot key":    `{"slots":{"nonsense":3}}`,
		"slot too high":   `{"slots":{"p1/255":3}}`,
		"negative spool":  `{"slots":{"p1/0":-2}}`,
		"unknown printer": `{"slots":{"zzz/0":3}}`,
		"not json":        `[`,
	} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	// An address and a bad slot together: nothing is applied.
	if rec := post(`{"url":"` + f.URL + `","slots":{"zzz/0":3}}`); rec.Code != http.StatusBadRequest || (cfg.Spoolman != nil && cfg.Spoolman.URL != "") {
		t.Fatalf("a refused request kept its address: %d %+v", rec.Code, cfg.Spoolman)
	}

	if rec := post(`{"enabled":true,"url":"` + f.URL + `/","slots":{"p1/0":7,"p1/254":9}}`); rec.Code != 200 {
		t.Fatalf("valid save: %d %s", rec.Code, rec.Body)
	}
	// Slots merge, and 0 unmaps; other fields stay.
	post(`{"slots":{"p1/0":0,"p1/1":4}}`)
	var v spoolmanView
	rec := serveHandler(handleSpoolman, httptest.NewRequest("GET", "/api/spoolman", nil))
	json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.Enabled || v.URL != f.URL+"/" || len(v.Slots) != 2 || v.Slots["p1/254"] != 9 || v.Slots["p1/1"] != 4 || v.Log == nil {
		t.Fatalf("view = %+v", v)
	}
	if len(cfg.Printers) != 1 {
		t.Fatalf("printers touched: %+v", cfg.Printers)
	}
	req := httptest.NewRequest("POST", "/api/spoolman", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	if rec := serveHandler(handleSpoolman, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: %d, want 415", rec.Code)
	}
}

func TestSpoolmanTestAndSpoolsRoutes(t *testing.T) {
	f := newFakeSpoolman(t)
	withBridgeConfig(t, spoolmanTestConfig(f.URL))

	var reply struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	rec := serveHandler(handleSpoolmanTest, jsonPost("/api/spoolman/test", `{"url":"`+f.URL+`"}`))
	json.Unmarshal(rec.Body.Bytes(), &reply)
	if !reply.OK || !strings.Contains(reply.Message, "0.22.1") {
		t.Fatalf("test: %s", rec.Body)
	}
	rec = serveHandler(handleSpoolmanTest, jsonPost("/api/spoolman/test", `{}`)) // falls back to the saved address
	json.Unmarshal(rec.Body.Bytes(), &reply)
	if !reply.OK {
		t.Fatalf("test with the saved address: %s", rec.Body)
	}
	rec = serveHandler(handleSpoolmanTest, jsonPost("/api/spoolman/test", `{"url":"http://u:p@host"}`))
	reply.OK = true
	json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.OK || strings.Contains(rec.Body.String(), "p@host") {
		t.Fatalf("a URL with a password must be refused without echoing it: %s", rec.Body)
	}

	rec = serveHandler(handleSpoolmanSpools, httptest.NewRequest("GET", "/api/spoolman/spools", nil))
	var spools []spoolman.Spool
	if err := json.Unmarshal(rec.Body.Bytes(), &spools); err != nil || len(spools) != 2 || spools[0].Tag != "04AABBCC" {
		t.Fatalf("spools: %d %s", rec.Code, rec.Body)
	}
	withBridgeConfig(t, &config.Config{})
	if rec := serveHandler(handleSpoolmanSpools, httptest.NewRequest("GET", "/api/spoolman/spools", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("spools without an address: %d, want 400", rec.Code)
	}
}

func TestSyncSpoolmanOnPrintEnd(t *testing.T) {
	f := newFakeSpoolman(t)
	cfg := spoolmanTestConfig(f.URL)
	withBridgeConfig(t, cfg)
	prev := spoolSyncer
	spoolSyncer = spoolman.NewSyncer()
	spoolSyncer.Delays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { spoolSyncer = prev })

	// Bambu: slot 0 is mapped to spool 7, slot 2 is not mapped but its tag matches spool 1,
	// slot 3 has neither, slot 1 used nothing.
	syncSpoolman(history.Record{PrinterName: "Fox", Filaments: []history.FilamentUse{
		{Slot: 0, Grams: 12.5},
		{Slot: 1},
		{Slot: 2, Grams: 3, TagUID: "04aabbcc"},
		{Slot: 3, Grams: 9},
	}})
	got := f.recorded()
	if len(got) != 2 || !spoolContains(got, `7 {"use_weight":12.5}`) || !spoolContains(got, `1 {"use_weight":3}`) {
		t.Fatalf("Bambu sync sent %v", got)
	}
	if log := spoolSyncer.Log(); len(log) != 2 || log[0].Status != "ok" || log[1].Status != "ok" {
		t.Fatalf("log = %+v", log)
	}

	// Klipper: length goes to slot 0's spool.
	syncSpoolman(history.Record{PrinterName: "Voron", FilamentMM: 2500})
	if got := f.recorded(); len(got) != 3 || !spoolContains(got, `1 {"use_length":2500}`) {
		t.Fatalf("Klipper sync sent %v", got)
	}

	// Off, or no filament, or an unknown printer: nothing is sent.
	syncSpoolman(history.Record{PrinterName: "Ghost", FilamentMM: 100})
	syncSpoolman(history.Record{PrinterName: "Voron"})
	cfg.Spoolman.Enabled = false
	syncSpoolman(history.Record{PrinterName: "Voron", FilamentMM: 100})
	if got := f.recorded(); len(got) != 3 {
		t.Fatalf("sent when it should not: %v", got)
	}
}

func spoolContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
