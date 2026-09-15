package webhook

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetRelayHealth clears the package-level problem so tests do not leak into
// each other.
func resetRelayHealth(t *testing.T) {
	t.Helper()
	clearRelayProblem()
	t.Cleanup(clearRelayProblem)
}

func TestNotePermanentReject_PlanLimit(t *testing.T) {
	resetRelayHealth(t)

	body := []byte(`{"error":"plan_limit","message":"plan_limit: FoxTrack Bridge requires a Pro or Enterprise plan."}`)
	msg := notePermanentReject(RelayURLV2, http.StatusForbidden, body, "Prusa")
	if msg == "" {
		t.Fatal("expected a plan_limit rejection to be reported")
	}
	if strings.HasPrefix(msg, "plan_limit: ") {
		t.Errorf("the raw SQL prefix must be stripped before the user sees it: %q", msg)
	}

	p := RelayHealth()
	if p == nil || p.Kind != "plan_limit" {
		t.Fatalf("expected a plan_limit problem, got %+v", p)
	}
	if p.Printer != "Prusa" {
		t.Errorf("printer = %q, want Prusa", p.Printer)
	}
	if p.Since == "" {
		t.Error("Since must be set so the dashboard can say how long this has been broken")
	}
}

func TestNotePermanentReject_PrinterLimitNamesTheNumbers(t *testing.T) {
	resetRelayHealth(t)

	body := []byte(`{"error":"printer_limit_reached","currentCount":3,"maxLimit":3,"plan":"starter"}`)
	msg := notePermanentReject(RelayURLV2, http.StatusForbidden, body, "X1C")
	for _, want := range []string{"starter", "3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should mention %q so the user knows what to change", msg, want)
		}
	}
	if p := RelayHealth(); p == nil || p.Kind != "printer_limit" {
		t.Fatalf("expected printer_limit, got %+v", p)
	}
}

func TestNotePermanentReject_Unauthorized(t *testing.T) {
	resetRelayHealth(t)

	if msg := notePermanentReject(HistoryURLV2, http.StatusUnauthorized, nil, "X1C"); msg == "" {
		t.Fatal("a 401 must be reported: retrying a revoked token never succeeds")
	}
	if p := RelayHealth(); p == nil || p.Kind != "unauthorized" {
		t.Fatalf("expected unauthorized, got %+v", p)
	}
}

// A 500 is a transient server fault, not something the user can fix, so it must
// stay on the retry path and never raise the dashboard banner.
func TestNotePermanentReject_IgnoresTransientFailures(t *testing.T) {
	resetRelayHealth(t)

	if msg := notePermanentReject(RelayURLV2, http.StatusInternalServerError, nil, "X1C"); msg != "" {
		t.Errorf("a 500 must stay retryable, got %q", msg)
	}
	if p := RelayHealth(); p != nil {
		t.Errorf("a 500 must not raise a problem banner, got %+v", p)
	}
}

// The bridge still dual-writes to the legacy project. A stale legacy token must
// not raise an alarm about a perfectly healthy current connection.
func TestNotePermanentReject_IgnoresLegacyProject(t *testing.T) {
	resetRelayHealth(t)

	if msg := notePermanentReject(URL, http.StatusUnauthorized, nil, "X1C"); msg != "" {
		t.Errorf("legacy rejections must be ignored, got %q", msg)
	}
	if p := RelayHealth(); p != nil {
		t.Errorf("legacy rejections must not set a problem, got %+v", p)
	}
}

// A permanent reject must fail fast: one request, no retry queue entry, and a
// problem raised for the dashboard. Before this the bridge sent the same doomed
// payload four times and then dropped it without telling anyone.
func TestSendRelay_PermanentRejectIsNotRetried(t *testing.T) {
	resetRelayHealth(t)

	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"plan_limit","message":"plan_limit: upgrade required"}`))
	}))
	defer srv.Close()

	restore := useTestServer(t, srv)
	defer restore()

	err := SendRelay("token", srv.URL, "SER", "X1C", RelayPayload{})
	if err == nil {
		t.Fatal("expected an error from a 403")
	}
	if !errors.Is(err, errNotRetryable) {
		t.Fatalf("a plan rejection must be marked not-retryable, got %v", err)
	}
	if hits != 1 {
		t.Errorf("expected exactly one request, got %d", hits)
	}
	if len(retryQueue) != 0 {
		t.Errorf("a permanent reject must not enter the retry queue, depth %d", len(retryQueue))
	}
	if p := RelayHealth(); p == nil || p.Kind != "plan_limit" {
		t.Fatalf("expected the dashboard problem to be raised, got %+v", p)
	}
}

// A successful send to the current project clears a previously raised problem,
// so the banner disappears once the user upgrades or fixes their token.
func TestSendRelay_SuccessClearsProblem(t *testing.T) {
	resetRelayHealth(t)
	setRelayProblem("plan_limit", "stale", "X1C")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	restore := useTestServer(t, srv)
	defer restore()

	if err := SendRelay("token", srv.URL, "SER", "X1C", RelayPayload{}); err != nil {
		t.Fatalf("SendRelay: %v", err)
	}
	if p := RelayHealth(); p != nil {
		t.Errorf("a successful send must clear the banner, got %+v", p)
	}
}

// useTestServer points the package at srv: its TLS client, and its URL treated
// as a current-project endpoint.
func useTestServer(t *testing.T, srv *httptest.Server) func() {
	t.Helper()
	oldClient := relayHTTPClient
	client := srv.Client()
	client.Timeout = oldClient.Timeout
	relayHTTPClient = client

	oldURLs := currentProjectURLs
	currentProjectURLs = append(append([]string{}, oldURLs...), srv.URL)

	return func() {
		relayHTTPClient = oldClient
		currentProjectURLs = oldURLs
	}
}

func TestSendSnapshot_RefusesOversizeFrameBeforeUpload(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := SendSnapshot("token", srv.URL, "SER", "X1C", make([]byte, MaxSnapshotBytes+1))
	if err == nil {
		t.Fatal("a frame over the receiver's cap must be refused locally")
	}
	if hits != 0 {
		t.Errorf("an oversize frame must not be uploaded at all, got %d request(s)", hits)
	}
}

func TestSendSnapshot_RefusesEmptyFrame(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	if err := SendSnapshot("token", srv.URL, "SER", "X1C", nil); err == nil {
		t.Fatal("an empty frame must be refused")
	}
	if hits != 0 {
		t.Errorf("an empty frame must not be uploaded, got %d request(s)", hits)
	}
}
