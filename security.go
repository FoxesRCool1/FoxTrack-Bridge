package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foxtrack-bridge/config"
)

// The dashboard password. Off by default (the Bridge stays open to the local
// network, as it always was); once set, every page and API call needs a
// signed session cookie or an API token, except the few listed in
// publicPaths. Sessions are stateless: an HMAC over the expiry and the
// password hash, so they survive restarts and a new password ends them all.

const (
	sessionCookie    = "ftb_session"
	sessionShort     = 12 * time.Hour
	sessionLong      = 30 * 24 * time.Hour
	pbkdf2Iterations = 600_000
	minPasswordLen   = 8
	tokenPrefix      = "ftk_"
)

// publicPaths answer without signing in: the sign-in page and what it loads,
// and the version (Docker's health check).
var publicPaths = map[string]bool{
	"/login":                true,
	"/api/auth/login":       true,
	"/api/auth/status":      true,
	"/api/auth/logout":      true,
	"/api/version":          true,
	"/logo.png":             true,
	"/logo-light.png":       true,
	"/logo-dark.png":        true,
	"/tailwind.css":         true,
	"/icons.css":            true,
	"/fonts.css":            true,
	"/favicon.ico":          true,
	"/manifest.webmanifest": true,
}

func isPublicPath(p string) bool {
	return publicPaths[p] || strings.HasPrefix(p, "/fonts/")
}

// securitySnapshot returns a copy of the security block, nil when no
// password is set.
func securitySnapshot() *config.Security {
	configMutex.RLock()
	defer configMutex.RUnlock()
	s := configStore.Security
	if s == nil || s.PasswordHash == "" {
		return nil
	}
	cp := *s
	cp.Tokens = append([]config.APIToken(nil), s.Tokens...)
	return &cp
}

// requireAuth wraps the whole mux. With no password set it changes nothing.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		sec := securitySnapshot()
		if sec != nil {
			// With a password, no other site may frame the dashboard (a
			// click-jacked Stop button) or learn its address. Without one the
			// dashboard stays embeddable, as in a Home Assistant iframe panel.
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
			h.Set("Referrer-Policy", "no-referrer")
		}
		if sec == nil || isPublicPath(r.URL.Path) || authorized(r, sec) {
			next.ServeHTTP(w, r)
			return
		}
		// A CORS preflight carries no credentials. Only the two endpoints the
		// FoxTrack web app calls answer it (their handlers return before doing
		// anything); every other OPTIONS needs a session like any request.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if strings.HasPrefix(r.URL.Path, "/api/control/") || strings.HasPrefix(r.URL.Path, "/api/camera/") {
				next.ServeHTTP(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Tells the dashboard this 401 is the Bridge's own sign-in, not an
		// error passed through from a printer or the Bambu cloud.
		h.Set("X-Bridge-Auth", "required")
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		case r.URL.Path == "/metrics":
			w.Header().Set("WWW-Authenticate", `Bearer realm="FoxTrack Bridge"`)
			http.Error(w, "This Bridge has a password. Send an API token as a bearer token.", http.StatusUnauthorized)
		default:
			http.Error(w, "Sign in to the Bridge first.", http.StatusUnauthorized)
		}
	})
}

// authorized accepts a valid session cookie or an API token.
func authorized(r *http.Request, sec *config.Security) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && validSession(c.Value, sec, time.Now()) {
		return true
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return tokenValid(strings.TrimSpace(tok), sec)
	}
	return false
}

func tokenValid(tok string, sec *config.Security) bool {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return false
	}
	want := sha256Hex(tok)
	ok := false
	for _, t := range sec.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// sessionMAC binds a session to its expiry and to the current password: a
// new password (or a new key) makes every earlier cookie invalid.
func sessionMAC(sec *config.Security, expiry int64) []byte {
	key, _ := hex.DecodeString(sec.SessionKey)
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "session|%d|%s", expiry, sec.PasswordHash)
	return m.Sum(nil)
}

func newSessionValue(sec *config.Security, expiry time.Time) string {
	b := make([]byte, 8, 8+sha256.Size)
	binary.BigEndian.PutUint64(b, uint64(expiry.Unix()))
	b = append(b, sessionMAC(sec, expiry.Unix())...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validSession(v string, sec *config.Security, now time.Time) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(b) != 8+sha256.Size || sec.SessionKey == "" {
		return false
	}
	expiry := int64(binary.BigEndian.Uint64(b[:8]))
	if now.Unix() >= expiry {
		return false
	}
	return hmac.Equal(b[8:], sessionMAC(sec, expiry))
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, sec *config.Security, remember bool) {
	life := sessionShort
	if remember {
		life = sessionLong
	}
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    newSessionValue(sec, time.Now().Add(life)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}
	if remember {
		c.MaxAge = int(life / time.Second)
	}
	http.SetCookie(w, c)
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// hashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<hash>".
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// hashSlots caps password checks running at once: each is deliberately
// slow, and a flood of sign-ins must not take every CPU from the printers.
var hashSlots = make(chan struct{}, 2)

func checkPassword(pw, stored string) bool {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Sign-in throttle: after 5 wrong passwords from one address, each further
// try waits out a lock that doubles from 30 s up to 15 minutes. A correct
// password clears it.
type loginTries struct {
	fails int
	until time.Time
}

var (
	loginMu   sync.Mutex
	loginByIP = map[string]*loginTries{}
)

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loginLockedFor(ip string, now time.Time) time.Duration {
	loginMu.Lock()
	defer loginMu.Unlock()
	if t := loginByIP[ip]; t != nil && now.Before(t.until) {
		return t.until.Sub(now)
	}
	return 0
}

// beginLogin is the check before a password is tried: it returns the wait
// when the address is locked, and otherwise counts the try as a failure
// right away, so parallel tries cannot all get past the lock while their
// slow hashes run. A correct password then clears it (noteLogin ok).
func beginLogin(ip string, now time.Time) time.Duration {
	loginMu.Lock()
	defer loginMu.Unlock()
	if t := loginByIP[ip]; t != nil && now.Before(t.until) {
		return t.until.Sub(now)
	}
	recordFailLocked(ip, now)
	return 0
}

func noteLogin(ip string, ok bool, now time.Time) {
	loginMu.Lock()
	defer loginMu.Unlock()
	if ok {
		delete(loginByIP, ip)
		return
	}
	recordFailLocked(ip, now)
}

// recordFailLocked counts one wrong password. The caller holds loginMu.
func recordFailLocked(ip string, now time.Time) {
	t := loginByIP[ip]
	if t == nil {
		t = &loginTries{}
		loginByIP[ip] = t
	}
	t.fails++
	if t.fails >= 5 {
		lock := 30 * time.Second << min(t.fails-5, 5)
		t.until = now.Add(min(lock, 15*time.Minute))
	}
	if len(loginByIP) > 1000 { // a scan of many addresses must not grow this forever
		for k, v := range loginByIP {
			if now.After(v.until) {
				delete(loginByIP, k)
			}
		}
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if !requireJSON(w, r) {
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "The request was not valid JSON.", http.StatusBadRequest)
		return false
	}
	return true
}

func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	sec := securitySnapshot()
	json.NewEncoder(w).Encode(map[string]bool{
		"password_set": sec != nil,
		"signed_in":    sec == nil || authorized(r, sec),
	})
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	sec := securitySnapshot()
	if sec == nil {
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	ip, now := clientIP(r), time.Now()
	if wait := beginLogin(ip, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(w, fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait)), http.StatusTooManyRequests)
		return
	}
	ok := checkPassword(req.Password, sec.PasswordHash)
	if ok {
		noteLogin(ip, true, now)
	}
	if !ok {
		log.Printf("[security] wrong dashboard password from %s", ip)
		http.Error(w, "That password is not right.", http.StatusUnauthorized)
		return
	}
	setSessionCookie(w, r, sec, req.Remember)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func roundWait(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())+1) + " seconds"
	}
	m := int(d.Minutes() + 0.99)
	if m == 1 {
		return "1 minute"
	}
	return strconv.Itoa(m) + " minutes"
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clearSessionCookie(w)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleLoginPage serves the sign-in page, or the dashboard when there is
// nothing to sign in to.
func handleLoginPage(w http.ResponseWriter, r *http.Request) {
	sec := securitySnapshot()
	if sec == nil || authorized(r, sec) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(loginHTML)
}

type securityView struct {
	PasswordSet bool        `json:"password_set"`
	Tokens      []tokenView `json:"tokens"`
}

type tokenView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hint      string `json:"hint"`
	CreatedAt int64  `json:"created_at"`
}

func currentSecurityView() securityView {
	configMutex.RLock()
	defer configMutex.RUnlock()
	v := securityView{Tokens: []tokenView{}}
	if s := configStore.Security; s != nil {
		v.PasswordSet = s.PasswordHash != ""
		for _, t := range s.Tokens {
			v.Tokens = append(v.Tokens, tokenView{ID: t.ID, Name: t.Name, Hint: t.Hint, CreatedAt: t.CreatedAt})
		}
	}
	return v
}

var errWrongCurrent = errors.New("The current password is not right.")

// handleSecurity: GET the view; POST {current, new} sets or changes the
// password; POST {current, remove: true} removes it.
func handleSecurity(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(currentSecurityView())
	case http.MethodPost:
		var req struct {
			Current string `json:"current"`
			New     string `json:"new"`
			Remove  bool   `json:"remove"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		ip, now := clientIP(r), time.Now()
		if wait := loginLockedFor(ip, now); wait > 0 {
			http.Error(w, fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait)), http.StatusTooManyRequests)
			return
		}
		if !req.Remove {
			if utf8.RuneCountInString(req.New) < minPasswordLen {
				http.Error(w, fmt.Sprintf("Use at least %d characters.", minPasswordLen), http.StatusBadRequest)
				return
			}
			if len(req.New) > 256 {
				http.Error(w, "That password is too long.", http.StatusBadRequest)
				return
			}
		}
		// The hashing is slow on purpose, so it runs outside the config lock;
		// the hash it checked against must still be the stored one.
		prevHash := ""
		if sec := securitySnapshot(); sec != nil {
			prevHash = sec.PasswordHash
			if wait := beginLogin(ip, now); wait > 0 {
				http.Error(w, fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait)), http.StatusTooManyRequests)
				return
			}
			if !checkPassword(req.Current, prevHash) {
				http.Error(w, errWrongCurrent.Error(), http.StatusForbidden)
				return
			}
		}
		var hash string
		if !req.Remove {
			h, err := hashPassword(req.New)
			if err != nil {
				http.Error(w, "Could not set the password: "+err.Error(), http.StatusInternalServerError)
				return
			}
			hash = h
		}
		var signIn *config.Security
		err := mutateConfig(func(c *config.Config) error {
			stored := ""
			if c.Security != nil {
				stored = c.Security.PasswordHash
			}
			if stored != prevHash {
				return errWrongCurrent // changed meanwhile: check again with the new one
			}
			if c.Security == nil {
				c.Security = &config.Security{}
			}
			if req.Remove {
				c.Security.PasswordHash, c.Security.SessionKey = "", ""
				return nil
			}
			c.Security.PasswordHash = hash
			c.Security.SessionKey = randomHex(32) // signs out every other session
			cp := *c.Security
			signIn = &cp
			return nil
		})
		if errors.Is(err, errWrongCurrent) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		noteLogin(ip, true, now)
		if signIn != nil {
			setSessionCookie(w, r, signIn, true) // keep this browser signed in
			log.Printf("[security] dashboard password set")
		} else {
			clearSessionCookie(w)
			log.Printf("[security] dashboard password removed")
		}
		json.NewEncoder(w).Encode(currentSecurityView())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleTokens: POST {name} creates a token and returns it once; DELETE
// /api/security/tokens/{id} revokes one.
func handleTokens(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	id := strings.TrimPrefix(r.URL.Path, "/api/security/tokens")
	id = strings.Trim(id, "/")
	switch {
	case r.Method == http.MethodPost && id == "":
		var req struct {
			Name string `json:"name"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "API token"
		}
		if utf8.RuneCountInString(name) > 60 {
			http.Error(w, "Keep the name under 60 characters.", http.StatusBadRequest)
			return
		}
		raw := make([]byte, 32)
		rand.Read(raw)
		tok := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
		t := config.APIToken{ID: randomHex(8), Name: name, Hash: sha256Hex(tok), Hint: tok[len(tok)-4:], CreatedAt: time.Now().Unix()}
		err := mutateConfig(func(c *config.Config) error {
			if c.Security == nil {
				c.Security = &config.Security{}
			}
			if len(c.Security.Tokens) >= 50 {
				return errors.New("There are already 50 tokens. Revoke one first.")
			}
			c.Security.Tokens = append(c.Security.Tokens, t)
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[security] API token %q created", name)
		json.NewEncoder(w).Encode(map[string]any{"token": tok, "view": currentSecurityView()})
	case r.Method == http.MethodDelete && id != "":
		found := false
		err := mutateConfig(func(c *config.Config) error {
			if c.Security == nil {
				return nil
			}
			kept := c.Security.Tokens[:0:0]
			for _, t := range c.Security.Tokens {
				if t.ID == id {
					found = true
					continue
				}
				kept = append(kept, t)
			}
			c.Security.Tokens = kept
			return nil
		})
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		if !found {
			http.Error(w, "No such token.", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(currentSecurityView())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// resetPasswordFromCLI removes the dashboard password from config.json, for
// someone locked out. API tokens are kept; they only work with a password.
func resetPasswordFromCLI() error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Security == nil || cfg.Security.PasswordHash == "" {
		fmt.Println("No dashboard password is set.")
		return nil
	}
	cfg.Security.PasswordHash, cfg.Security.SessionKey = "", ""
	if err := config.SaveConfig(cfg); err != nil {
		return err
	}
	fmt.Println("The dashboard password was removed. Start the Bridge again (stop it first if it is running) and set a new one in Settings, Security.")
	return nil
}

func init() {
	http.HandleFunc("/login", handleLoginPage)
	http.HandleFunc("/api/auth/status", handleAuthStatus)
	http.HandleFunc("/api/auth/login", handleAuthLogin)
	http.HandleFunc("/api/auth/logout", handleAuthLogout)
	http.HandleFunc("/api/security", handleSecurity)
	http.HandleFunc("/api/security/tokens", handleTokens)
	http.HandleFunc("/api/security/tokens/", handleTokens)
}
