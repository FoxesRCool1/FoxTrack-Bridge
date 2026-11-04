package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword("correct horse", h) {
		t.Fatal("the right password was refused")
	}
	if checkPassword("correct hors", h) || checkPassword("", h) || checkPassword("correct horse", "plain") {
		t.Fatal("a wrong password or a bad hash was accepted")
	}
}

func TestSessionValidity(t *testing.T) {
	sec := &config.Security{PasswordHash: "pbkdf2-sha256$1$00$00", SessionKey: randomHex(32)}
	now := time.Now()
	v := newSessionValue(sec, now.Add(time.Hour))
	if !validSession(v, sec, now) {
		t.Fatal("fresh session refused")
	}
	if validSession(v, sec, now.Add(2*time.Hour)) {
		t.Fatal("expired session accepted")
	}
	other := *sec
	other.PasswordHash = "pbkdf2-sha256$1$00$01"
	if validSession(v, &other, now) {
		t.Fatal("a new password must end earlier sessions")
	}
	tampered := []byte(v)
	tampered[len(tampered)-1] ^= 1
	if validSession(string(tampered), sec, now) || validSession("junk", sec, now) {
		t.Fatal("tampered session accepted")
	}
}

func TestLoginThrottle(t *testing.T) {
	ip, now := "10.9.9.9", time.Now()
	defer noteLogin(ip, true, now)
	for i := 0; i < 4; i++ {
		noteLogin(ip, false, now)
	}
	if loginLockedFor(ip, now) != 0 {
		t.Fatal("locked before five failures")
	}
	noteLogin(ip, false, now)
	if d := loginLockedFor(ip, now); d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("first lock = %v, want 30s", d)
	}
	noteLogin(ip, true, now)
	if loginLockedFor(ip, now) != 0 {
		t.Fatal("a correct password must clear the lock")
	}
	// Tries counted up front: the sixth of six parallel tries is refused
	// before any of their hashes finished.
	for i := 0; i < 5; i++ {
		if beginLogin(ip, now) != 0 {
			t.Fatalf("try %d refused too early", i+1)
		}
	}
	if beginLogin(ip, now) == 0 {
		t.Fatal("the sixth try must wait")
	}
}

// withSecurityConfig points the config at a temp dir and installs sec.
func withSecurityConfig(t *testing.T, sec *config.Security) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	configMutex.Lock()
	prev := configStore
	configStore = &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Fox"}}, Security: sec}
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})
	_ = os.MkdirAll(dir, 0o700)
}

func TestRequireAuthGate(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	h := requireAuth(inner)
	get := func(path string, mod func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	withSecurityConfig(t, nil)
	if w := get("/api/status", nil); w.Code != 200 {
		t.Fatalf("no password: got %d, want open", w.Code)
	}

	hash, _ := hashPassword("hunter22")
	sec := &config.Security{PasswordHash: hash, SessionKey: randomHex(32), Tokens: []config.APIToken{}}
	tok := tokenPrefix + "abc"
	sum := sha256Hex(tok)
	sec.Tokens = append(sec.Tokens, config.APIToken{ID: "t1", Name: "ha", Hash: sum})
	withSecurityConfig(t, sec)

	if w := get("/api/status", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d", w.Code)
	}
	if w := get("/", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("page: got %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("/metrics", nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("metrics: got %d", w.Code)
	}
	for _, p := range []string{"/api/version", "/tailwind.css", "/fonts/inter-latin.woff2", "/login", "/api/auth/status"} {
		if w := get(p, nil); w.Code != 200 {
			t.Fatalf("%s must stay public, got %d", p, w.Code)
		}
	}
	if w := get("/api/status", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); w.Code != 200 {
		t.Fatalf("token: got %d", w.Code)
	}
	if w := get("/api/status", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ftk_wrong") }); w.Code != 401 {
		t.Fatalf("wrong token: got %d", w.Code)
	}
	opts := func(path string, preflight bool) int {
		r := httptest.NewRequest("OPTIONS", path, nil)
		if preflight {
			r.Header.Set("Access-Control-Request-Method", "POST")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := opts("/api/status", false); c != http.StatusUnauthorized {
		t.Fatalf("OPTIONS without a session must not reach a handler, got %d", c)
	}
	if c := opts("/api/status", true); c != http.StatusNoContent {
		t.Fatalf("a preflight to another endpoint gets an empty answer, got %d", c)
	}
	if c := opts("/api/control/Fox/pause", true); c != 200 {
		t.Fatalf("the FoxTrack web app's preflight reaches the control handler, got %d", c)
	}
	if w := get("/api/status", nil); w.Header().Get("X-Bridge-Auth") != "required" {
		t.Fatal("the dashboard tells its own sign-in 401 by the X-Bridge-Auth header")
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: newSessionValue(sec, time.Now().Add(time.Hour))}
	if w := get("/api/status", func(r *http.Request) { r.AddCookie(cookie) }); w.Code != 200 {
		t.Fatalf("session: got %d", w.Code)
	}
}

func TestPasswordLifecycleEndpoints(t *testing.T) {
	withSecurityConfig(t, nil)
	post := func(path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "10.1.1.1:5000"
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		requireAuth(http.DefaultServeMux).ServeHTTP(w, r)
		return w
	}
	if w := post("/api/security", map[string]string{"new": "short"}); w.Code != 400 {
		t.Fatalf("short password: got %d", w.Code)
	}
	w := post("/api/security", map[string]string{"new": "long enough pw"})
	if w.Code != 200 {
		t.Fatalf("set: got %d %s", w.Code, w.Body)
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("setting a password must keep this browser signed in")
	}
	// Feature blocks survive a Settings save through /api/config.
	configMutex.RLock()
	old := configStore
	configMutex.RUnlock()
	next, err := resolveConfigUpdate(old, []byte(`{"api_key":"x","security":null}`))
	if err != nil || next.Security == nil || next.Security.PasswordHash == "" {
		t.Fatalf("POST /api/config must never touch the password: %v", err)
	}
	if w := post("/api/security", map[string]string{"current": "nope", "new": "another pw 1"}, session); w.Code != http.StatusForbidden {
		t.Fatalf("wrong current: got %d", w.Code)
	}
	if w := post("/api/security", map[string]any{"current": "long enough pw", "remove": true}, session); w.Code != 200 {
		t.Fatalf("remove: got %d %s", w.Code, w.Body)
	}
	if securitySnapshot() != nil {
		t.Fatal("password still set after remove")
	}
	noteLogin("10.1.1.1", true, time.Now())
}

func TestTokenEndpoints(t *testing.T) {
	withSecurityConfig(t, nil)
	b := bytes.NewReader([]byte(`{"name":"Home Assistant"}`))
	r := httptest.NewRequest("POST", "/api/security/tokens", b)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleTokens(w, r)
	var out struct {
		Token string       `json:"token"`
		View  securityView `json:"view"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.Token, tokenPrefix) || len(out.View.Tokens) != 1 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), out.View.Tokens[0].Hint) && out.View.Tokens[0].Hint != out.Token[len(out.Token)-4:] {
		t.Fatal("hint is the last four characters")
	}
	if strings.Contains(currentSecurityViewJSON(t), out.Token) {
		t.Fatal("the token must never be shown again")
	}
	r = httptest.NewRequest("DELETE", "/api/security/tokens/"+out.View.Tokens[0].ID, nil)
	w = httptest.NewRecorder()
	handleTokens(w, r)
	if w.Code != 200 || len(currentSecurityView().Tokens) != 0 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
}

func currentSecurityViewJSON(t *testing.T) string {
	b, err := json.Marshal(currentSecurityView())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
