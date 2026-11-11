package webhook

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestForgetToken(t *testing.T) {
	ForgetToken("a")
	if !tokenForgotten("a") {
		t.Error("ForgetToken(\"a\") must make tokenForgotten(\"a\") true")
	}
	if tokenForgotten("b") {
		t.Error("tokenForgotten(\"b\") must stay false")
	}
	UseToken("a")
	if tokenForgotten("a") {
		t.Error("UseToken(\"a\") must make tokenForgotten(\"a\") false again")
	}
	ForgetToken("")
	if tokenForgotten("") {
		t.Error("ForgetToken(\"\") must be a no-op")
	}
}

// A queued relay retry for a forgotten token must not make any further HTTP
// request.
func TestRelayRetryDroppedWhenTokenForgotten(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	restore := useTestServer(t, srv)
	defer restore()

	oldBackoffs := relayBackoffs
	relayBackoffs = []time.Duration{time.Millisecond}
	defer func() { relayBackoffs = oldBackoffs }()

	ForgetToken("forgotten-token")
	defer UseToken("forgotten-token")

	// First attempt goes out (first sends are not blocked by the set).
	err := SendRelay("forgotten-token", srv.URL, "SER", "X1C", RelayPayload{})
	if err == nil {
		t.Fatal("expected an error from a 503")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected exactly one request after the first attempt, got %d", hits.Load())
	}
	if pendingRelayRetries.Load() == 0 {
		t.Fatal("expected a retry to be queued")
	}
	time.Sleep(50 * time.Millisecond)
	if got := pendingRelayRetries.Load(); got != 0 {
		t.Errorf("expected the pending retry to be dropped, depth %d", got)
	}
	if hits.Load() != 1 {
		t.Errorf("expected no further requests after the token was forgotten, got %d", hits.Load())
	}
}
