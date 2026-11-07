package plug

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// --- Tapo on older firmware: RSA handshake, then AES "securePassthrough" ---
//
// Before KLAP, Tapo plugs took a 1024-bit RSA public key, sent back an AES
// key and IV encrypted with it, and then a login_device call with the
// TP-Link account (hashed as below) returned a token for every later request.
// Everything stays on the LAN. Follows python-kasa's AesTransport.

// tapoAESErrLogin is the device's error_code for a wrong login, and
// tapoAESErrSession the one for an expired session.
const (
	tapoAESErrLogin   = -1501
	tapoAESErrSession = 9999
)

type tapoAESSession struct {
	mu      sync.Mutex
	base    string // http://host/app
	cookie  string
	key, iv []byte
	token   string
	expires time.Time
}

func (s *tapoAESSession) isSmart() bool     { return true }
func (s *tapoAESSession) expiry() time.Time { return s.expires }

func (s *tapoAESSession) encrypt(b []byte) string {
	blk, _ := aes.NewCipher(s.key)
	n := 16 - len(b)%16
	p := append(append([]byte(nil), b...), bytes.Repeat([]byte{byte(n)}, n)...)
	cipher.NewCBCEncrypter(blk, s.iv).CryptBlocks(p, p)
	return base64.StdEncoding.EncodeToString(p)
}

func (s *tapoAESSession) decrypt(b64 string) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(ct) == 0 || len(ct)%16 != 0 {
		return nil, errKlapBadReply
	}
	blk, _ := aes.NewCipher(s.key)
	cipher.NewCBCDecrypter(blk, s.iv).CryptBlocks(ct, ct)
	n := int(ct[len(ct)-1])
	if n == 0 || n > 16 || n > len(ct) {
		return nil, errKlapBadReply
	}
	return ct[:len(ct)-n], nil
}

// tapoAESPost posts one JSON body to the /app endpoint and decodes the reply.
func tapoAESPost(ctx context.Context, u string, body any, cookie string) (map[string]json.RawMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return nil, "", errBadAddress
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("requestByApp", "true")
	if cookie != "" {
		req.Header.Set("Cookie", "TP_SESSIONID="+cookie)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", errNoAnswer
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		return nil, "", errKlapNoKLAP
	}
	var newCookie string
	for _, c := range resp.Cookies() {
		if c.Name == "TP_SESSIONID" || c.Name == "SESSIONID" {
			newCookie = c.Value
		}
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(data, &v) != nil {
		return nil, "", errKlapBadReply
	}
	return v, newCookie, nil
}

func tapoErrorCode(v map[string]json.RawMessage) int {
	var code int
	_ = json.Unmarshal(v["error_code"], &code)
	return code
}

// tapoNow is the clock for the login-failure cache; tests replace it.
var tapoNow = time.Now

// loginFailTTL is how long a rejected login is remembered, so a wrong password
// is not retried on every poll and the account is not locked out.
const loginFailTTL = 5 * time.Minute

type loginFail struct {
	err   error
	until time.Time
}

var (
	loginFailMu sync.Mutex
	loginFails  = map[string]loginFail{}
)

// tapoLoginKey identifies a login by host and a SHA-256 of the credentials, so
// the password itself is never kept in memory as a map key.
func tapoLoginKey(host, user, pass string) string {
	h := sha256.Sum256([]byte(user + "\x00" + pass))
	return host + "|" + hex.EncodeToString(h[:])
}

// cachedLoginFail returns the error of a recent rejected login, or nil.
func cachedLoginFail(key string) error {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	f, ok := loginFails[key]
	if !ok {
		return nil
	}
	if !tapoNow().Before(f.until) {
		delete(loginFails, key)
		return nil
	}
	return f.err
}

// rememberLoginFail stores a credential rejection for loginFailTTL.
func rememberLoginFail(key string, err error) {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	loginFails[key] = loginFail{err: err, until: tapoNow().Add(loginFailTTL)}
}

// forgetLoginFail drops any cached rejection once a login succeeds.
func forgetLoginFail(key string) {
	loginFailMu.Lock()
	defer loginFailMu.Unlock()
	delete(loginFails, key)
}

// tapoAESLogin runs the handshake, then login_device with the user's login
// (both login versions), then the default Tapo login.
func tapoAESLogin(ctx context.Context, host, user, pass string) (*tapoAESSession, error) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "80")
	}
	key := tapoLoginKey(host, user, pass)
	if err := cachedLoginFail(key); err != nil {
		return nil, err
	}
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pem := "-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n"
	s := &tapoAESSession{base: "http://" + host + "/app"}
	v, cookie, err := tapoAESPost(ctx, s.base, map[string]any{"method": "handshake", "params": map[string]string{"key": pem}}, "")
	if err != nil {
		return nil, err
	}
	if tapoErrorCode(v) != 0 {
		return nil, errKlapNoKLAP
	}
	var res struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(v["result"], &res)
	enc, err := base64.StdEncoding.DecodeString(res.Key)
	if err != nil {
		return nil, errKlapBadReply
	}
	keyIV, err := rsa.DecryptPKCS1v15(nil, priv, enc)
	if err != nil || len(keyIV) != 32 {
		return nil, errKlapBadReply
	}
	s.key, s.iv, s.cookie = keyIV[:16], keyIV[16:], cookie
	s.expires = time.Now().Add(time.Hour)

	for _, l := range [][2]string{{user, pass}, {"test@tp-link.net", "test"}} {
		for _, v2 := range []bool{false, true} {
			params := tapoAESLoginParams(l[0], l[1], v2)
			res, err := s.call(ctx, map[string]any{"method": "login_device", "params": params, "request_time_milis": time.Now().UnixMilli()})
			var code tapoCodeError
			if errors.As(err, &code) && (code == tapoAESErrLogin || code == 1111) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var tok struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(res, &tok) != nil || tok.Token == "" {
				return nil, errKlapBadReply
			}
			s.token = tok.Token
			forgetLoginFail(key)
			return s, nil
		}
	}
	rememberLoginFail(key, errKlapLogin)
	return nil, errKlapLogin
}

// tapoAESLoginParams hashes the login the way the plug expects: the username
// as base64 of its SHA-1 hex; the password as base64 of itself (login v1) or
// of its SHA-1 hex, sent as "password2" (login v2).
func tapoAESLoginParams(user, pass string, v2 bool) map[string]string {
	sha1Hex := func(x string) string { h := sha1.Sum([]byte(x)); return hex.EncodeToString(h[:]) }
	b64 := func(x string) string { return base64.StdEncoding.EncodeToString([]byte(x)) }
	if v2 {
		return map[string]string{"username": b64(sha1Hex(user)), "password2": b64(sha1Hex(pass))}
	}
	return map[string]string{"username": b64(sha1Hex(user)), "password": b64(pass)}
}

// tapoCodeError is a non-zero error_code inside the passthrough reply.
type tapoCodeError int

func (e tapoCodeError) Error() string {
	return fmt.Sprintf("The Tapo plug refused the request (error %d).", int(e))
}

// call sends one request through securePassthrough and returns its result.
func (s *tapoAESSession) call(ctx context.Context, req any) (json.RawMessage, error) {
	plain, _ := json.Marshal(req)
	out, err := s.request(ctx, plain)
	if err != nil {
		return nil, err
	}
	var v struct {
		ErrorCode int             `json:"error_code"`
		Result    json.RawMessage `json:"result"`
	}
	if json.Unmarshal(out, &v) != nil {
		return nil, errKlapBadReply
	}
	if v.ErrorCode != 0 {
		return nil, tapoCodeError(v.ErrorCode)
	}
	return v.Result, nil
}

// request wraps plain JSON in securePassthrough and returns the inner reply.
func (s *tapoAESSession) request(ctx context.Context, plain []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.base
	if s.token != "" {
		u += "?token=" + url.QueryEscape(s.token)
	}
	v, _, err := tapoAESPost(ctx, u, map[string]any{"method": "securePassthrough", "params": map[string]string{"request": s.encrypt(plain)}}, s.cookie)
	if err != nil {
		return nil, err
	}
	if code := tapoErrorCode(v); code != 0 {
		if code == tapoAESErrSession {
			return nil, errKlapExpired
		}
		return nil, tapoCodeError(code)
	}
	var res struct {
		Response string `json:"response"`
	}
	if json.Unmarshal(v["result"], &res) != nil {
		return nil, errKlapBadReply
	}
	out, err := s.decrypt(res.Response)
	if err != nil && json.Valid([]byte(res.Response)) {
		return []byte(res.Response), nil // some firmware answers in the clear
	}
	return out, err
}
