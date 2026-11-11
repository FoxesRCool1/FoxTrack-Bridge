package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postNamed is post with the printer's name chosen by the test.
func postNamed(t *testing.T, name string, p RelayPayload) (http.Header, map[string]json.RawMessage) {
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
	if err := SendRelay("tok", srv.URL, "SER-"+name, name, p); err != nil {
		t.Fatalf("SendRelay: %v", err)
	}
	return hdr, body
}

// Deno decodes header bytes as Latin-1, so the name header is sent that way;
// the exact name goes in the body.
func TestHeaderName(t *testing.T) {
	cases := map[string]string{
		"Café":                   "Caf\xe9",
		"  Voron 2.4  ":          "Voron 2.4",
		"Fox 🦊":                  "Fox ?",
		"Tab\there":              "Tab here",
		strings.Repeat("é", 200): strings.Repeat("\xe9", 128),
	}
	for in, want := range cases {
		if got := HeaderName(in); got != want {
			t.Errorf("HeaderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSendRelay_NameInBodyAndLatin1Header(t *testing.T) {
	resetRelayHealth(t)
	hdr, body := postNamed(t, "Café", RelayPayload{Print: RelayPrint{GcodeState: "IDLE"}})
	if got := hdr.Get("X-Printer-Name"); got != "Caf\xe9" {
		t.Errorf("X-Printer-Name = %q, want Latin-1 %q", got, "Caf\xe9")
	}
	if got := string(body["printer_name"]); got != `"Café"` {
		t.Errorf("printer_name in body = %s, want \"Café\"", got)
	}
}

// Any other 4xx from the current project is a report FoxTrack will keep
// refusing: it is shown on the dashboard and not retried.
func TestNotePermanentReject_Other4xxIsRejected(t *testing.T) {
	resetRelayHealth(t)
	msg := notePermanentReject(RelayURLV2, http.StatusBadRequest, []byte(`{"error":"Printer headers are limited to 128 characters"}`), "SER-1", "Long")
	if !strings.Contains(msg, "Printer headers are limited to 128 characters") || !strings.Contains(msg, "HTTP 400") {
		t.Fatalf("message = %q", msg)
	}
	p := RelayHealth()
	if p == nil || p.Kind != "rejected" || p.Printer != "Long" {
		t.Fatalf("problem = %+v, want kind rejected for Long", p)
	}
	if m := notePermanentReject(RelayURLV2, http.StatusNotFound, nil, "SER-1", "Long"); m != "" {
		t.Errorf("404 must stay as before, got %q", m)
	}
	if m := notePermanentReject(RelayURLV2, http.StatusTooManyRequests, nil, "SER-1", "Long"); m != "" {
		t.Errorf("429 must stay retryable, got %q", m)
	}
}

// A new or removed token forgets what the old one was told.
func TestResetRelayHealth_ForgetsProblemsAndDelivered(t *testing.T) {
	resetRelayHealth(t)
	setRelayProblem("unauthorized", "old token", "SER-1", "X1C")
	markDelivered()
	ResetRelayHealth()
	if RelayHealth() != nil || RelayDelivered() {
		t.Fatal("ResetRelayHealth must clear the problems and the delivered flag")
	}
}

func TestRelayDelivered_SetByFirstAcceptedReport(t *testing.T) {
	resetRelayHealth(t)
	relayProblemMu.Lock()
	relayDelivered = false
	relayProblemMu.Unlock()
	if RelayDelivered() {
		t.Fatal("delivered before any post")
	}
	postNamed(t, "X1C", RelayPayload{Print: RelayPrint{GcodeState: "IDLE"}})
	if !RelayDelivered() {
		t.Fatal("a 2xx from the current project must set delivered")
	}
}
