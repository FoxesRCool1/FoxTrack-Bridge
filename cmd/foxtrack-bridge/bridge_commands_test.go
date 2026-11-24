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

	// Control commands are deduped on the command id, so a pass that starts
	// before the queue is cleared does not run or ack it again.
	got := e.snapshot()
	if e.count("upload:") != 0 || len(got) != 1 {
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

	// Unknown commands are deduped like the quick ones: one ack, failed with a message.
	got := e.snapshot()
	if len(got) != 1 {
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
	waitBridge(t, 10*time.Second, func() bool { return e.count("ack:failed:") >= 1 })
	e.queueBridge()

	// A console command is acked "running" first, then with its result.
	got := e.snapshot()
	if len(got) != 2 || got[0] != "ack:running" || !strings.HasPrefix(got[1], "ack:failed:") {
		t.Fatalf("events = %v", got)
	}
	msg := strings.TrimPrefix(got[1], "ack:failed:")
	if n := len([]rune(msg)); n != maxErrorChars {
		t.Fatalf("message has %d chars, want %d", n, maxErrorChars)
	}
	if !strings.HasSuffix(msg, "...") {
		t.Fatalf("truncated message does not end in ...: %q", msg[len(msg)-10:])
	}
}

// quickPrinter adds a Klipper printer whose fake Moonraker counts the pause
// requests it gets, and returns its external id and the counter.
func quickPrinter(t *testing.T, e *printEnv) (string, *atomic.Int32) {
	t.Helper()
	pauses := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/printer/objects/query"):
			_, _ = w.Write([]byte(`{"result":{"status":{"print_stats":{"state":"printing"}}}}`))
		case strings.HasPrefix(r.URL.Path, "/printer/print/pause"):
			pauses.Add(1)
			_, _ = w.Write([]byte(`{"result":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return filesPrinters(e, config.Printer{Name: "Voron Quick", MoonrakerURL: srv.URL})[0], pauses
}

// G2: FoxTrack re-sends a command until its ack lands. A lost ack must not run
// the command a second time; the ack itself is retried.
func TestQuickCommand_LostAckDoesNotRunItTwice(t *testing.T) {
	e := newPrintEnv(t)
	external, pauses := quickPrinter(t, e)
	e.mu.Lock()
	e.ackFail["done"] = 1 // the first done ack is answered 500
	e.mu.Unlock()

	cmd := bridgeCommand{ID: "q1", ExternalID: external, Command: "pause"}
	startQuickCommand("test-key", cmd)
	startQuickCommand("test-key", cmd) // the next poll hands it out again
	printJobs.wg.Wait()
	startQuickCommand("test-key", cmd)
	printJobs.wg.Wait()

	if pauses.Load() != 1 {
		t.Fatalf("the printer was paused %d times, want 1; events = %v", pauses.Load(), e.snapshot())
	}
	if e.count("ack500:done") != 1 || e.count("ack:done") != 1 || len(e.snapshot()) != 2 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// G2: a command that failed to run still gets its failed ack, once, and is
// not run again on the next poll.
func TestQuickCommand_FailedCommandIsAckedFailedOnce(t *testing.T) {
	e := newPrintEnv(t)
	cmd := bridgeCommand{ID: "q1", ExternalID: "Voron", Command: "frobnicate"}
	startQuickCommand("test-key", cmd)
	printJobs.wg.Wait()
	startQuickCommand("test-key", cmd)
	printJobs.wg.Wait()

	got := e.snapshot()
	if len(got) != 1 || !strings.HasPrefix(got[0], "ack:failed:") || got[0] == "ack:failed:" {
		t.Fatalf("events = %v", got)
	}
}

// G2: a command for a printer this Bridge does not have is neither acked nor
// marked seen, so another Bridge can still take it.
func TestQuickCommand_OtherBridgesPrinterIsNotMarkedSeen(t *testing.T) {
	e := newPrintEnv(t)
	startQuickCommand("test-key", bridgeCommand{ID: "q1", ExternalID: "Someone Elses Printer", Command: "pause"})
	printJobs.wg.Wait()
	if len(e.snapshot()) != 0 || printJobs.hasSeen("q1") {
		t.Fatalf("events = %v, seen = %v", e.snapshot(), printJobs.hasSeen("q1"))
	}
}

// G2: the seen ids do not pile up; an id older than seenCommandTTL is dropped
// the next time the set is used.
func TestQuickCommand_SeenIdsArePrunedAfterTheTTL(t *testing.T) {
	newPrintEnv(t)
	printJobs.mu.Lock()
	printJobs.seen["old"] = time.Now().Add(-seenCommandTTL - time.Minute)
	printJobs.mu.Unlock()
	printJobs.markSeen("new")
	printJobs.mu.Lock()
	_, oldKept := printJobs.seen["old"]
	n := len(printJobs.seen)
	printJobs.mu.Unlock()
	if oldKept || n != 1 {
		t.Fatalf("seen has %d ids, old kept = %v; want only the new one", n, oldKept)
	}
}

// G1: a console command is acked "running" before it runs, so FoxTrack does
// not expire a long home while the printer is still moving.
func TestConsoleCommand_AcksRunningBeforeTheResult(t *testing.T) {
	e := newPrintEnv(t)
	cmd := bridgeCommand{ID: "k1", ExternalID: "Voron", Command: "set_nozzle_temp"}
	startConsoleCommand("test-key", cmd)
	startConsoleCommand("test-key", cmd)
	printJobs.wg.Wait()

	got := e.snapshot()
	if len(got) != 2 || got[0] != "ack:running" || !(strings.HasPrefix(got[1], "ack:done") || strings.HasPrefix(got[1], "ack:failed:")) {
		t.Fatalf("events = %v", got)
	}
}

// G1: FoxTrack answers 404 to the running ack when the command expired or
// another Bridge took it. The command is then not run and not tried again.
func TestConsoleCommand_Running404SkipsTheCommand(t *testing.T) {
	e := newPrintEnv(t)
	e.mu.Lock()
	e.runningHTTP = http.StatusNotFound
	e.mu.Unlock()
	cmd := bridgeCommand{ID: "k1", ExternalID: "Voron", Command: "set_nozzle_temp"}
	startConsoleCommand("test-key", cmd)
	startConsoleCommand("test-key", cmd)
	printJobs.wg.Wait()

	if len(e.snapshot()) != 0 || e.runningTry.Load() != 1 {
		t.Fatalf("events = %v, running acks = %d (want none, 1)", e.snapshot(), e.runningTry.Load())
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

func TestBridgeCommandsReply_OldReplyWithoutAppAlerts(t *testing.T) {
	var reply bridgeCommandsReply
	if err := json.Unmarshal([]byte(`{"commands":[],"watched":true,"poll_after_ms":5000}`), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.AppAlerts != nil {
		t.Fatalf("AppAlerts = %v, want nil", reply.AppAlerts)
	}
}

func TestBridgeCommandsReply_AppAlertsDecodes(t *testing.T) {
	var reply bridgeCommandsReply
	if err := json.Unmarshal([]byte(`{"commands":[],"app_alerts":{"telegram":["print_error"]}}`), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(reply.AppAlerts["telegram"]) != 1 || reply.AppAlerts["telegram"][0] != "print_error" {
		t.Fatalf("AppAlerts = %v", reply.AppAlerts)
	}
}

func TestBridgeCommandsReply_EmptyAppAlertsIsNonNil(t *testing.T) {
	var reply bridgeCommandsReply
	if err := json.Unmarshal([]byte(`{"app_alerts":{}}`), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.AppAlerts == nil || len(reply.AppAlerts) != 0 {
		t.Fatalf("AppAlerts = %v, want a non-nil empty map", reply.AppAlerts)
	}
}

func TestBridgePoll_GetCarriesEmptyNotifyHeaderWithoutChannels(t *testing.T) {
	e := newPrintEnv(t)
	startBridgePoller(t)
	waitBridge(t, 10*time.Second, e.fetched)
	e.mu.Lock()
	notify := e.getHeader.Get("X-Bridge-Notify")
	e.mu.Unlock()
	if notify != "{}" {
		t.Fatalf("X-Bridge-Notify = %q, want {}", notify)
	}
}
