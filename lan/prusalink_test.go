package lan

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// prusaLinkServer answers /api/v1/status and /api/v1/job with canned PrusaLink
// JSON and records the last write it was sent.
func prusaLinkServer(t *testing.T, state string, gotWrite *string, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "prusa-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/status":
			_, _ = w.Write([]byte(`{
				"job": {"id": 297, "progress": 42.0, "time_remaining": 1830, "time_printing": 1200},
				"storage": {"path": "/usb/", "name": "usb", "read_only": false},
				"printer": {"state": "` + state + `", "temp_nozzle": 214.9, "target_nozzle": 215.0,
					"temp_bed": 59.8, "target_bed": 60.0, "axis_z": 2.4, "speed": 100}
			}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/job":
			_, _ = w.Write([]byte(`{"id": 297, "state": "` + state + `", "progress": 42.0,
				"file": {"name": "CUBE~1.BGC", "display_name": "cube_PLA_MK4.bgcode", "path": "/usb"}}`))
		default:
			*gotWrite = r.Method + " " + r.URL.Path + " print=" + r.Header.Get("Print-After-Upload") + " overwrite=" + r.Header.Get("Overwrite")
			if gotBody != nil {
				b, _ := io.ReadAll(r.Body)
				*gotBody = string(b)
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func TestFetchPrusaLinkTelemetry_MapsFields(t *testing.T) {
	var write string
	srv := prusaLinkServer(t, "PRINTING", &write, nil)
	defer srv.Close()

	tel, relay, err := fetchTelemetry(configpkg.Printer{Name: "MK4", PrusaLinkURL: srv.URL + "/", APIKey: "prusa-key"})
	if err != nil {
		t.Fatal(err)
	}
	want := mqttpkg.TelemetryData{Status: "printing", FileName: "cube_PLA_MK4.bgcode", Progress: 42,
		NozzleTemp: 214.9, NozzleTarget: 215, BedTemp: 59.8, BedTarget: 60, TimeRemaining: 30, PrinterModel: "PrusaLink"}
	if !reflect.DeepEqual(tel, want) {
		t.Fatalf("telemetry = %+v\nwant %+v", tel, want)
	}
	p := relay.Print
	if p.GcodeState != "RUNNING" || p.SubTaskName != "cube_PLA_MK4.bgcode" || p.McPercent != 42 || p.McRemainingTime != 30 ||
		p.PrinterKind != "prusalink" || len(p.BridgeCapabilities) != 1 || p.BridgeCapabilities[0] != "print_file" {
		t.Fatalf("relay = %+v", p)
	}
}

func TestPrusaLinkCommandsAndUpload(t *testing.T) {
	var write, body string
	srv := prusaLinkServer(t, "PRINTING", &write, nil)
	defer srv.Close()
	c := NewController()
	c.printers["MK4"] = configpkg.Printer{Name: "MK4", PrusaLinkURL: srv.URL, APIKey: "prusa-key"}

	for cmd, want := range map[string]string{
		"pause":  "PUT /api/v1/job/297/pause print= overwrite=",
		"resume": "PUT /api/v1/job/297/resume print= overwrite=",
		"stop":   "DELETE /api/v1/job/297 print= overwrite=",
	} {
		write = ""
		if err := c.SendCommand("MK4", cmd, nil); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if write != want {
			t.Fatalf("%s sent %q, want %q", cmd, write, want)
		}
	}
	if err := c.SendCommand("MK4", "light", nil); err == nil {
		t.Fatal("light: want an unsupported-command error")
	}

	up := prusaLinkServer(t, "IDLE", &write, &body)
	defer up.Close()
	c.printers["MK4"] = configpkg.Printer{Name: "MK4", PrusaLinkURL: up.URL, APIKey: "prusa-key"}
	if err := c.UploadAndPrint(context.Background(), "MK4", writeTemp(t, "G28\n"), "My Cube.gcode"); err != nil {
		t.Fatal(err)
	}
	if write != "PUT /api/v1/files/usb/My_Cube.gcode print=?1 overwrite=?1" || body != "G28\n" {
		t.Fatalf("upload sent %q with body %q", write, body)
	}
}

func TestNormalizeState(t *testing.T) {
	for in, want := range map[string]string{
		// PrusaLink
		"IDLE": "idle", "READY": "idle", "BUSY": "busy", "PRINTING": "printing", "PAUSED": "paused",
		"ATTENTION": "paused", "FINISHED": "finished", "STOPPED": "cancelled", "ERROR": "error",
		// Home Assistant integrations
		"standby": "idle", "running": "printing", "pausing": "paused", "complete": "finished",
		"failed": "error", "unavailable": "disconnected", "unknown": "disconnected", "offline": "disconnected",
	} {
		if got := normalizeState(in); got != want {
			t.Errorf("normalizeState(%q) = %q, want %q", in, got, want)
		}
	}
	for status, want := range map[string]string{"printing": "RUNNING", "paused": "PAUSE", "finished": "FINISH",
		"error": "FAILED", "idle": "IDLE", "cancelled": "CANCELLED"} {
		if got := relayGcodeState(status); got != want {
			t.Errorf("relayGcodeState(%q) = %q, want %q", status, got, want)
		}
	}
}
