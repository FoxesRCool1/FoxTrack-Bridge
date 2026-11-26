//go:build headless

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
)

// Every writer of the printer list (PATCH, create, PUT, a cloud printer's new
// IP) builds a new config and swaps it in, so a reader that copied the stored
// pointer under the lock and reads it after unlocking never sees a write. Run
// with -race; the old in-place PATCH and cloud-IP writes were reported here.
func TestPrinterWritersSwapTheConfig(t *testing.T) {
	cfg := &config.Config{
		Printers: []config.Printer{
			{ID: "a", Name: "A", IP: "192.0.2.1", Serial: "SA", LANCode: "11111111"},
			{ID: "c", Name: "C", IP: "192.0.2.3", Serial: "SC", Connection: config.ConnectionCloud},
		},
		Notifications: &config.Notifications{Channels: []config.NotifyChannel{
			{ID: "n", Kind: "ntfy", Enabled: true, Events: []string{"none"}, Printers: []string{"a"}},
		}},
	}
	withBridgeConfig(t, cfg)
	forgetLAN(t, "New")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var rounds atomic.Int64
	reader := func(read func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				read()
				rounds.Add(1)
			}
		}()
	}
	reader(func() { dispatchNotification(events.Event{Kind: events.PrintFinished, PrinterID: "a"}) })
	reader(func() { handleStatus(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/status", nil)) })
	reader(func() {
		configMutex.RLock()
		held := configStore
		configMutex.RUnlock()
		for _, p := range held.Printers { // read after unlocking, as a held snapshot is
			_ = p.Name + p.IP
			_ = p.CameraHidden
		}
	})
	waitRounds := func() {
		for n := rounds.Load(); rounds.Load() < n+60; {
			time.Sleep(time.Millisecond)
		}
	}
	waitRounds()

	do := func(method, path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost {
			handlePrinters(rec, req)
		} else {
			handlePrinterByName(rec, req)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status %d: %s", method, path, rec.Code, rec.Body)
		}
		waitRounds()
	}
	do(http.MethodPatch, "/api/printers/a", `{"camera_hidden":true}`)
	do(http.MethodPost, "/api/printers", `{"name":"New","moonraker_url":"http://127.0.0.1:1"}`)
	do(http.MethodPut, "/api/printers/New", `{"webcam_url":"http://127.0.0.1:1/cam"}`)
	onCloudIP("SC", "192.0.2.99")
	waitRounds()
	close(stop)
	wg.Wait()

	if len(cfg.Printers) != 2 || cfg.Printers[0].CameraHidden || cfg.Printers[1].IP != "192.0.2.3" {
		t.Errorf("the config a reader held was edited in place: %+v", cfg.Printers)
	}
	live := liveConfig()
	if len(live.Printers) != 3 {
		t.Fatalf("live printers = %+v, want 3", live.Printers)
	}
	if !live.Printers[0].CameraHidden || live.Printers[1].IP != "192.0.2.99" || live.Printers[2].WebcamURL != "http://127.0.0.1:1/cam" {
		t.Errorf("live printers = %+v, want camera hidden on A, C at 192.0.2.99, New with its webcam", live.Printers)
	}
	saved, err := config.LoadConfig()
	if err != nil || len(saved.Printers) != 3 || !saved.Printers[0].CameraHidden || saved.Printers[1].IP != "192.0.2.99" {
		t.Errorf("saved config = %+v (%v)", saved, err)
	}
}

// Deleting a printer while notifications go out must not edit the channel list
// the sender is reading: DELETE builds a new config and swaps it in. Run with
// -race; the old in-place edit was reported as a data race here.
func TestDeletePrinterWhileDispatching(t *testing.T) {
	cfg := &config.Config{
		Printers: []config.Printer{
			{ID: "a", Name: "A", IP: "192.0.2.1", Serial: "SA", LANCode: "11111111"},
			{ID: "b", Name: "B", IP: "192.0.2.2", Serial: "SB", LANCode: "22222222"},
		},
		// Events no event matches: the loop reads every channel and sends nothing.
		Notifications: &config.Notifications{Channels: []config.NotifyChannel{
			{ID: "both", Kind: "ntfy", Enabled: true, Events: []string{"none"}, Printers: []string{"a", "b"}},
			{ID: "only-a", Kind: "ntfy", Enabled: true, Events: []string{"none"}, Printers: []string{"a"}},
		}},
		Timelapse: &config.Timelapse{Enabled: true, Printers: []string{"a", "b"}},
	}
	withBridgeConfig(t, cfg)

	stop := make(chan struct{})
	done := make(chan struct{})
	var rounds atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			dispatchNotification(events.Event{Kind: events.PrintFinished, PrinterID: "b"})
			rounds.Add(1)
		}
	}()
	for rounds.Load() < 50 {
		time.Sleep(time.Millisecond)
	}
	rec := httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodDelete, "/api/printers/a", nil))
	for n := rounds.Load(); rounds.Load() < n+50; {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d: %s", rec.Code, rec.Body)
	}
	live := liveConfig()
	if live == cfg {
		t.Fatal("DELETE edited the stored config in place instead of swapping in a copy")
	}
	if len(live.Printers) != 1 || live.Printers[0].ID != "b" {
		t.Errorf("printers after delete = %+v, want only b", live.Printers)
	}
	ch := live.Notifications.Channels
	if len(ch[0].Printers) != 1 || ch[0].Printers[0] != "b" || ch[1].Enabled {
		t.Errorf("channels after delete = %+v, want both=[b] and only-a off", ch)
	}
	if p := live.Timelapse.Printers; len(p) != 1 || p[0] != "b" {
		t.Errorf("timelapse printers = %v, want [b]", p)
	}
	// The config the sender may still hold is as it was.
	if old := cfg.Notifications.Channels; len(old[0].Printers) != 2 || !old[1].Enabled || len(cfg.Printers) != 2 {
		t.Errorf("the replaced config was edited: %+v", old)
	}
}

// Deleting a printer takes its ID out of everything that names printers by
// ID. A list that would become empty ("all printers") turns the feature off
// instead of widening it.
func TestForgetPrinterIDs(t *testing.T) {
	cfg := &config.Config{
		Notifications: &config.Notifications{Channels: []config.NotifyChannel{
			{ID: "all", Enabled: true},
			{ID: "both", Enabled: true, Printers: []string{"a", "b"}},
			{ID: "only-a", Enabled: true, Printers: []string{"a"}},
		}},
		Timelapse:   &config.Timelapse{Enabled: true, Printers: []string{"a"}},
		Security:    &config.Security{Shares: []config.ShareLink{{ID: "s1", PrinterID: "a"}, {ID: "s2", PrinterID: "b"}}},
		Maintenance: &config.Maintenance{Tasks: []config.MaintenanceTask{{ID: "t1", PrinterID: "a"}, {ID: "t2", PrinterID: "b"}}},
	}
	forgetPrinterIDs(cfg, map[string]bool{"a": true})

	ch := cfg.Notifications.Channels
	if !ch[0].Enabled || len(ch[0].Printers) != 0 {
		t.Errorf("a channel for all printers changed: %+v", ch[0])
	}
	if !ch[1].Enabled || len(ch[1].Printers) != 1 || ch[1].Printers[0] != "b" {
		t.Errorf("channel with two printers = %+v, want only b left", ch[1])
	}
	if ch[2].Enabled || len(ch[2].Printers) == 0 {
		t.Errorf("channel whose last printer went = %+v, want off and not widened to all", ch[2])
	}
	if cfg.Timelapse.Enabled || len(cfg.Timelapse.Printers) == 0 {
		t.Errorf("timelapse = %+v, want off and not widened to all", cfg.Timelapse)
	}
	if s := cfg.Security.Shares; len(s) != 1 || s[0].ID != "s2" {
		t.Errorf("shares = %+v, want only s2", s)
	}
	if m := cfg.Maintenance.Tasks; len(m) != 1 || m[0].ID != "t2" {
		t.Errorf("tasks = %+v, want only t2", m)
	}

	forgetPrinterIDs(nil, map[string]bool{"a": true})              // no panic
	forgetPrinterIDs(&config.Config{}, map[string]bool{"a": true}) // no panic
}

// A sign-in check that never ran (the Bridge was busy) is not a wrong password.
func TestUndoLoginTry(t *testing.T) {
	const ip = "192.0.2.77"
	now := time.Now()
	t.Cleanup(func() { noteLogin(ip, true, now) })
	for i := 0; i < 20; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("try %d locked out though every try was taken back", i)
		}
		undoLoginTry(ip)
	}
	// Real wrong passwords still lock.
	locked := false
	for i := 0; i < 6; i++ {
		if beginLogin(ip, now) > 0 {
			locked = true
		}
	}
	if !locked {
		t.Fatal("six wrong passwords did not lock the address")
	}
}

// A try taken back at the edge of the limit leaves no lock behind.
func TestUndoLoginTry_AtTheLimit(t *testing.T) {
	const ip = "192.0.2.78"
	now := time.Now()
	t.Cleanup(func() { noteLogin(ip, true, now) })
	for i := 0; i < 4; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("try %d locked out too early", i)
		}
	}
	for i := 0; i < 3; i++ {
		if wait := beginLogin(ip, now); wait > 0 {
			t.Fatalf("locked out by a try that was taken back (round %d)", i)
		}
		undoLoginTry(ip)
	}
}

func TestSafePrinterID(t *testing.T) {
	for id, want := range map[string]bool{
		"":                                 true,
		"0f3a9c":                           true,
		"fallback-ab_1.2":                  true,
		"a'b":                              false,
		"a b":                              false,
		"a/b":                              false,
		"<x>":                              false,
		string(make([]byte, 65)):           false,
		"ééé":                              false,
		"0123456789abcdef0123456789abcdef": true,
	} {
		if got := safePrinterID(id); got != want {
			t.Errorf("safePrinterID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestPrinterNameFor(t *testing.T) {
	configMutex.Lock()
	old := configStore
	configStore = &config.Config{Printers: []config.Printer{{ID: "id-1", Name: "Voron"}}}
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
	})
	if got := printerNameFor("id-1"); got != "Voron" {
		t.Errorf("by id = %q, want Voron", got)
	}
	if got := printerNameFor("Voron"); got != "Voron" {
		t.Errorf("by name = %q, want Voron", got)
	}
	if got := printerNameFor(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}
