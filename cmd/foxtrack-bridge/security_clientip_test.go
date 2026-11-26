package main

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func TestClientIPThrottleKey(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"direct LAN client", "192.168.1.20:5000", nil, "192.168.1.20"},
		{"LAN client with a spoofed header", "192.168.1.20:5000", []string{"10.0.0.7"}, "192.168.1.20"},
		{"LAN client with a spoofed list", "192.168.1.20:5000", []string{"1.2.3.4, 10.0.0.7"}, "192.168.1.20"},
		{"loopback without the header", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"IPv6 loopback without the header", "[::1]:5000", nil, "::1"},
		{"no port in RemoteAddr", "192.168.1.20", nil, "192.168.1.20"},
		{"proxy, one entry", "127.0.0.1:5000", []string{"10.0.0.7"}, "10.0.0.7"},
		{"proxy, last entry wins", "127.0.0.1:5000", []string{"1.2.3.4, 10.0.0.7"}, "10.0.0.7"},
		{"IPv6 proxy", "[::1]:5000", []string{"1.2.3.4, 10.0.0.7"}, "10.0.0.7"},
		{"mapped loopback proxy", "[::ffff:127.0.0.1]:5000", []string{"10.0.0.7"}, "10.0.0.7"},
		{"spaces trimmed", "127.0.0.1:5000", []string{"  1.2.3.4 ,   10.0.0.7  "}, "10.0.0.7"},
		{"entry with a port", "127.0.0.1:5000", []string{"10.0.0.7:4711"}, "10.0.0.7"},
		{"bracketed IPv6 with a port", "127.0.0.1:5000", []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"bracketed IPv6", "127.0.0.1:5000", []string{"[2001:db8::1]"}, "2001:db8::1"},
		{"bare IPv6", "127.0.0.1:5000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"several lines, last one counts", "127.0.0.1:5000", []string{"9.9.9.9", "1.1.1.1, 10.0.0.8"}, "10.0.0.8"},
		{"garbage falls back", "127.0.0.1:5000", []string{"not-an-ip"}, "127.0.0.1"},
		{"garbage last entry, earlier ones ignored", "127.0.0.1:5000", []string{"10.0.0.7, garbage"}, "127.0.0.1"},
		{"empty last entry falls back", "127.0.0.1:5000", []string{"10.0.0.7, "}, "127.0.0.1"},
		{"empty header falls back", "127.0.0.1:5000", []string{""}, "127.0.0.1"},
		{"hostname falls back", "127.0.0.1:5000", []string{"example.com"}, "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/auth/login", nil)
			r.RemoteAddr = c.remote
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r); got != c.want {
				t.Fatalf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// The throttle key is never a trust decision: a forwarded request from
// loopback still does not count as this computer.
func TestClientIPDoesNotChangeLoopbackTrust(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/config/export", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if loopbackClient(r) {
		t.Fatal("a forwarded request must not count as this computer")
	}
}

// fastHash is a one-iteration pbkdf2 hash, so the sign-in tests do not spend
// seconds hashing.
func fastHash(t *testing.T, pw string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	key, err := pbkdf2.Key(sha256.New, pw, salt, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	return "pbkdf2-sha256$1$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key)
}

func TestLoginLockBehindProxyIsPerClient(t *testing.T) {
	withSecurityConfig(t, &config.Security{PasswordHash: fastHash(t, "right password"), SessionKey: randomHex(32)})
	keys := []string{"10.0.0.7", "10.0.0.8", "127.0.0.1", "192.168.1.20"}
	t.Cleanup(func() {
		for _, k := range keys {
			noteLogin(k, true, time.Now())
		}
	})
	login := func(remote, xff, pw string) int {
		b, _ := json.Marshal(map[string]string{"password": pw})
		r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		handleAuthLogin(w, r)
		return w.Code
	}

	// One visitor behind the proxy gets five passwords wrong.
	for i := 0; i < 5; i++ {
		if c := login("127.0.0.1:40000", "203.0.113.9, 10.0.0.7", "wrong"); c != http.StatusUnauthorized {
			t.Fatalf("wrong try %d: got %d, want 401", i+1, c)
		}
	}
	if c := login("127.0.0.1:40001", "10.0.0.7", "right password"); c != http.StatusTooManyRequests {
		t.Fatalf("the locked visitor: got %d, want 429", c)
	}
	// Another visitor behind the same proxy, the owner, can still sign in.
	if c := login("127.0.0.1:40002", "10.0.0.8", "right password"); c != http.StatusOK {
		t.Fatalf("another visitor behind the proxy: got %d, want 200", c)
	}
	// So can this computer without the proxy.
	if c := login("127.0.0.1:40003", "", "right password"); c != http.StatusOK {
		t.Fatalf("loopback without the header: got %d, want 200", c)
	}
	// Changing the forwarded entries before the last one does not help.
	if c := login("127.0.0.1:40004", "10.0.0.8, 10.0.0.7", "right password"); c != http.StatusTooManyRequests {
		t.Fatalf("a spoofed earlier entry: got %d, want 429", c)
	}

	// A LAN client cannot dodge its own lock by sending the header.
	for i := 0; i < 5; i++ {
		if c := login("192.168.1.20:5000", "", "wrong"); c != http.StatusUnauthorized {
			t.Fatalf("LAN wrong try %d: got %d, want 401", i+1, c)
		}
	}
	if c := login("192.168.1.20:5001", "10.0.0.99", "right password"); c != http.StatusTooManyRequests {
		t.Fatalf("a LAN client with a spoofed header: got %d, want 429", c)
	}
}
