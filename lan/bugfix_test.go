package lan

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// Moonraker holds POST /printer/gcode/script open until G28 has finished, which
// takes far longer than the 6 s the other commands get.
func TestKlipperHome_WaitsForSlowHoming(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 6.5 s")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(6500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
	if err := c.SendCommand("Voron", "home", nil); err != nil {
		t.Fatalf("a slow home must still succeed: %v", err)
	}
}

// A poll that reaches PrusaLink or OctoPrint but finds the printer gone must
// count as a failed poll, so a running print gets pollGrace.
func TestFetchTelemetry_LostPrinterIsAFailedPoll(t *testing.T) {
	var write string
	prusa := prusaLinkServer(t, "OFFLINE", &write, nil)
	defer prusa.Close()
	if _, _, err := fetchTelemetry(configpkg.Printer{Name: "MK4", PrusaLinkURL: prusa.URL, APIKey: "prusa-key"}); err == nil {
		t.Error("PrusaLink: an offline printer must read as an error")
	}

	octo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/printer" {
			_, _ = w.Write([]byte(octoPrinterJSON("Offline", "", "")))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer octo.Close()
	if _, _, err := fetchTelemetry(configpkg.Printer{Name: "Ender", OctoPrintURL: octo.URL}); err == nil {
		t.Error("OctoPrint: an offline printer must read as an error")
	}
}

// A webcam stream (multipart) runs past the still-image limit; a still does not.
func TestProxyCamera_StreamOutlivesStillLimit(t *testing.T) {
	old := cameraStillLimit
	cameraStillLimit = 200 * time.Millisecond
	defer func() { cameraStillLimit = old }()

	slow := func(contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte("one"))
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
			_, _ = w.Write([]byte("two"))
		}
	}
	proxy := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		defer srv.Close()
		c := NewController()
		c.printers["K"] = configpkg.Printer{Name: "K", MoonrakerURL: srv.URL, WebcamURL: srv.URL}
		rec := httptest.NewRecorder()
		_ = c.ProxyCamera(rec, httptest.NewRequest("GET", "/cam", nil), "K")
		return rec.Body.String()
	}
	if got := proxy(slow("multipart/x-mixed-replace; boundary=f")); got != "onetwo" {
		t.Errorf("stream was cut: %q", got)
	}
	if got := proxy(slow("image/jpeg")); got != "one" {
		t.Errorf("a still must stay bounded: %q", got)
	}
}

func TestRemoveAndRenamePrinter_Bookkeeping(t *testing.T) {
	c := NewController()
	seed := func(name string) {
		c.sessions[name] = &printSession{StartTime: 1}
		c.relayLastT[name] = 5
		c.relayHeld[name] = true
		c.snapLastT[name] = 7
	}
	seed("Old")
	c.RenamePrinter("Old", "New")
	if c.sessions["New"] == nil || c.relayLastT["New"] != 5 || !c.relayHeld["New"] || c.snapLastT["New"] != 7 {
		t.Fatalf("rename lost the bookkeeping: %+v", c.sessions)
	}
	if c.sessions["Old"] != nil || len(c.relayLastT) != 1 {
		t.Fatalf("rename left the old name behind: %+v %+v", c.sessions, c.relayLastT)
	}
	c.RemovePrinter("Old")
	if c.sessions["New"] == nil {
		t.Fatal("removing the old name must not touch the renamed printer")
	}
	c.RemovePrinter("New")
	if len(c.sessions) != 0 || len(c.relayLastT) != 0 || len(c.relayHeld) != 0 || len(c.snapLastT) != 0 {
		t.Fatalf("remove left bookkeeping: %+v %+v %+v %+v", c.sessions, c.relayLastT, c.relayHeld, c.snapLastT)
	}
}

// A poll already running when its printer is removed must not put the state back.
func TestStoreState_IgnoresRemovedPrinter(t *testing.T) {
	c := NewController()
	c.AddOrUpdatePrinter(configpkg.Printer{Name: "K", MoonrakerURL: "http://127.0.0.1:1"}, "", "")
	c.mu.RLock()
	stop := c.cancelers["K"]
	c.mu.RUnlock()
	if _, ok := c.storeState("K", stop, &mqttpkg.TelemetryData{Status: "idle"}); !ok {
		t.Fatal("a registered printer must store its state")
	}
	c.RemovePrinter("K")
	if _, ok := c.storeState("K", stop, &mqttpkg.TelemetryData{Status: "idle"}); ok {
		t.Fatal("a removed printer's poll must not store state")
	}
	if len(c.GetStates()) != 0 {
		t.Fatalf("states came back: %+v", c.GetStates())
	}
}

// One relay post per printer at a time; while it runs, only the newest later
// payload is kept.
func TestQueueRelay_OneInFlightKeepsNewest(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	release := make(chan struct{})
	old := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		mu.Lock()
		sent = append(sent, p.Print.SubTaskName)
		first := len(sent) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return nil
	}
	defer func() { sendRelay = old }()

	c := NewController()
	job := func(file string) relayJob {
		return relayJob{foxAPIKey: "k", payload: webhook.RelayPayload{Print: webhook.RelayPrint{SubTaskName: file}}}
	}
	start := time.Now()
	c.queueRelay("K", job("a"))
	c.queueRelay("K", job("b"))
	c.queueRelay("K", job("c"))
	if time.Since(start) > time.Second {
		t.Fatal("queueRelay must not wait for the post")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := strings.Join(sent, ",")
		mu.Unlock()
		if got == "a,c" {
			return
		}
		if time.Now().After(deadline) || got == "a,b,c" || got == "a,b" {
			t.Fatalf("sent %q, want a then only the newest, c", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
