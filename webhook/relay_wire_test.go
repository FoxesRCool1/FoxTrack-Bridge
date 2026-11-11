package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clearRelayProblems() {
	relayProblemMu.Lock()
	relayProblems = map[string]*RelayProblem{}
	relayProblemMu.Unlock()
}

// post sends one relay payload to a test server and returns what arrived.
func post(t *testing.T, serial string, p RelayPayload) (http.Header, map[string]json.RawMessage) {
	t.Helper()
	var hdr http.Header
	var body map[string]json.RawMessage
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		_ = json.Unmarshal(outer["print"], &body)
	}))
	defer srv.Close()
	defer useTestServer(t, srv)()
	if err := SendRelay("tok", srv.URL, serial, "Shop Klipper", p); err != nil {
		t.Fatalf("SendRelay: %v", err)
	}
	return hdr, body
}

// A Klipper printer is keyed on its config ID; the name it used to be keyed on
// goes in X-Printer-Legacy-Id (current name first) so FoxTrack can re-key.
func TestSendRelay_KlipperIdentityHeaders(t *testing.T) {
	hdr, _ := post(t, "a1b2c3d4e5f60718", RelayPayload{LegacyID: "Shop Klipper,Old Name"})
	if got := hdr.Get("X-Printer-Serial"); got != "a1b2c3d4e5f60718" {
		t.Errorf("X-Printer-Serial = %q, want the printer ID", got)
	}
	if got := hdr.Get("X-Printer-Name"); got != "Shop Klipper" {
		t.Errorf("X-Printer-Name = %q", got)
	}
	if got := hdr.Get("X-Printer-Legacy-Id"); got != "Shop Klipper,Old Name" {
		t.Errorf("X-Printer-Legacy-Id = %q", got)
	}

	hdr, _ = post(t, "01P00A123456789", RelayPayload{})
	if _, sent := hdr["X-Printer-Legacy-Id"]; sent {
		t.Error("a Bambu printer sends no legacy id")
	}
}

// The remaining time is always on the wire: 0 stays 0, -1 means unknown.
func TestSendRelay_RemainingTimeIsAlwaysSent(t *testing.T) {
	for _, want := range []string{"-1", "0", "37"} {
		var n int
		if err := json.Unmarshal([]byte(want), &n); err != nil {
			t.Fatal(err)
		}
		_, body := post(t, "SER", RelayPayload{Print: RelayPrint{McRemainingTime: n}})
		if got := string(body["mc_remaining_time"]); got != want {
			t.Errorf("mc_remaining_time = %q, want %q", got, want)
		}
	}
}

// [] means all clear and a missing key means no change, so they must differ on
// the wire (omitempty would have merged them).
func TestSendRelay_AlarmsKeyOnlyWhenSet(t *testing.T) {
	_, body := post(t, "SER", RelayPayload{Print: RelayPrint{Alarms: CleanAlarms(nil)}})
	if got := string(body["alarms"]); got != "[]" {
		t.Errorf("a full report with no alarms sends %q, want []", got)
	}

	_, body = post(t, "SER", RelayPayload{Print: RelayPrint{GcodeState: "OFFLINE"}})
	if _, has := body["alarms"]; has {
		t.Errorf("a report without alarms must leave the key out, got %s", body["alarms"])
	}

	msg := "Filament ran out"
	_, body = post(t, "SER", RelayPayload{Print: RelayPrint{Alarms: []RelayAlarm{{Source: AlarmSourceMessage, Severity: AlarmFatal, Message: &msg}}}})
	if !strings.Contains(string(body["alarms"]), msg) {
		t.Errorf("alarms = %s", body["alarms"])
	}
}

func refuse(kind, serial, name string) {
	setRelayProblem(kind, kind+" for "+name, serial, name)
}

// One printer's refusal stays until that printer posts well; another printer's
// good post must not clear it, and the dashboard shows every refused printer.
func TestRelayProblems_ArePerPrinter(t *testing.T) {
	resetRelayHealth(t)
	refuse("printer_limit", "SER-A", "A")
	refuse("printer_limit", "SER-B", "B")

	p := RelayHealth()
	if p == nil || !strings.Contains(p.Printer, "A") || !strings.Contains(p.Printer, "B") {
		t.Fatalf("the union must name both printers, got %+v", p)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	defer useTestServer(t, srv)()

	if err := SendRelay("tok", srv.URL, "SER-A", "A", RelayPayload{}); err != nil {
		t.Fatal(err)
	}
	p = RelayHealth()
	if p == nil || p.Printer != "B" {
		t.Fatalf("after A posted well only B is refused, got %+v", p)
	}
	if err := SendRelay("tok", srv.URL, "SER-C", "C", RelayPayload{}); err != nil {
		t.Fatal(err)
	}
	if p := RelayHealth(); p == nil || p.Printer != "B" {
		t.Fatalf("a third printer posting well must not clear B, got %+v", p)
	}
	if err := SendRelay("tok", srv.URL, "SER-B", "B", RelayPayload{}); err != nil {
		t.Fatal(err)
	}
	if p := RelayHealth(); p != nil {
		t.Fatalf("B posted well, nothing is refused now: %+v", p)
	}
}

func TestRelayProblems_AccountWideComesFirstAndAnyGoodPostClearsIt(t *testing.T) {
	resetRelayHealth(t)
	refuse("printer_limit", "SER-A", "A")
	refuse("unauthorized", "SER-A", "A")
	if p := RelayHealth(); p == nil || p.Kind != "unauthorized" {
		t.Fatalf("a refused token outranks a printer limit, got %+v", p)
	}
	clearRelayProblem("SER-Z") // another printer posted well
	if p := RelayHealth(); p == nil || p.Kind != "printer_limit" || p.Printer != "A" {
		t.Fatalf("the token is fine again, A is still over the limit, got %+v", p)
	}
}

// A printer problem for a printer that stopped asking (removed) ages out.
func TestRelayProblems_StalePrinterProblemExpires(t *testing.T) {
	resetRelayHealth(t)
	refuse("printer_limit", "SER-GONE", "Gone")
	relayProblemMu.Lock()
	relayProblems["printer:SER-GONE"].seen = relayProblems["printer:SER-GONE"].seen.Add(-2 * printerProblemTTL)
	relayProblemMu.Unlock()
	if p := RelayHealth(); p != nil {
		t.Fatalf("a printer that stopped asking must not hold the banner: %+v", p)
	}
}

// FoxTrack answers 202 {"ok":false,"skipped":"unknown printer"} for a printer it
// does not know. That is a problem for that printer, not a "sent", and it is not
// retried.
func TestSendHistoryAndSnapshot_SkippedIsAProblemNotRetried(t *testing.T) {
	resetRelayHealth(t)
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"ok":false,"skipped":"unknown printer"}`))
	}))
	defer srv.Close()
	defer useTestServer(t, srv)()

	err := doSendHistory("tok", srv.URL, HistoryPayload{PrinterName: "Prusa", Serial: "ID-1", Result: "finished"})
	if !errors.Is(err, errNotRetryable) {
		t.Fatalf("a skipped history post must not be retried, got %v", err)
	}
	p := RelayHealth()
	if p == nil || p.Kind != "unknown_printer" || p.Printer != "Prusa" || !strings.Contains(p.Message, "link it in FoxTrack") {
		t.Fatalf("expected an unknown_printer problem for Prusa, got %+v", p)
	}

	SendHistory("tok", srv.URL, HistoryPayload{PrinterName: "Prusa", Serial: "ID-1", Result: "finished"})
	if pendingHistoryRetries.Load() != 0 {
		t.Errorf("a skipped history post entered the retry queue")
	}

	clearRelayProblems()
	err = SendSnapshot("tok", srv.URL, "ID-2", "Voron", []byte{0xff, 0xd8, 0xff})
	if !errors.Is(err, errNotRetryable) {
		t.Fatalf("a skipped snapshot must report the refusal, got %v", err)
	}
	if p := RelayHealth(); p == nil || p.Printer != "Voron" {
		t.Fatalf("expected a problem for Voron, got %+v", p)
	}
	if hits != 3 {
		t.Errorf("hits = %d, want 3 (no retries)", hits)
	}

	// The printer is then known: its next relay post clears the problem.
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	defer useTestServer(t, ok)()
	if err := SendRelay("tok", ok.URL, "ID-2", "Voron", RelayPayload{}); err != nil {
		t.Fatal(err)
	}
	if p := RelayHealth(); p != nil {
		t.Fatalf("the printer posted well, got %+v", p)
	}
}
