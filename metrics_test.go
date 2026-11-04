//go:build headless

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

func metricsStr(s string) *string { return &s }

func TestRenderMetrics_ExactFormat(t *testing.T) {
	printers := []config.Printer{
		{ID: "a1", Name: "Fox \"Prime\"\\1\nline2", Serial: "S1"},
		{ID: "b2", Name: "Voron", MoonrakerURL: "http://v:7125"},
	}
	states := map[string]*mqttpkg.TelemetryData{
		printers[0].Name: {
			Status: "printing", Progress: 42, NozzleTemp: 210.5, NozzleTarget: 220, BedTemp: 60, BedTarget: 60, TimeRemaining: 35, PrinterModel: "P1S",
			Alarms: []webhook.RelayAlarm{
				{Severity: webhook.AlarmSerious, Message: metricsStr("Filament ran out")},
				{Severity: webhook.AlarmInfo, Message: metricsStr("chatter")},
			},
		},
		"Voron": {Status: "disconnected", NozzleTemp: 99}, // stale numbers must not be shown
	}
	stats := map[string]printStats{
		printers[0].Name: {Jobs: map[string]int{"finished": 3, "error": 1}, Seconds: 7200, EnergyWh: 150.5},
	}
	a := `printer="Fox \"Prime\"\\1\nline2",printer_id="a1",model="P1S",kind="bambu"`
	b := `printer="Voron",printer_id="b2",model="Klipper",kind="klipper"`
	want := `# HELP foxtrack_bridge_info The Bridge version, as a label. Always 1.
# TYPE foxtrack_bridge_info gauge
foxtrack_bridge_info{version="2.6.0"} 1
# HELP foxtrack_printer_up 1 when the printer is reporting to the Bridge, 0 when it is not.
# TYPE foxtrack_printer_up gauge
foxtrack_printer_up{` + a + `} 1
foxtrack_printer_up{` + b + `} 0
# HELP foxtrack_printer_printing 1 while the printer is printing, 0 otherwise.
# TYPE foxtrack_printer_printing gauge
foxtrack_printer_printing{` + a + `} 1
foxtrack_printer_printing{` + b + `} 0
# HELP foxtrack_printer_progress_ratio Progress of the current print, from 0 to 1.
# TYPE foxtrack_printer_progress_ratio gauge
foxtrack_printer_progress_ratio{` + a + `} 0.42
# HELP foxtrack_printer_nozzle_temperature_celsius Nozzle temperature in degrees Celsius.
# TYPE foxtrack_printer_nozzle_temperature_celsius gauge
foxtrack_printer_nozzle_temperature_celsius{` + a + `} 210.5
# HELP foxtrack_printer_nozzle_target_temperature_celsius Nozzle target temperature in degrees Celsius.
# TYPE foxtrack_printer_nozzle_target_temperature_celsius gauge
foxtrack_printer_nozzle_target_temperature_celsius{` + a + `} 220
# HELP foxtrack_printer_bed_temperature_celsius Bed temperature in degrees Celsius.
# TYPE foxtrack_printer_bed_temperature_celsius gauge
foxtrack_printer_bed_temperature_celsius{` + a + `} 60
# HELP foxtrack_printer_bed_target_temperature_celsius Bed target temperature in degrees Celsius.
# TYPE foxtrack_printer_bed_target_temperature_celsius gauge
foxtrack_printer_bed_target_temperature_celsius{` + a + `} 60
# HELP foxtrack_printer_time_remaining_seconds Estimated seconds left in the current print.
# TYPE foxtrack_printer_time_remaining_seconds gauge
foxtrack_printer_time_remaining_seconds{` + a + `} 2100
# HELP foxtrack_printer_alarms Alarms the printer reports right now, not counting info-level ones.
# TYPE foxtrack_printer_alarms gauge
foxtrack_printer_alarms{` + a + `} 1
# HELP foxtrack_printer_print_jobs_total Prints that ended, by result, from the Bridge's history.
# TYPE foxtrack_printer_print_jobs_total counter
foxtrack_printer_print_jobs_total{` + a + `,result="finished"} 3
foxtrack_printer_print_jobs_total{` + a + `,result="cancelled"} 0
foxtrack_printer_print_jobs_total{` + a + `,result="error"} 1
foxtrack_printer_print_jobs_total{` + b + `,result="finished"} 0
foxtrack_printer_print_jobs_total{` + b + `,result="cancelled"} 0
foxtrack_printer_print_jobs_total{` + b + `,result="error"} 0
# HELP foxtrack_printer_print_seconds_total Seconds spent printing, from the Bridge's history.
# TYPE foxtrack_printer_print_seconds_total counter
foxtrack_printer_print_seconds_total{` + a + `} 7200
foxtrack_printer_print_seconds_total{` + b + `} 0
# HELP foxtrack_printer_energy_wh_total Watt-hours used while printing, from the printer's smart plug.
# TYPE foxtrack_printer_energy_wh_total counter
foxtrack_printer_energy_wh_total{` + a + `} 150.5
`
	if got := renderMetrics("2.6.0", printers, states, stats); got != want {
		t.Errorf("metrics text differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderMetrics_NoPrinters(t *testing.T) {
	got := renderMetrics("dev", nil, nil, nil)
	if got != "# HELP foxtrack_bridge_info The Bridge version, as a label. Always 1.\n# TYPE foxtrack_bridge_info gauge\nfoxtrack_bridge_info{version=\"dev\"} 1\n" {
		t.Errorf("got %q", got)
	}
}

func TestMetricLabelValue(t *testing.T) {
	cases := map[string]string{
		`plain`:      `plain`,
		`a"b`:        `a\"b`,
		`a\b`:        `a\\b`,
		"a\nb":       `a\nb`,
		`\"`:         `\\\"`,
		"bad\xffutf": "bad\uFFFDutf",
	}
	for in, want := range cases {
		if got := metricLabelValue(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func setMetricsConfig(t *testing.T, enabled bool, printers []config.Printer) {
	t.Helper()
	isolateConfigDir(t)
	configMutex.Lock()
	old := configStore
	configStore = &config.Config{Printers: printers, Metrics: &config.Metrics{Enabled: enabled}}
	configMutex.Unlock()
	metricsHistory.Lock()
	metricsHistory.stats = nil
	metricsHistory.Unlock()
	oldLoad := loadHistory
	loadHistory = func() ([]history.Record, error) { return nil, nil }
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
		loadHistory = oldLoad
	})
}

func TestHandleMetrics_OffByDefaultIs404(t *testing.T) {
	setMetricsConfig(t, false, []config.Printer{{ID: "a1", Name: "Fox"}})
	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Metrics are turned off") || strings.Contains(body, "foxtrack_") {
		t.Errorf("body = %q", body)
	}
}

func TestHandleMetrics_On(t *testing.T) {
	setMetricsConfig(t, true, []config.Printer{{ID: "a1", Name: "Fox", Serial: "S"}})
	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `foxtrack_printer_up{printer="Fox",printer_id="a1"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest("POST", "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestPrintStatsByPrinter(t *testing.T) {
	printers := []config.Printer{{ID: "a1", Name: "New name", PreviousNames: []string{"Old name"}}, {ID: "b2", Name: "Other"}}
	setMetricsConfig(t, true, printers)
	calls := 0
	wh := 12.5
	loadHistory = func() ([]history.Record, error) {
		calls++
		return []history.Record{
			{PrinterName: "Old name", Result: "finished", Duration: 100, EnergyWh: &wh},
			{PrinterName: "New name", Result: "finished", Duration: 50},
			{PrinterName: "New name", Result: "cancelled", Duration: 10},
			{PrinterName: "Deleted printer", Result: "finished", Duration: 999},
		}, nil
	}
	now := time.Now()
	got := printStatsByPrinter(printers, now)
	s := got["New name"]
	if s.Jobs["finished"] != 2 || s.Jobs["cancelled"] != 1 || s.Seconds != 160 || s.EnergyWh != 12.5 {
		t.Errorf("stats = %+v (a rename must keep the old prints)", s)
	}
	if _, ok := got["Deleted printer"]; ok {
		t.Error("a printer that is gone must not appear")
	}
	printStatsByPrinter(printers, now.Add(29*time.Second))
	if calls != 1 {
		t.Errorf("history read %d times within 30 s, want 1", calls)
	}
	printStatsByPrinter(printers, now.Add(31*time.Second))
	if calls != 2 {
		t.Errorf("history read %d times after 30 s, want 2", calls)
	}
}

func TestHandleMetricsSettings(t *testing.T) {
	setMetricsConfig(t, false, []config.Printer{{ID: "a1", Name: "Fox"}})
	rec := httptest.NewRecorder()
	handleMetricsSettings(rec, jsonPost("/api/metrics", `{"enabled":true}`))
	if rec.Code != http.StatusOK || !metricsEnabled() {
		t.Fatalf("status = %d, enabled = %v: %s", rec.Code, metricsEnabled(), rec.Body.String())
	}
	configMutex.RLock()
	n := len(configStore.Printers)
	configMutex.RUnlock()
	if n != 1 {
		t.Errorf("printers = %d: a metrics save must not touch them", n)
	}
	rec = httptest.NewRecorder()
	handleMetricsSettings(rec, httptest.NewRequest("POST", "/api/metrics", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusUnsupportedMediaType || !metricsEnabled() {
		t.Errorf("a POST without a JSON type must be refused: %d", rec.Code)
	}
}
