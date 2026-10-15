package lan

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configpkg "foxtrack-bridge/config"
)

func haServer(t *testing.T, states map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ha-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := states[strings.TrimPrefix(r.URL.Path, "/api/states/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func haPrinter(url string) configpkg.Printer {
	return configpkg.Printer{Name: "Prusa via HA", HomeAssistantURL: url, HomeAssistantToken: "ha-token",
		HAStateEntity: "sensor.mk4_state", HAProgressEntity: "sensor.mk4_progress", HANozzleEntity: "sensor.mk4_nozzle",
		HABedEntity: "sensor.mk4_bed", HAFileEntity: "sensor.mk4_file", HARemainingEntity: "sensor.mk4_remaining"}
}

func TestFetchHomeAssistantTelemetry_MapsEntities(t *testing.T) {
	srv := haServer(t, map[string]string{
		"sensor.mk4_state":     `{"entity_id": "sensor.mk4_state", "state": "printing", "attributes": {"friendly_name": "MK4"}}`,
		"sensor.mk4_progress":  `{"state": "57", "attributes": {"unit_of_measurement": "%"}}`,
		"sensor.mk4_nozzle":    `{"state": "215.2", "attributes": {"unit_of_measurement": "°C"}}`,
		"sensor.mk4_bed":       `{"state": "60.1", "attributes": {"unit_of_measurement": "°C"}}`,
		"sensor.mk4_file":      `{"state": "benchy.gcode", "attributes": {}}`,
		"sensor.mk4_remaining": `{"state": "5400", "attributes": {"unit_of_measurement": "s"}}`,
	})
	defer srv.Close()

	tel, relay, err := fetchTelemetry(haPrinter(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if tel.Status != "printing" || tel.Progress != 57 || tel.NozzleTemp != 215.2 || tel.BedTemp != 60.1 ||
		tel.FileName != "benchy.gcode" || tel.TimeRemaining != 90 {
		t.Fatalf("telemetry = %+v", tel)
	}
	p := relay.Print
	if p.GcodeState != "RUNNING" || p.PrinterKind != "home_assistant" || p.BridgeCapabilities != nil || p.McRemainingTime != 90 {
		t.Fatalf("relay = %+v", p)
	}
}

func TestFetchHomeAssistantTelemetry_OptionalEntitiesAndOffline(t *testing.T) {
	states := map[string]string{
		"sensor.mk4_state":     `{"state": "Paused", "attributes": {}}`,
		"sensor.mk4_progress":  `{"state": "unavailable", "attributes": {}}`,
		"sensor.mk4_file":      `{"state": "unknown", "attributes": {}}`,
		"sensor.mk4_remaining": `{"state": "12", "attributes": {"unit_of_measurement": "min"}}`,
		// nozzle and bed are missing (404): left at 0
	}
	srv := haServer(t, states)
	defer srv.Close()

	tel, _, err := fetchTelemetry(haPrinter(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if tel.Status != "paused" || tel.Progress != 0 || tel.FileName != "" || tel.NozzleTemp != 0 || tel.TimeRemaining != 12 {
		t.Fatalf("telemetry = %+v", tel)
	}

	states["sensor.mk4_state"] = `{"state": "unavailable", "attributes": {}}`
	if _, _, err := fetchTelemetry(haPrinter(srv.URL)); err == nil {
		t.Fatal("an unavailable printer must read as an error (offline), got nil")
	}

	bad := haPrinter(srv.URL)
	bad.HomeAssistantToken = "wrong"
	if _, _, err := fetchTelemetry(bad); err == nil {
		t.Fatal("a refused token must read as an error, got nil")
	}
}

func TestHomeAssistantIsReadOnly(t *testing.T) {
	c := NewController()
	c.printers["Prusa via HA"] = haPrinter("http://127.0.0.1:1")
	if err := c.SendCommand("Prusa via HA", "pause", nil); !errors.Is(err, errReadOnly) {
		t.Fatalf("pause: err = %v, want errReadOnly", err)
	}
	if err := c.PrintReady("Prusa via HA"); !errors.Is(err, errReadOnly) {
		t.Fatalf("PrintReady: err = %v, want errReadOnly", err)
	}
}

func TestHAMinutes(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		state, unit string
		want        int
	}{
		{"90", "min", 90}, {"90", "", 90}, {"5400", "s", 90}, {"1.5", "h", 90},
		{"2026-10-15T13:30:00+00:00", "", 90}, {"2026-10-15T11:00:00+00:00", "", 0}, {"unknown", "min", 0},
	} {
		if got := haMinutes(c.state, c.unit, now); got != c.want {
			t.Errorf("haMinutes(%q, %q) = %d, want %d", c.state, c.unit, got, c.want)
		}
	}
}
