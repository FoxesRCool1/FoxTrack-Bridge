package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/plug"
	"foxtrack-bridge/webhook"
)

// A printer its plug switched off sends nothing, so the Bridge tells FoxTrack
// itself: OFFLINE plus the plug, every minute, and once more when the plug
// comes back on. A printer that reports speaks for itself.
func TestRelayQuietPlugs(t *testing.T) {
	var mu sync.Mutex
	on := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/rpc/Switch.GetStatus":
			if on {
				_, _ = w.Write([]byte(`{"output":true,"apower":30,"aenergy":{"total":10}}`))
			} else {
				_, _ = w.Write([]byte(`{"output":false,"apower":0,"aenergy":{"total":10}}`))
			}
		case "/rpc/Switch.Set":
			on = strings.Contains(r.URL.RawQuery, "on=true")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	const name, serial = "Quiet-plug-relay-test", "01P00QUIETPLUG"
	printers := []config.Printer{{
		ID: "p-q", Name: name, Serial: serial, Connection: "cloud",
		Plug: &config.Plug{Kind: "shelly", Address: strings.TrimPrefix(srv.URL, "http://")},
	}}
	type sent struct {
		id string
		p  webhook.RelayPayload
	}
	var got []sent
	var down bool // FoxTrack takes nothing
	oldSend := sendPlugRelay
	sendPlugRelay = func(key, url, id, _ string, p webhook.RelayPayload) error {
		if url != webhook.RelayURLV2 || key != "ftb_test" {
			t.Errorf("sent to %s with key %q", url, key)
		}
		if down {
			return errors.New("FoxTrack is down")
		}
		got = append(got, sent{id, p})
		return nil
	}
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{FoxTrack2APIKey: "ftb_test", Printers: printers}
	configMutex.Unlock()
	// A private manager: Stop on the shared one would stay stopped for the
	// next test or the next -count run.
	oldPlugs := plugs
	plugs = plug.NewManager()
	t.Cleanup(func() {
		sendPlugRelay = oldSend
		plugs.Stop() // wait for the pollers: they read configStore, restored below
		plugs = oldPlugs
		wirePlugs()
		plugRelayMu.Lock()
		delete(plugRelayLast, name)
		delete(plugRelayRetry, name)
		plugRelayMu.Unlock()
		configMutex.Lock()
		configStore = oldCfg
		configMutex.Unlock()
	})
	wirePlugs()
	plugs.Sync(printers)
	waitPlug := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if pl := plugs.Current(name); pl != nil && pl.Reachable && pl.On == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("plug never read as on=%v", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitPlug(false)

	now := time.Now()
	// The printer is off: no report.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "disconnected"})
	// A report FoxTrack did not take is not counted as sent: it is tried
	// again, half a heartbeat later and not on every tick.
	down = true
	relayQuietPlugs(now.Add(-40 * time.Second))
	down = false
	relayQuietPlugs(now.Add(-30 * time.Second))
	if len(got) != 0 {
		t.Fatalf("10 s after a failed report: %d reports, want 0", len(got))
	}
	relayQuietPlugs(now)
	if len(got) != 1 {
		t.Fatalf("plug off, printer quiet: %d reports, want 1", len(got))
	}
	pr := got[0].p.Print
	if got[0].id != serial || pr.GcodeState != "OFFLINE" || pr.Plug == nil || pr.Plug.On ||
		strings.Join(pr.BridgeCapabilities, ",") != "alarms,plug_on,plug_off" || pr.PrinterKind != "bambu" {
		t.Fatalf("report = id %q %+v", got[0].id, pr)
	}

	relayQuietPlugs(now.Add(20 * time.Second))
	if len(got) != 1 {
		t.Fatalf("within the heartbeat: %d reports, want still 1", len(got))
	}
	relayQuietPlugs(now.Add(61 * time.Second))
	if len(got) != 2 {
		t.Fatalf("after the heartbeat: %d reports, want 2", len(got))
	}

	// Switched on from FoxTrack: told once, then the printer takes over.
	if err := plugs.Set(context.Background(), name, true); err != nil {
		t.Fatal(err)
	}
	waitPlug(true)
	relayQuietPlugs(now.Add(70 * time.Second))
	if len(got) != 3 || !got[2].p.Print.Plug.On {
		t.Fatalf("plug on: %d reports (last %+v), want 3 with the plug on", len(got), got[len(got)-1].p.Print.Plug)
	}
	relayQuietPlugs(now.Add(200 * time.Second))
	if len(got) != 3 {
		t.Fatalf("plug on and already told: %d reports, want 3", len(got))
	}

	// A printer that reports carries the plug on its own relay.
	mu.Lock()
	on = false
	mu.Unlock()
	plugs.Sync(nil)
	plugs.Sync(printers)
	waitPlug(false)
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "idle", NozzleTemp: 25})
	relayQuietPlugs(time.Now())
	if len(got) != 3 {
		t.Fatalf("printer reporting: %d reports, want 3", len(got))
	}
	if st := statusFor(t, "p-q"); st.Status != "idle" {
		t.Fatalf("/api/status for a reporting printer = %q, want idle", st.Status)
	}
	// The plug is off and the printer sends nothing: the dashboard shows it
	// offline (Powered off), not its last state.
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: "connected", NozzleTemp: 25})
	if st := statusFor(t, "p-q"); st.Status != "disconnected" || st.NozzleTemp != 0 || st.Plug == nil || st.Plug.On {
		t.Fatalf("/api/status for a quiet plug-off printer = %+v", st)
	}
}

func statusFor(t *testing.T, id string) mqttpkg.TelemetryData {
	t.Helper()
	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
	var st map[string]mqttpkg.TelemetryData
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st[id]
}

// The plug-only report and the printer's own offline chain both reach
// FoxTrack, which stores each report as sent: they must be the same body, or
// the stored file, progress and filament slots flip between two versions.
func TestPlugOnlyPayload_MatchesTheOfflineChain(t *testing.T) {
	p := config.Printer{ID: "p-same", Name: "Plug-payload-test", Serial: "01P00SAMEBODY01", IP: "192.168.1.5", LANCode: "12345678"}
	pl := &webhook.RelayPlug{On: false, Reachable: true}
	got := plugOnlyPayload(p, pl)
	want := mqttpkg.OfflineRelayPayload(mqttpkg.Printer{Name: p.Name, Serial: p.Serial})
	if got.Print.Plug != pl || got.Print.GcodeState != "OFFLINE" || got.Print.McRemainingTime != -1 || got.Print.Alarms != nil {
		t.Fatalf("plug-only report = %+v", got.Print)
	}
	caps := strings.Join(got.Print.BridgeCapabilities, ",")
	if !strings.Contains(caps, "plug_on") || !strings.Contains(caps, "plug_off") {
		t.Fatalf("capabilities = %q, want the plug commands", caps)
	}
	got.Print.Plug, want.Print.Plug = nil, nil
	got.Print.BridgeCapabilities, want.Print.BridgeCapabilities = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plug-only report differs from the offline chain:\n got  %+v\n want %+v", got.Print, want.Print)
	}
}
