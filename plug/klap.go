package plug

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// --- TP-Link KLAP (Tapo, and Kasa on newer firmware), HTTP port 80 ---
//
// KLAP logs in with a hash of the TP-Link account email and password, which
// stays on the LAN: the plug checks it against the hash it got when it was
// set up, no cloud is called. Handshake 1 tells which hash the plug uses: v1
// (MD5) plugs speak Kasa's JSON, v2 (SHA) plugs speak Tapo's "method" JSON.
// Requests are AES-CBC with an IV and a SHA-256 signature derived from both
// seeds and a sequence number. Follows python-kasa's KlapTransport(V2).

var (
	errKlapLogin    = errors.New("The plug did not accept the email and password. Use the TP-Link account from the Tapo or Kasa app; both are case-sensitive.")
	errKlapNoKLAP   = errors.New("That address did not take a TP-Link login. Check the address, and that it is a Tapo or Kasa plug.")
	errKlapBadReply = errors.New("That address answers, but not like a TP-Link plug.")
	errKlapExpired  = errors.New("klap: session expired")
)

// klapDefaults are the logins a TP-Link plug accepts before it is bound to an
// account (python-kasa's DEFAULT_CREDENTIALS), plus a blank login.
var klapDefaults = [][2]string{
	{"kasa@tp-link.net", "kasaSetup"},
	{"test@tp-link.net", "test"},
	{"", ""},
}

type klapSession struct {
	mu      sync.Mutex
	host    string
	cookie  string
	key     []byte
	ivBase  []byte
	sig     []byte
	seq     int32
	smart   bool // a v2 plug: Tapo "method" requests
	expires time.Time
}

// tplinkSession is a logged-in TP-Link session: KLAP, or the older AES
// passthrough (tapo_aes.go). request takes and returns plain JSON.
type tplinkSession interface {
	request(ctx context.Context, plain []byte) ([]byte, error)
	isSmart() bool
	expiry() time.Time
}

func (s *klapSession) isSmart() bool     { return s.smart }
func (s *klapSession) expiry() time.Time { return s.expires }

// klapSessions holds one session per host and login, so a 10 s poll does not
// log in every time.
var klapSessions sync.Map

func klapAuthHash(v int, user, pass string) []byte {
	if v == 1 {
		u, p := md5.Sum([]byte(user)), md5.Sum([]byte(pass))
		h := md5.Sum(append(u[:], p[:]...))
		return h[:]
	}
	u, p := sha1.Sum([]byte(user)), sha1.Sum([]byte(pass))
	h := sha256.Sum256(append(u[:], p[:]...))
	return h[:]
}

func sha256Of(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// klapSeedHashes are what the plug sends back in handshake 1 and what the
// Bridge sends in handshake 2.
func klapSeedHashes(v int, local, remote, auth []byte) (h1, h2 []byte) {
	if v == 1 {
		return sha256Of(local, auth), sha256Of(remote, auth)
	}
	return sha256Of(local, remote, auth), sha256Of(remote, local, auth)
}

func newKlapSession(host string, local, remote, auth []byte, smart bool) *klapSession {
	iv := sha256Of([]byte("iv"), local, remote, auth)
	return &klapSession{
		host:   host,
		key:    sha256Of([]byte("lsk"), local, remote, auth)[:16],
		ivBase: iv[:12],
		seq:    int32(binary.BigEndian.Uint32(iv[28:])),
		sig:    sha256Of([]byte("ldk"), local, remote, auth)[:28],
		smart:  smart,
	}
}

func (s *klapSession) iv(seq int32) []byte {
	return binary.BigEndian.AppendUint32(append([]byte(nil), s.ivBase...), uint32(seq))
}

// encrypt returns signature + ciphertext for the next sequence number.
func (s *klapSession) encrypt(plain []byte) ([]byte, int32) {
	s.seq++
	b, _ := aes.NewCipher(s.key)
	n := 16 - len(plain)%16
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(b, s.iv(s.seq)).CryptBlocks(ct, padded)
	seqb := binary.BigEndian.AppendUint32(nil, uint32(s.seq))
	return append(sha256Of(s.sig, seqb, ct), ct...), s.seq
}

func (s *klapSession) decrypt(body []byte, seq int32) ([]byte, error) {
	if len(body) < 48 || (len(body)-32)%16 != 0 {
		return nil, errKlapBadReply
	}
	b, _ := aes.NewCipher(s.key)
	out := make([]byte, len(body)-32)
	cipher.NewCBCDecrypter(b, s.iv(seq)).CryptBlocks(out, body[32:])
	n := int(out[len(out)-1])
	if n == 0 || n > 16 || n > len(out) {
		return nil, errKlapBadReply
	}
	return out[:len(out)-n], nil
}

func klapPost(ctx context.Context, u string, body []byte, cookie string) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", errBadAddress
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if cookie != "" {
		req.Header.Set("Cookie", "TP_SESSIONID="+cookie)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, "", errNoAnswer
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	var newCookie string
	for _, c := range resp.Cookies() {
		if c.Name == "TP_SESSIONID" {
			newCookie = c.Value
		}
	}
	return resp.StatusCode, data, newCookie, nil
}

func klapBase(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "80")
	}
	return "http://" + host + "/app"
}

// klapLogin runs both handshakes with the user's login, then the defaults.
func klapLogin(ctx context.Context, host, user, pass string) (*klapSession, error) {
	base := klapBase(host)
	local := make([]byte, 16)
	_, _ = rand.Read(local)
	status, body, cookie, err := klapPost(ctx, base+"/handshake1", local, "")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, errKlapNoKLAP
	}
	if len(body) != 48 {
		return nil, errKlapBadReply
	}
	remote, server := body[:16], body[16:]
	logins := append([][2]string{{user, pass}}, klapDefaults...)
	for _, l := range logins {
		for _, v := range []int{2, 1} {
			auth := klapAuthHash(v, l[0], l[1])
			h1, h2 := klapSeedHashes(v, local, remote, auth)
			if !bytes.Equal(h1, server) {
				continue
			}
			status, _, _, err := klapPost(ctx, base+"/handshake2", h2, cookie)
			if err != nil {
				return nil, err
			}
			if status != 200 {
				return nil, errKlapLogin
			}
			s := newKlapSession(host, local, remote, auth, v == 2)
			s.cookie = cookie
			s.expires = time.Now().Add(time.Hour)
			return s, nil
		}
	}
	return nil, errKlapLogin
}

// request sends one encrypted JSON request and returns the decrypted reply.
func (s *klapSession) request(ctx context.Context, plain []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, seq := s.encrypt(plain)
	status, body, _, err := klapPost(ctx, klapBase(s.host)+"/request?seq="+strconv.Itoa(int(seq)), payload, s.cookie)
	if err != nil {
		return nil, err
	}
	if status == 403 {
		return nil, errKlapExpired
	}
	if status != 200 {
		return nil, fmt.Errorf("The TP-Link plug answered HTTP %d.", status)
	}
	return s.decrypt(body, seq)
}

// tplinkLogin tries KLAP, then the older AES login for a plug that does not
// offer KLAP.
func tplinkLogin(ctx context.Context, host, user, pass string) (tplinkSession, error) {
	s, err := klapLogin(ctx, host, user, pass)
	if errors.Is(err, errKlapNoKLAP) {
		return tapoAESLogin(ctx, host, user, pass)
	}
	return s, err
}

// klapDo runs fn on a cached session, logging in again once when the session
// is gone or has expired.
func klapDo(ctx context.Context, host string, g config.Plug, fn func(tplinkSession) error) error {
	user, pass := strings.TrimSpace(g.Username), g.Password
	// The password is in the key so a "Check plug" with a wrong one never
	// rides on a session the right one opened.
	key := fmt.Sprintf("%s\x00%s\x00%x", host, user, sha256.Sum256([]byte(pass)))
	for attempt := 0; attempt < 2; attempt++ {
		var s tplinkSession
		if v, ok := klapSessions.Load(key); ok && attempt == 0 && time.Now().Before(v.(tplinkSession).expiry()) {
			s = v.(tplinkSession)
		} else {
			var err error
			if s, err = tplinkLogin(ctx, host, user, pass); err != nil {
				klapSessions.Delete(key)
				return err
			}
			klapSessions.Store(key, s)
		}
		err := fn(s)
		if err == nil {
			return nil
		}
		klapSessions.Delete(key)
		if attempt == 1 || errors.Is(err, errKlapLogin) {
			return err
		}
	}
	return errNoAnswer
}

func klapRead(ctx context.Context, host string, g config.Plug) (Reading, error) {
	var r Reading
	err := klapDo(ctx, host, g, func(s tplinkSession) error {
		var err error
		if s.isSmart() {
			r, err = tapoRead(ctx, s)
		} else {
			var body []byte
			if body, err = s.request(ctx, []byte(kasaReadRequest)); err == nil {
				r, err = parseKasa(body)
			}
		}
		return err
	})
	return r, err
}

func klapSwitch(ctx context.Context, host string, g config.Plug, on bool) error {
	return klapDo(ctx, host, g, func(s tplinkSession) error {
		if s.isSmart() {
			_, err := tapoCall(ctx, s, "set_device_info", map[string]any{"device_on": on})
			return err
		}
		body, err := s.request(ctx, []byte(kasaSwitchRequest(on)))
		if err != nil {
			return err
		}
		return parseKasaSwitch(body)
	})
}

// Kasa's JSON is the same over TCP 9999 and over KLAP.
const kasaReadRequest = `{"system":{"get_sysinfo":{}},"emeter":{"get_realtime":{}}}`

func kasaSwitchRequest(on bool) string {
	state := 0
	if on {
		state = 1
	}
	return fmt.Sprintf(`{"system":{"set_relay_state":{"state":%d}}}`, state)
}

func parseKasaSwitch(body []byte) error {
	var v struct {
		System struct {
			Set *struct {
				ErrCode int `json:"err_code"`
			} `json:"set_relay_state"`
		} `json:"system"`
	}
	if json.Unmarshal(body, &v) != nil || v.System.Set == nil || v.System.Set.ErrCode != 0 {
		return errors.New("The Kasa plug refused the switch command.")
	}
	return nil
}

// --- Tapo "method" requests over a v2 session ---

var tapoTerminal = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(h[:])
}()

// tapoCall sends one method and returns its result. A non-zero error_code is
// an error, except where the caller reads it (see tapoRead).
func tapoCall(ctx context.Context, s tplinkSession, method string, params map[string]any) (json.RawMessage, error) {
	req := map[string]any{"method": method, "request_time_milis": time.Now().UnixMilli(), "terminal_uuid": tapoTerminal}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	body, err := s.request(ctx, b)
	if err != nil {
		return nil, err
	}
	var v struct {
		ErrorCode int             `json:"error_code"`
		Result    json.RawMessage `json:"result"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil, errKlapBadReply
	}
	if v.ErrorCode != 0 {
		return nil, fmt.Errorf("The Tapo plug refused %s (error %d).", method, v.ErrorCode)
	}
	return v.Result, nil
}

// tapoRead reads device_on, then the power: get_energy_usage's current_power
// is in milliwatts, get_current_power's (the fallback) in watts. A plug with
// no meter fails both, and Watts stays nil.
func tapoRead(ctx context.Context, s tplinkSession) (Reading, error) {
	res, err := tapoCall(ctx, s, "get_device_info", nil)
	if err != nil {
		return Reading{}, err
	}
	var info struct {
		DeviceOn *bool `json:"device_on"`
	}
	if json.Unmarshal(res, &info) != nil || info.DeviceOn == nil {
		return Reading{}, errKlapBadReply
	}
	r := Reading{On: *info.DeviceOn}
	var p struct {
		CurrentPower *float64 `json:"current_power"`
	}
	if res, err := tapoCall(ctx, s, "get_energy_usage", nil); err == nil && json.Unmarshal(res, &p) == nil && p.CurrentPower != nil {
		w := *p.CurrentPower / 1000
		r.Watts = &w
	} else if errors.Is(err, errKlapExpired) || errors.Is(err, errNoAnswer) {
		return Reading{}, err
	} else if res, err := tapoCall(ctx, s, "get_current_power", nil); err == nil && json.Unmarshal(res, &p) == nil && p.CurrentPower != nil {
		w := *p.CurrentPower
		r.Watts = &w
	}
	return r, nil
}
