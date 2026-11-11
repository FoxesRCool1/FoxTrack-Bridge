package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/plug"
)

// A printer added through a Bambu account still has its plug on the LAN: FoxTrack's
// plug_on / plug_off reach it by serial, and the off guard reads the cloud report.
func TestPlugCommand_BambuCloudPrinter(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	on := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/rpc/Switch.GetStatus":
			if on {
				_, _ = w.Write([]byte(`{"output":true,"apower":120,"aenergy":{"total":500}}`))
			} else {
				_, _ = w.Write([]byte(`{"output":false,"apower":0,"aenergy":{"total":500}}`))
			}
		case "/rpc/Switch.Set":
			calls = append(calls, r.URL.RawQuery)
			on = strings.Contains(r.URL.RawQuery, "on=true")
			_, _ = w.Write([]byte(`{"was_on":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	const name, serial = "Madame-cloud-plug-test", "01P00CLOUDPLUG"
	printers := []config.Printer{{
		ID: "p-cloud", Name: name, Serial: serial, Connection: "cloud",
		Plug: &config.Plug{Kind: "shelly", Address: strings.TrimPrefix(srv.URL, "http://")},
	}}
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{Printers: printers}
	configMutex.Unlock()
	t.Cleanup(func() {
		plugs.Stop() // wait for the pollers: they read configStore, restored below
		configMutex.Lock()
		configStore = oldCfg
		configMutex.Unlock()
	})
	if !isBambuPrinterConfig(printers[0]) {
		t.Fatal("a cloud printer must be a Bambu printer for command lookup")
	}
	wirePlugs()
	plugs.Sync(printers)

	run := func(cmd string) error {
		return executeBridgeCommand(bridgeCommand{ID: "c1", ExternalID: serial, Command: cmd})
	}

	// Printing (from the cloud report): off is refused, nothing is switched.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "printing", NozzleTemp: 215})
	if err := run("plug_off"); !errors.Is(err, plug.ErrPrinting) {
		t.Fatalf("plug_off while printing: err = %v, want ErrPrinting", err)
	}
	// Idle but hot: still refused.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "idle", NozzleTemp: 180})
	if err := run("plug_off"); !errors.Is(err, plug.ErrHot) {
		t.Fatalf("plug_off with a hot nozzle: err = %v, want ErrHot", err)
	}
	// Idle and cool: off goes through, then on.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "idle", NozzleTemp: 30})
	if err := run("plug_off"); err != nil {
		t.Fatalf("plug_off idle and cool: %v", err)
	}
	if err := run("plug_on"); err != nil {
		t.Fatalf("plug_on: %v", err)
	}
	// The dashboard shows the plug even while the printer sends no reports
	// (switched off by its plug): /api/status reads the plug poller.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "disconnected"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := httptest.NewRecorder()
		handleStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
		var st map[string]mqttpkg.TelemetryData
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if pl := st["p-cloud"].Plug; pl != nil && pl.Reachable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/api/status has no plug for the cloud printer: %s", rec.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	got := strings.Join(calls, " | ")
	mu.Unlock()
	if !strings.Contains(got, "on=false") || !strings.HasSuffix(got, "on=true") || len(calls) != 2 {
		t.Fatalf("switch calls = %q, want one off then one on", got)
	}
}
