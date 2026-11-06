//go:build headless

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/webhook"
)

// startBridgePoller runs the real poll loop for one test and stops it (and
// waits for it to end) when the test finishes, so no poller outlives its test
// and eats another test's wake signal.
func startBridgePoller(t *testing.T) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { pollBridgeCommandsUntil(stop); close(done) }()
	t.Cleanup(func() { close(stop); <-done })
}

// queueBridge sets what the fake FoxTrack hands out on the next GET and wakes
// the poller. Calling it with no commands clears the queue.
func (e *printEnv) queueBridge(cmds ...bridgeCommand) {
	e.mu.Lock()
	e.pending = cmds
	e.mu.Unlock()
	wakeBridgeCommands()
}

func (e *printEnv) fetched() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.getHeader != nil
}

func waitBridge(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v", within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ackRecorder answers every POST with a fixed status and keeps what it saw.
type ackRecorder struct {
	hits   atomic.Int32
	mu     sync.Mutex
	auth   []string
	bodies []string
}

func newAckRecorder(t *testing.T, status int) *ackRecorder {
	t.Helper()
	rec := &ackRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.bodies = append(rec.bodies, string(body))
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	oldURL := webhook.BridgeCommandsURLV2
	webhook.BridgeCommandsURLV2 = srv.URL
	t.Cleanup(func() { webhook.BridgeCommandsURLV2 = oldURL })
	return rec
}

func TestBridgePoll_ControlCommandIsDispatchedAndAcked(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	e.queueBridge(bridgeCommand{ID: "c1", ExternalID: "Voron", Command: "pause"})
	waitBridge(t, 10*time.Second, func() bool { return e.count("ack:") >= 1 })
	e.queueBridge()

	// Control commands are not deduped, so a pass that starts before the queue
	// is cleared can ack again. Every ack must still be done or failed.
	got := e.snapshot()
	if e.count("upload:") != 0 || len(got) == 0 {
		t.Fatalf("events = %v", got)
	}
	for _, ev := range got {
		if !(strings.HasPrefix(ev, "ack:done") || strings.HasPrefix(ev, "ack:failed:")) {
			t.Fatalf("events = %v", got)
		}
	}
}

func TestBridgePoll_PrintFileIsRunningAckedThenDone(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	e.queueBridge(e.command("c1", "Voron", "gcode", nil))
	waitBridge(t, 10*time.Second, func() bool { return e.count("ack:done") >= 1 })
	e.queueBridge()

	got := strings.Join(e.snapshot(), " | ")
	if got != "ack:running | upload:Voron:vase.gcode | ack:done" {
		t.Fatalf("events = %s", got)
	}
}

func TestBridgePoll_UnknownCommandIsAckedFailedWithMessage(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	e.queueBridge(bridgeCommand{ID: "c1", ExternalID: "Voron", Command: "frobnicate"})
	waitBridge(t, 10*time.Second, func() bool { return e.count("ack:") >= 1 })
	e.queueBridge()

	// Unknown commands are not deduped, so a repeat ack is allowed; each must be failed with a message.
	got := e.snapshot()
	if len(got) == 0 {
		t.Fatalf("events = %v", got)
	}
	for _, ev := range got {
		if !strings.HasPrefix(ev, "ack:failed:") || strings.TrimPrefix(ev, "ack:failed:") == "" {
			t.Fatalf("events = %v", got)
		}
	}
}

func TestBridgePoll_CommandForAnotherPrinterIsNotAcked(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	e.queueBridge(bridgeCommand{ID: "c1", ExternalID: "Someone Elses Printer", Command: "pause"})
	waitBridge(t, 10*time.Second, e.fetched)
	time.Sleep(300 * time.Millisecond)
	e.queueBridge()

	if got := e.snapshot(); len(got) != 0 {
		t.Fatalf("expected no ack, got %v", got)
	}
}

func TestBridgePoll_NoAPIKeyMeansNoPolling(t *testing.T) {
	e := newPrintEnv(t)
	configMutex.Lock()
	configStore.FoxTrack2APIKey = ""
	configMutex.Unlock()

	startBridgePoller(t)
	e.queueBridge(bridgeCommand{ID: "c1", ExternalID: "Voron", Command: "pause"})
	time.Sleep(500 * time.Millisecond)

	if e.fetched() || len(e.snapshot()) != 0 {
		t.Fatalf("polled without an API key: events = %v", e.snapshot())
	}
}

func TestBridgePoll_ErrorMessageCutTo500Chars(t *testing.T) {
	e := newPrintEnv(t)
	long := strings.Repeat("x", 600)
	configMutex.Lock()
	configStore.Printers = append(configStore.Printers, config.Printer{Name: long, MoonrakerURL: "http://127.0.0.1:7125"})
	configMutex.Unlock()

	startBridgePoller(t)
	// Console commands are the ones whose failure message is cut to maxErrorChars.
	e.queueBridge(bridgeCommand{ID: "c1", ExternalID: long, Command: "set_nozzle_temp"})
	waitBridge(t, 10*time.Second, func() bool { return e.count("ack:") >= 1 })
	e.queueBridge()

	got := e.snapshot()
	if len(got) == 0 || !strings.HasPrefix(got[0], "ack:failed:") {
		t.Fatalf("events = %v", got)
	}
	msg := strings.TrimPrefix(got[0], "ack:failed:")
	if n := len([]rune(msg)); n != maxErrorChars {
		t.Fatalf("message has %d chars, want %d", n, maxErrorChars)
	}
	if !strings.HasSuffix(msg, "...") {
		t.Fatalf("truncated message does not end in ...: %q", msg[len(msg)-10:])
	}
}

func TestBridgeWake_TriggersImmediatePoll(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	// Wait for one pass, so the poller is in its wait. Its next scheduled pass
	// is seconds away, so an answer within 2 s can only come from the wake.
	waitBridge(t, 10*time.Second, e.fetched)

	e.mu.Lock()
	e.pending = []bridgeCommand{{ID: "c1", ExternalID: "Voron", Command: "pause"}}
	e.mu.Unlock()
	wakeBridgeCommands()

	waitBridge(t, 2*time.Second, func() bool { return e.count("ack:") >= 1 })
	e.queueBridge()
}

func TestBridgeAck_PostsResultWithBearerAndResultShape(t *testing.T) {
	rec := newAckRecorder(t, http.StatusOK)
	err := ackBridgeCommand("test-key", bridgeCommandResult{CommandID: "c1", Status: "failed", ErrorMessage: "boom"})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if rec.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", rec.hits.Load())
	}
	rec.mu.Lock()
	auth, body := rec.auth[0], rec.bodies[0]
	rec.mu.Unlock()
	if auth != "Bearer test-key" {
		t.Fatalf("Authorization = %q", auth)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if len(got) != 3 || got["command_id"] != "c1" || got["status"] != "failed" || got["error_message"] != "boom" {
		t.Fatalf("body = %s", body)
	}
}

func TestBridgeAck_NotFoundStopsRetrying(t *testing.T) {
	rec := newAckRecorder(t, http.StatusNotFound)
	oldBackoff := printAckBackoff
	printAckBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { printAckBackoff = oldBackoff })

	ackWithRetry("test-key", bridgeCommandResult{CommandID: "c1", Status: "failed"}, func(string, ...interface{}) {})
	if rec.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (no retry after 404)", rec.hits.Load())
	}
}
