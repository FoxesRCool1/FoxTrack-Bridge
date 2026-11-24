package mqtt

import (
	"testing"
	"time"

	"foxtrack-bridge/history"
	"foxtrack-bridge/pace"
	"foxtrack-bridge/webhook"
)

func TestRelayRemaining(t *testing.T) {
	cases := []struct {
		status  string
		minutes int
		want    int
	}{
		{"printing", 0, -1}, // running with no estimate yet
		{"paused", 0, -1},
		{"printing", 37, 37},
		{"idle", 0, 0}, // not printing: 0 stays 0
		{"finished", 0, 0},
	}
	for _, c := range cases {
		if got := RelayRemaining(c.status, c.minutes); got != c.want {
			t.Errorf("RelayRemaining(%q, %d) = %d, want %d", c.status, c.minutes, got, c.want)
		}
	}
}

type sentRelay struct {
	url, serial string
	payload     webhook.RelayPayload
}

func captureRelays(t *testing.T) chan sentRelay {
	t.Helper()
	sent := make(chan sentRelay, 8)
	prev := sendRelay
	sendRelay = func(_, url, serial, _ string, p webhook.RelayPayload) error {
		sent <- sentRelay{url, serial, p}
		return nil
	}
	t.Cleanup(func() { sendRelay = prev })
	return sent
}

func nextRelay(t *testing.T, sent chan sentRelay) sentRelay {
	t.Helper()
	select {
	case s := <-sent:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no relay post")
		return sentRelay{}
	}
}

// An unreachable printer is reported as OFFLINE (no alarms key, unknown
// remaining time; the current project first, then the legacy one); a second
// report inside pace.HeartbeatSec does not post and one after it does; the
// first real report afterwards re-arms it.
func TestReportOffline_RepeatsEachHeartbeat(t *testing.T) {
	const name = "offline-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })
	sent := captureRelays(t)

	p := Printer{Name: name, Serial: "OFFL", APIKey: "legacy", FoxTrack2APIKey: "v2"}
	reportOffline(p, 1000)
	reportOffline(p, 1001)

	first := nextRelay(t, sent)
	if first.url != webhook.RelayURLV2 {
		t.Errorf("the current project must be posted first, got %s", first.url)
	}
	if first.serial != "OFFL" {
		t.Errorf("serial = %q", first.serial)
	}
	pr := first.payload.Print
	if pr.GcodeState != "OFFLINE" || pr.Alarms != nil || pr.McRemainingTime != -1 {
		t.Errorf("OFFLINE report = %+v", pr)
	}
	if second := nextRelay(t, sent); second.url != webhook.URL {
		t.Errorf("the legacy project comes second, got %s", second.url)
	}
	// Still inside the heartbeat window: nothing. After it: the report repeats.
	reportOffline(p, 1000+int64(pace.HeartbeatSec)-1)
	select {
	case extra := <-sent:
		t.Fatalf("OFFLINE re-posted inside the heartbeat window: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
	reportOffline(p, 1000+int64(pace.HeartbeatSec))
	if got := nextRelay(t, sent); got.payload.Print.GcodeState != "OFFLINE" {
		t.Fatalf("a printer still down after a heartbeat must be reported again, got %q", got.payload.Print.GcodeState)
	}
	nextRelay(t, sent) // the legacy copy

	// The printer comes back: a real report goes out and re-arms the OFFLINE post.
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"IDLE","nozzle_temper":25}}`})
	if got := nextRelay(t, sent); got.payload.Print.GcodeState != "IDLE" {
		t.Fatalf("first report after the outage = %q, want IDLE", got.payload.Print.GcodeState)
	}
	nextRelay(t, sent) // the legacy copy
	reportOffline(p, 2000)
	if got := nextRelay(t, sent); got.payload.Print.GcodeState != "OFFLINE" {
		t.Fatalf("a new outage must be reported, got %q", got.payload.Print.GcodeState)
	}
}

// A Bridge restart in the middle of a print keeps the print's start time and
// spool snapshot when the printer is still printing the same file; another file,
// or an ended print, starts clean.
func TestSession_SurvivesBridgeRestart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const name = "restart-test"
	resetPrinterState(name)
	t.Cleanup(func() { resetPrinterState(name) })

	p := Printer{Name: name, Serial: "RST"} // no API keys: nothing is posted
	start := func(file string, percent string) *printSession {
		t.Helper()
		makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","subtask_name":"` + file + `","mc_percent":` + percent + `}}`})
		sessionMu.Lock()
		defer sessionMu.Unlock()
		s := activeSessions[name]
		if s == nil {
			t.Fatal("no session after a RUNNING report")
		}
		c := *s
		return &c
	}

	first := start("cube.gcode", "10")
	// Make the print old enough to tell, in memory and on disk.
	sessionMu.Lock()
	sess := activeSessions[name]
	sess.StartTime -= 5000
	sess.Spools = []AmsSlot{{Slot: 0, Material: "PLA", Remaining: 80}}
	saveSession(p, sess)
	sessionMu.Unlock()
	wantStart := first.StartTime - 5000

	// The restart: all in-memory state is gone, the file stays.
	resetPrinterState(name)
	again := start("cube.gcode", "12")
	if again.StartTime != wantStart {
		t.Errorf("start after a restart = %d, want the original %d", again.StartTime, wantStart)
	}
	if len(again.Spools) != 1 || again.Spools[0].Remaining != 80 {
		t.Errorf("spool snapshot not kept: %+v", again.Spools)
	}

	// A different file is a different print.
	resetPrinterState(name)
	other := start("vase.gcode", "12")
	if other.StartTime == wantStart {
		t.Error("another file must not take the old session")
	}

	// The print ends: nothing is left to take back.
	makeHandler(p)(nil, fakeMessage{`{"print":{"gcode_state":"FINISH"}}`})
	var left savedSession
	if history.LoadSession(sessionKey(p), &left) {
		t.Errorf("an ended print left a saved session: %+v", left)
	}
	// The history record is written in the background. Wait for it, or the
	// write races the removal of the temp dir.
	deadline := time.Now().Add(3 * time.Second)
	for last, stable := -1, 0; stable < 3 && time.Now().Before(deadline); {
		recs, _ := history.ForPrinter(name)
		if len(recs) > 0 && len(recs) == last {
			stable++
		} else {
			stable = 0
		}
		last = len(recs)
		time.Sleep(20 * time.Millisecond)
	}
}
