package plug

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A rejected login is cached per host and credentials: the plug is not asked
// again until the TTL passes, and a different password is tried normally.
func TestTapoAESLoginFailCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	origNow := tapoNow
	tapoNow = func() time.Time { return now }
	defer func() { tapoNow = origNow }()

	key := tapoLoginKey(host, "me@example.com", "wrong")
	forgetLoginFail(key)
	defer forgetLoginFail(key)

	rememberLoginFail(key, errKlapLogin)
	if _, err := tapoAESLogin(context.Background(), host, "me@example.com", "wrong"); err != errKlapLogin {
		t.Fatalf("cached login error %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("plug contacted %d times despite cached failure", n)
	}

	if cachedLoginFail(tapoLoginKey(host, "me@example.com", "right")) != nil {
		t.Fatalf("different password was blocked")
	}

	now = now.Add(loginFailTTL - time.Second)
	if cachedLoginFail(key) == nil {
		t.Fatalf("failure expired early")
	}
	now = now.Add(2 * time.Second)
	if cachedLoginFail(key) != nil {
		t.Fatalf("failure did not expire after TTL")
	}
}
