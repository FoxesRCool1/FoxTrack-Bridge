package lan

import (
	"net/http"
	"net/http/httptest"
	"testing"

	configpkg "foxtrack-bridge/config"
)

func TestFetchKlipperTelemetry_MapsFieldsAndUsesHeader(t *testing.T) {
	const expectedPath = "/printer/objects/query"
	const expectedQuery = "print_stats&heater_bed&extruder&display_status&virtual_sdcard"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != expectedPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.RawQuery != expectedQuery {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Api-Key"); got != "moon-key" {
			t.Fatalf("missing X-Api-Key header: got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"result": {
				"status": {
					"print_stats": {
						"state": "printing",
						"filename": "cube.gcode",
						"print_duration": 120
					},
					"display_status": {
						"progress": 0.42
					},
					"virtual_sdcard": {
						"progress": 0.10
					},
					"extruder": {
						"temperature": 215.4,
						"target": 220.0
					},
					"heater_bed": {
						"temperature": 58.1,
						"target": 60.0
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	printer := configpkg.Printer{
		Name:         "K1",
		MoonrakerURL: srv.URL,
		APIKey:       "moon-key",
	}

	telemetry, relay, err := fetchKlipperTelemetry(printer)
	if err != nil {
		t.Fatalf("fetchKlipperTelemetry error: %v", err)
	}

	if telemetry.Status != "printing" {
		t.Fatalf("unexpected telemetry status: %s", telemetry.Status)
	}
	if telemetry.Progress != 42 {
		t.Fatalf("unexpected telemetry progress: %d", telemetry.Progress)
	}
	if telemetry.FileName != "cube.gcode" {
		t.Fatalf("unexpected telemetry file: %s", telemetry.FileName)
	}
	if telemetry.NozzleTemp != 215.4 || telemetry.NozzleTarget != 220.0 {
		t.Fatalf("unexpected nozzle values: now=%v target=%v", telemetry.NozzleTemp, telemetry.NozzleTarget)
	}
	if telemetry.BedTemp != 58.1 || telemetry.BedTarget != 60.0 {
		t.Fatalf("unexpected bed values: now=%v target=%v", telemetry.BedTemp, telemetry.BedTarget)
	}

	if relay.Print.GcodeState != "RUNNING" {
		t.Fatalf("unexpected relay gcode_state: %s", relay.Print.GcodeState)
	}
	if relay.Print.McPercent != 42 {
		t.Fatalf("unexpected relay mc_percent: %d", relay.Print.McPercent)
	}
	if relay.Print.SubTaskName != "cube.gcode" {
		t.Fatalf("unexpected relay subtask_name: %s", relay.Print.SubTaskName)
	}
	if relay.Print.NozzleTemper != 215.4 || relay.Print.NozzleTargetTemper != 220.0 {
		t.Fatalf("unexpected relay nozzle values: now=%v target=%v", relay.Print.NozzleTemper, relay.Print.NozzleTargetTemper)
	}
	if relay.Print.BedTemper != 58.1 || relay.Print.BedTargetTemper != 60.0 {
		t.Fatalf("unexpected relay bed values: now=%v target=%v", relay.Print.BedTemper, relay.Print.BedTargetTemper)
	}
}

func TestFetchKlipperTelemetry_ProgressFallbackToVirtualSDCard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"result": {
				"status": {
					"print_stats": {
						"state": "paused",
						"filename": "benchy.gcode"
					},
					"display_status": {
						"progress": 0
					},
					"virtual_sdcard": {
						"progress": 0.77
					},
					"extruder": {
						"temperature": 200,
						"target": 210
					},
					"heater_bed": {
						"temperature": 50,
						"target": 55
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	printer := configpkg.Printer{Name: "Klipper", MoonrakerURL: srv.URL}
	telemetry, relay, err := fetchKlipperTelemetry(printer)
	if err != nil {
		t.Fatalf("fetchKlipperTelemetry error: %v", err)
	}

	if telemetry.Progress != 77 {
		t.Fatalf("expected telemetry progress 77, got %d", telemetry.Progress)
	}
	if relay.Print.McPercent != 77 {
		t.Fatalf("expected relay mc_percent 77, got %d", relay.Print.McPercent)
	}
	if relay.Print.GcodeState != "PAUSE" {
		t.Fatalf("expected relay gcode_state PAUSE, got %s", relay.Print.GcodeState)
	}
}

func TestMapMoonrakerRelayState(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "printing", want: "RUNNING"},
		{in: "paused", want: "PAUSE"},
		{in: "complete", want: "FINISH"},
		{in: "error", want: "FAILED"},
		{in: "standby", want: "IDLE"},
		{in: "", want: "IDLE"},
		{in: "custom_state", want: "CUSTOM_STATE"},
	}

	for _, tc := range cases {
		got := mapMoonrakerRelayState(tc.in)
		if got != tc.want {
			t.Fatalf("state %q: expected %q, got %q", tc.in, tc.want, got)
		}
	}
}
