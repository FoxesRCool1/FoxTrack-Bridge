package cloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/version"
)

// Bambu Cloud REST client: sign-in and the device list. Everything here is
// paced. Bambu's API sits behind Cloudflare bot protection and Bambu bans
// accounts that hammer its servers, so this package refuses to send more than
// the limits below no matter how often the dashboard asks.

const (
	RegionGlobal = "global"
	RegionCN     = "cn"

	// loginInterval is the minimum gap between password sign-in attempts.
	loginInterval = 60 * time.Second
	// codeInterval is the minimum gap between attempts to redeem an emailed code.
	codeInterval = 10 * time.Second
	// cloudflareHold is how long every cloud call is refused after Cloudflare
	// blocks one: the challenge is tied to the public IP, clears on its own in
	// hours, and gets longer with every retry.
	cloudflareHold = 5 * time.Minute
	// bindInterval caches the device list so nothing can poll it.
	bindInterval = 60 * time.Second
	// tokenLifetime is the fallback expiry for tokens that carry no exp claim.
	// Bambu issues 90-day tokens; a day of margin avoids a failed connect.
	tokenLifetime = 89 * 24 * time.Hour

	requestTimeout = 20 * time.Second
)

var (
	ErrCodeRequired = errors.New("verification code required")
	ErrRateLimited  = errors.New("please wait a minute before trying again")
	ErrCloudflare   = errors.New("Bambu Cloud is blocking sign-in from this network for a while. Wait a few minutes and try again, or paste an access token instead")
	ErrUnauthorized = errors.New("Bambu Cloud rejected the access token; link the account again")
	ErrWrongCode    = errors.New("that code is wrong")
	ErrCodeExpired  = errors.New("that code has expired; sign in again to get a new one")
)

// Device is one printer bound to the Bambu account.
type Device struct {
	Serial      string `json:"serial"`
	Name        string `json:"name"`
	Model       string `json:"model,omitempty"`
	Online      bool   `json:"online"`
	PrintStatus string `json:"print_status,omitempty"`
	// AccessCode is the printer's LAN access code as reported by the cloud.
	// It is a device credential and is never serialized.
	AccessCode string `json:"-"`
}

// Session is a signed-in Bambu account.
type Session struct {
	Region    string
	Email     string
	Token     string
	Username  string // u_<uid>: the MQTT username
	IssuedAt  int64
	ExpiresAt int64
}

// Client talks to the Bambu Cloud REST API for one region.
type Client struct {
	Region  string
	BaseURL string // overridable for tests
	HTTP    *http.Client
}

// NewClient returns a client for region ("global" or "cn").
func NewClient(region string) *Client {
	return &Client{
		Region:  normalizeRegion(region),
		BaseURL: apiBase(region),
		HTTP:    &http.Client{Timeout: requestTimeout},
	}
}

func normalizeRegion(region string) string {
	if region == RegionCN {
		return RegionCN
	}
	return RegionGlobal
}

func apiBase(region string) string {
	if normalizeRegion(region) == RegionCN {
		return "https://api.bambulab.cn"
	}
	return "https://api.bambulab.com"
}

// Broker returns the MQTT broker URL for a region.
func Broker(region string) string {
	if normalizeRegion(region) == RegionCN {
		return "ssl://cn.mqtt.bambulab.com:8883"
	}
	return "ssl://us.mqtt.bambulab.com:8883"
}

// Pacing state. One account per bridge, and Cloudflare's limits are per public
// IP anyway, so the limiter is package-wide.
var (
	limitMu     sync.Mutex
	lastLoginAt time.Time
	lastCodeAt  time.Time
	holdUntil   time.Time
	lastBindAt  time.Time
	bindCache   []Device
	bindToken   string

	now = time.Now // swapped in tests
)

// ResetPacing clears the limiter and cache. Tests only.
func ResetPacing() {
	limitMu.Lock()
	defer limitMu.Unlock()
	lastLoginAt, lastCodeAt, holdUntil, lastBindAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	bindCache, bindToken = nil, ""
}

// HoldUntil reports when a Cloudflare hold ends (zero when none is active).
func HoldUntil() time.Time {
	limitMu.Lock()
	defer limitMu.Unlock()
	if now().Before(holdUntil) {
		return holdUntil
	}
	return time.Time{}
}

// takeSlot enforces one attempt per interval for the given stamp and refuses
// everything while a Cloudflare hold is active.
func takeSlot(stamp *time.Time, interval time.Duration) error {
	limitMu.Lock()
	defer limitMu.Unlock()
	t := now()
	if t.Before(holdUntil) {
		return ErrCloudflare
	}
	if !stamp.IsZero() && t.Sub(*stamp) < interval {
		return ErrRateLimited
	}
	*stamp = t
	return nil
}

func startHold() {
	limitMu.Lock()
	holdUntil = now().Add(cloudflareHold)
	limitMu.Unlock()
}

type loginResponse struct {
	AccessToken string `json:"accessToken"`
	ExpiresIn   int64  `json:"expiresIn"`
	LoginType   string `json:"loginType"` // "verifyCode" or "tfa" when a second step is needed
	TfaKey      string `json:"tfaKey"`
}

type apiEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// Login starts a password sign-in. It returns a Session when Bambu issues a
// token directly. When Bambu asks for a second step (the normal case since
// late 2024), it requests an emailed code and returns ErrCodeRequired; finish
// with LoginWithCode.
func (c *Client) Login(ctx context.Context, email, password string) (*Session, error) {
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return nil, errors.New("email and password are required")
	}
	if err := takeSlot(&lastLoginAt, loginInterval); err != nil {
		return nil, err
	}
	status, body, err := c.do(ctx, "POST", "/v1/user-service/user/login", "", map[string]string{
		"account":  email,
		"password": password,
		"apiError": "",
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError("sign-in", status, body)
	}
	var lr loginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, fmt.Errorf("sign-in: unexpected reply: %w", err)
	}
	if lr.AccessToken != "" {
		return c.buildSession(ctx, email, lr.AccessToken, lr.ExpiresIn)
	}
	// verifyCode, tfa, or an empty reply: the emailed code works for all of
	// them, and it is the only second step that has stayed reliable.
	if err := c.sendCode(ctx, email); err != nil {
		return nil, err
	}
	return nil, ErrCodeRequired
}

// sendCode asks Bambu to email a one-time sign-in code.
func (c *Client) sendCode(ctx context.Context, email string) error {
	status, body, err := c.do(ctx, "POST", "/v1/user-service/user/sendemail/code", "", map[string]string{
		"email": email,
		"type":  "codeLogin",
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return apiError("send code", status, body)
	}
	return nil
}

// LoginWithCode redeems the emailed code from Login.
func (c *Client) LoginWithCode(ctx context.Context, email, code string) (*Session, error) {
	email, code = strings.TrimSpace(email), strings.TrimSpace(code)
	if email == "" || code == "" {
		return nil, errors.New("email and code are required")
	}
	if err := takeSlot(&lastCodeAt, codeInterval); err != nil {
		return nil, err
	}
	status, body, err := c.do(ctx, "POST", "/v1/user-service/user/login", "", map[string]string{
		"account": email,
		"code":    code,
	})
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest {
		var env apiEnvelope
		_ = json.Unmarshal(body, &env)
		switch strings.Trim(string(env.Code), `"`) {
		case "1":
			return nil, ErrCodeExpired
		case "2":
			return nil, ErrWrongCode
		}
	}
	if status != http.StatusOK {
		return nil, apiError("sign-in", status, body)
	}
	var lr loginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, fmt.Errorf("sign-in: unexpected reply: %w", err)
	}
	if lr.AccessToken == "" {
		return nil, errors.New("sign-in: Bambu did not return a token")
	}
	return c.buildSession(ctx, email, lr.AccessToken, lr.ExpiresIn)
}

// SessionFromToken builds a session from a token the user pasted (the escape
// hatch when Cloudflare blocks sign-in). It makes one call to resolve the
// MQTT username unless the token is a JWT that already carries it.
func (c *Client) SessionFromToken(ctx context.Context, email, token string) (*Session, error) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	if token == "" {
		return nil, errors.New("token is required")
	}
	return c.buildSession(ctx, strings.TrimSpace(email), token, 0)
}

func (c *Client) buildSession(ctx context.Context, email, token string, expiresIn int64) (*Session, error) {
	issued := now()
	username, err := c.username(ctx, token)
	if err != nil {
		return nil, err
	}
	return &Session{
		Region:    c.Region,
		Email:     email,
		Token:     token,
		Username:  username,
		IssuedAt:  issued.Unix(),
		ExpiresAt: TokenExpiry(token, issued, expiresIn),
	}, nil
}

// username returns the MQTT username (u_<uid>) for token: from the JWT
// payload when there is one, else from the preference endpoint.
func (c *Client) username(ctx context.Context, token string) (string, error) {
	if claims, ok := jwtClaims(token); ok {
		if u, _ := claims["username"].(string); strings.HasPrefix(u, "u_") {
			return u, nil
		}
	}
	status, body, err := c.do(ctx, "GET", "/v1/design-user-service/my/preference", token, nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized {
		return "", ErrUnauthorized
	}
	if status != http.StatusOK {
		return "", apiError("profile", status, body)
	}
	var pref struct {
		UID json.Number `json:"uid"`
	}
	if err := json.Unmarshal(body, &pref); err != nil || pref.UID.String() == "" {
		return "", errors.New("profile: Bambu did not return a user id")
	}
	return "u_" + pref.UID.String(), nil
}

// Devices lists the printers bound to the account. Results are cached for
// bindInterval; force only bypasses a cache older than that, never the
// interval itself, so the dashboard cannot turn this into a poll.
func (c *Client) Devices(ctx context.Context, token string, force bool) ([]Device, error) {
	limitMu.Lock()
	t := now()
	if t.Before(holdUntil) {
		limitMu.Unlock()
		return nil, ErrCloudflare
	}
	fresh := bindToken == token && !lastBindAt.IsZero() && t.Sub(lastBindAt) < bindInterval
	if fresh || (!force && bindToken == token && bindCache != nil) {
		out := append([]Device(nil), bindCache...)
		limitMu.Unlock()
		return out, nil
	}
	lastBindAt = t
	limitMu.Unlock()

	status, body, err := c.do(ctx, "GET", "/v1/iot-service/api/user/bind", token, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if status != http.StatusOK {
		return nil, apiError("device list", status, body)
	}
	var resp struct {
		Devices []struct {
			DevID          string `json:"dev_id"`
			Name           string `json:"name"`
			Online         bool   `json:"online"`
			PrintStatus    string `json:"print_status"`
			DevModelName   string `json:"dev_model_name"`
			DevProductName string `json:"dev_product_name"`
			DevAccessCode  string `json:"dev_access_code"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("device list: unexpected reply: %w", err)
	}
	out := make([]Device, 0, len(resp.Devices))
	for _, d := range resp.Devices {
		model := d.DevProductName
		if model == "" {
			model = d.DevModelName
		}
		out = append(out, Device{
			Serial:      d.DevID,
			Name:        d.Name,
			Model:       model,
			Online:      d.Online,
			PrintStatus: d.PrintStatus,
			AccessCode:  d.DevAccessCode,
		})
	}
	limitMu.Lock()
	bindCache, bindToken = append([]Device(nil), out...), token
	limitMu.Unlock()
	return out, nil
}

// do sends one request and returns the status and body. It sends an honest
// User-Agent (never a slicer's identity) and turns a Cloudflare block into
// ErrCloudflare plus a hold.
func (c *Client) do(ctx context.Context, method, path, token string, payload interface{}) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "FoxTrack-Bridge/"+version.AppVersion)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("bambu cloud: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("bambu cloud: read: %w", err)
	}
	if isCloudflareBlock(resp, data) {
		startHold()
		return resp.StatusCode, data, ErrCloudflare
	}
	return resp.StatusCode, data, nil
}

// isCloudflareBlock recognises Cloudflare's challenge page: a 403 or 429 that
// is HTML rather than the API's JSON.
func isCloudflareBlock(resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return true
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "cloudflare") || strings.Contains(lower, "attention required")
}

func apiError(what string, status int, body []byte) error {
	var env apiEnvelope
	_ = json.Unmarshal(body, &env)
	msg := env.Message
	if msg == "" || msg == "success" {
		msg = env.Error
	}
	if msg == "" {
		return fmt.Errorf("%s: HTTP %d", what, status)
	}
	return fmt.Errorf("%s: HTTP %d: %s", what, status, msg)
}

// jwtClaims decodes the payload of a JWT without verifying it. Verification is
// Bambu's job; the bridge only reads the username and expiry it was handed.
func jwtClaims(token string) (map[string]interface{}, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

// TokenExpiry returns the unix time a token stops working: the JWT exp claim
// when present, else issued + expiresIn, else issued + tokenLifetime.
func TokenExpiry(token string, issued time.Time, expiresIn int64) int64 {
	if claims, ok := jwtClaims(token); ok {
		if exp, ok := claims["exp"].(float64); ok && exp > 0 {
			return int64(exp)
		}
	}
	if expiresIn > 0 {
		return issued.Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	return issued.Add(tokenLifetime).Unix()
}
