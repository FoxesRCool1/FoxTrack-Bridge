package plug

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// --- Tuya / Smart Life (TCP 6668, protocol 3.3, 3.4 or 3.5) ---
//
// A Tuya plug talks AES over the LAN with its local key, which only the Tuya
// cloud hands out: the user fetches it once (tinytuya wizard or the Tuya IoT
// platform) and the Bridge never calls the cloud itself. 3.3 is AES-ECB with a
// CRC frame; 3.4 adds a session key and HMAC-SHA256 frames; 3.5 swaps to
// AES-GCM in 0x6699 frames. Data point 1 is the relay, 19 the power in
// tenths of a watt. Tuya has no lifetime energy counter, so EnergyWh stays nil.

var (
	errTuyaNeedKey  = errors.New("A Tuya plug needs its device ID and local key. See the plug help for how to get them.")
	errTuyaKeyLen   = errors.New("The local key must be exactly 16 characters. Copy it again from the tinytuya wizard output.")
	errTuyaWrongKey = errors.New("The plug answered but did not accept the local key. The key changes each time the plug is re-paired in the Smart Life app: fetch it again.")
	errTuyaNoAnswer = errors.New("The plug did not answer on port 6668. Check the address, and close any other app (Home Assistant Local Tuya, tinytuya) holding a connection to it.")
	errTuyaBadReply = errors.New("The plug did not accept the request. Check the local key (it changes each time the plug is re-paired in the Smart Life app) and the protocol version.")
)

const (
	tuyaSessStart   = 3
	tuyaSessResp    = 4
	tuyaSessFinish  = 5
	tuyaControl     = 7
	tuyaStatus      = 8
	tuyaDPQuery     = 10
	tuyaControlNew  = 13
	tuyaDPQueryNew  = 16
	tuyaPrefix55AA  = 0x000055AA
	tuyaSuffix55AA  = 0x0000AA55
	tuyaPrefix6699  = 0x00006699
	tuyaSuffix6699  = 0x00009966
	tuyaMaxFrameLen = 64 << 10
)

// tuyaVersions is the auto-detect order: 3.3 is the most common plug.
var tuyaVersions = []string{"3.3", "3.4", "3.5"}

// tuyaDetected remembers the version auto-detect found, per device ID, so a
// poll does not retry the wrong versions every time.
var tuyaDetected sync.Map

type tuyaConn struct {
	c     net.Conn
	ver   string
	key   []byte // the local key, then the session key on 3.4 and 3.5
	devID string
	seq   uint32
}

func tuyaRead(ctx context.Context, host string, g config.Plug) (Reading, error) {
	var r Reading
	err := tuyaDo(ctx, host, g, func(t *tuyaConn) error {
		dps, err := t.query()
		if err != nil {
			return err
		}
		r, err = tuyaReading(dps)
		return err
	})
	return r, err
}

func tuyaSwitch(ctx context.Context, host string, g config.Plug, on bool) error {
	return tuyaDo(ctx, host, g, func(t *tuyaConn) error { return t.set(on) })
}

// tuyaDo opens a session with the configured version, or tries each version
// when none is set and keeps the first that works.
func tuyaDo(ctx context.Context, host string, g config.Plug, fn func(*tuyaConn) error) error {
	id, key := strings.TrimSpace(g.DeviceID), strings.TrimSpace(g.LocalKey)
	if id == "" || key == "" {
		return errTuyaNeedKey
	}
	if len(key) != 16 {
		return errTuyaKeyLen
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "6668")
	}
	versions := tuyaVersions
	if v := strings.TrimSpace(g.Version); v != "" {
		versions = []string{v}
	} else if v, ok := tuyaDetected.Load(id); ok {
		versions = []string{v.(string)}
	}
	// Auto-detect gives each version 1.5 s, so all three tries fit in the 5 s
	// poll even when a plug ignores a wrong version instead of hanging up.
	wait := requestTimeout
	if len(versions) > 1 {
		wait = 1500 * time.Millisecond
	}
	var firstErr error
	for _, v := range versions {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := tuyaSession(ctx, host, id, key, v, wait, fn)
		if err == nil {
			if g.Version == "" {
				tuyaDetected.Store(id, v)
			}
			return nil
		}
		if errors.Is(err, errTuyaWrongKey) || errors.Is(err, errTuyaNoAnswer) {
			return err // the same on every version
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	tuyaDetected.Delete(id)
	return firstErr
}

func tuyaSession(ctx context.Context, host, id, key, ver string, wait time.Duration, fn func(*tuyaConn) error) error {
	if ver != "3.3" && ver != "3.4" && ver != "3.5" {
		return fmt.Errorf("Tuya protocol version %q is not supported. Use 3.3, 3.4 or 3.5, or leave it on auto.", ver)
	}
	d := net.Dialer{Timeout: requestTimeout}
	c, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return errTuyaNoAnswer
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(wait))
	t := &tuyaConn{c: c, ver: ver, key: []byte(key), devID: id}
	if ver != "3.3" {
		if err := t.negotiate(); err != nil {
			return err
		}
	}
	return fn(t)
}

// --- framing ---

func (t *tuyaConn) send(cmd uint32, plain []byte) error {
	t.seq++
	var b []byte
	switch t.ver {
	case "3.5":
		iv := make([]byte, 12)
		_, _ = rand.Read(iv)
		b = make([]byte, 18, 18+12+len(plain)+16+4)
		binary.BigEndian.PutUint32(b[0:], tuyaPrefix6699)
		binary.BigEndian.PutUint32(b[6:], t.seq)
		binary.BigEndian.PutUint32(b[10:], cmd)
		binary.BigEndian.PutUint32(b[14:], uint32(12+len(plain)+16))
		gcm, err := newGCM(t.key)
		if err != nil {
			return err
		}
		b = append(b, iv...)
		b = gcm.Seal(b, iv, plain, b[4:18])
		b = binary.BigEndian.AppendUint32(b, tuyaSuffix6699)
	default:
		payload := ecbEncrypt(t.key, plain, true)
		if t.ver == "3.3" && cmd != tuyaDPQuery {
			payload = append(tuyaHeader("3.3"), payload...)
		}
		tail := 8
		if t.ver == "3.4" {
			tail = 36
		}
		b = make([]byte, 16, 16+len(payload)+tail)
		binary.BigEndian.PutUint32(b[0:], tuyaPrefix55AA)
		binary.BigEndian.PutUint32(b[4:], t.seq)
		binary.BigEndian.PutUint32(b[8:], cmd)
		binary.BigEndian.PutUint32(b[12:], uint32(len(payload)+tail))
		b = append(b, payload...)
		if t.ver == "3.4" {
			m := hmac.New(sha256.New, t.key)
			m.Write(b)
			b = m.Sum(b)
		} else {
			b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
		}
		b = binary.BigEndian.AppendUint32(b, tuyaSuffix55AA)
	}
	if _, err := t.c.Write(b); err != nil {
		return errTuyaNoAnswer
	}
	return nil
}

// recv reads one frame and returns its command and decrypted body, with the
// return code and version header removed. An empty body is an ack.
func (t *tuyaConn) recv() (uint32, []byte, error) {
	if t.ver == "3.5" {
		var h [18]byte
		if _, err := io.ReadFull(t.c, h[:]); err != nil {
			return 0, nil, tuyaReadErr(err)
		}
		if binary.BigEndian.Uint32(h[0:]) != tuyaPrefix6699 {
			return 0, nil, errTuyaBadReply
		}
		cmd, n := binary.BigEndian.Uint32(h[10:]), binary.BigEndian.Uint32(h[14:])
		if n < 28 || n > tuyaMaxFrameLen {
			return 0, nil, errTuyaBadReply
		}
		body := make([]byte, n+4)
		if _, err := io.ReadFull(t.c, body); err != nil {
			return 0, nil, tuyaReadErr(err)
		}
		gcm, err := newGCM(t.key)
		if err != nil {
			return 0, nil, err
		}
		plain, err := gcm.Open(nil, body[:12], body[12:n], h[4:])
		if err != nil {
			return 0, nil, errTuyaWrongKey
		}
		return cmd, stripTuyaHeader(stripRetcode(plain)), nil
	}
	var h [16]byte
	if _, err := io.ReadFull(t.c, h[:]); err != nil {
		return 0, nil, tuyaReadErr(err)
	}
	if binary.BigEndian.Uint32(h[0:]) != tuyaPrefix55AA {
		return 0, nil, errTuyaBadReply
	}
	cmd, n := binary.BigEndian.Uint32(h[8:]), binary.BigEndian.Uint32(h[12:])
	tail := uint32(8)
	if t.ver == "3.4" {
		tail = 36
	}
	if n < tail || n > tuyaMaxFrameLen {
		return 0, nil, errTuyaBadReply
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(t.c, body); err != nil {
		return 0, nil, tuyaReadErr(err)
	}
	payload := stripTuyaHeader(stripRetcode(body[:n-tail]))
	if len(payload) == 0 {
		return cmd, nil, nil
	}
	if len(payload)%16 == 15 {
		// Some 3.3 plugs send a version header that is not "3.3".
		payload = payload[15:]
	}
	if len(payload)%16 != 0 {
		// 3.3 sends some errors as plain text.
		return cmd, payload, nil
	}
	plain, ok := ecbDecrypt(t.key, payload)
	if !ok {
		return 0, nil, errTuyaWrongKey
	}
	return cmd, stripTuyaHeader(plain), nil
}

// tuyaReadErr: a plug hangs up on, or ignores, a frame it cannot read (a
// wrong key or version), so auto-detect goes on to the next version.
func tuyaReadErr(error) error { return errTuyaBadReply }

func tuyaHeader(ver string) []byte { return append([]byte(ver), make([]byte, 12)...) }

// stripRetcode drops the 4-byte return code a device puts before its payload.
func stripRetcode(b []byte) []byte {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 {
		return b[4:]
	}
	return b
}

// stripTuyaHeader drops a "3.x" + 12 bytes version header.
func stripTuyaHeader(b []byte) []byte {
	if len(b) >= 15 && b[0] == '3' && b[1] == '.' {
		return b[15:]
	}
	return b
}

// --- session key (3.4, 3.5) ---

func (t *tuyaConn) negotiate() error {
	local := make([]byte, 16)
	_, _ = rand.Read(local)
	if err := t.send(tuyaSessStart, local); err != nil {
		return err
	}
	var resp []byte
	for {
		cmd, body, err := t.recv()
		if err != nil {
			return err
		}
		if cmd == tuyaSessResp {
			resp = body
			break
		}
	}
	if len(resp) < 48 {
		return errTuyaBadReply
	}
	resp = resp[len(resp)-48:]
	remote := resp[:16]
	if !hmac.Equal(resp[16:48], hmacSHA256(t.key, local)) {
		return errTuyaWrongKey
	}
	if err := t.send(tuyaSessFinish, hmacSHA256(t.key, remote)); err != nil {
		return err
	}
	t.key = tuyaSessionKey(t.ver, t.key, local, remote)
	return nil
}

// tuyaSessionKey is the local key encrypting the XOR of both nonces: AES-ECB
// on 3.4, AES-GCM with the client nonce as IV on 3.5.
func tuyaSessionKey(ver string, key, local, remote []byte) []byte {
	mix := make([]byte, 16)
	for i := range mix {
		mix[i] = local[i] ^ remote[i]
	}
	if ver == "3.5" {
		gcm, _ := newGCM(key)
		return gcm.Seal(nil, local[:12], mix, nil)[:16]
	}
	return ecbEncrypt(key, mix, false)
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// --- commands ---

func (t *tuyaConn) query() (map[string]any, error) {
	if t.ver == "3.3" {
		now := strconv.FormatInt(time.Now().Unix(), 10)
		req, _ := json.Marshal(map[string]string{"gwId": t.devID, "devId": t.devID, "uid": t.devID, "t": now})
		if err := t.send(tuyaDPQuery, req); err != nil {
			return nil, err
		}
		dps, err := t.awaitDPS()
		if errors.Is(err, errTuyaDevice22) {
			// Some 3.3 plugs refuse DP_QUERY; they answer a CONTROL_NEW
			// asking for the points instead.
			req, _ = json.Marshal(map[string]any{"devId": t.devID, "uid": t.devID, "t": now,
				"dps": map[string]any{"1": nil, "19": nil}})
			if err := t.send(tuyaControlNew, req); err != nil {
				return nil, err
			}
			return t.awaitDPS()
		}
		return dps, err
	}
	if err := t.send(tuyaDPQueryNew, []byte("{}")); err != nil {
		return nil, err
	}
	return t.awaitDPS()
}

func (t *tuyaConn) set(on bool) error {
	now := time.Now().Unix()
	dps := map[string]bool{"1": on}
	if t.ver == "3.3" {
		req, _ := json.Marshal(map[string]any{"devId": t.devID, "uid": t.devID, "t": strconv.FormatInt(now, 10), "dps": dps})
		if err := t.send(tuyaControl, req); err != nil {
			return err
		}
	} else {
		req, _ := json.Marshal(map[string]any{"protocol": 5, "t": now, "data": map[string]any{"dps": dps}})
		if err := t.send(tuyaControlNew, append(tuyaHeader(t.ver), req...)); err != nil {
			return err
		}
	}
	for {
		cmd, body, err := t.recv()
		if err != nil {
			return err
		}
		if err := tuyaBodyErr(body); err != nil {
			return err
		}
		if isControlAck(cmd) {
			return nil
		}
	}
}

// isControlAck reports whether a frame command is the ack for a control
// request. Plain status pushes are not acks and must be skipped.
func isControlAck(cmd uint32) bool {
	return cmd == tuyaControl || cmd == tuyaControlNew
}

var errTuyaDevice22 = errors.New("tuya: data unvalid")

// awaitDPS reads frames until one carries data points, skipping acks.
func (t *tuyaConn) awaitDPS() (map[string]any, error) {
	for {
		_, body, err := t.recv()
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			continue
		}
		if err := tuyaBodyErr(body); err != nil {
			return nil, err
		}
		var v struct {
			DPS  map[string]any `json:"dps"`
			Data struct {
				DPS map[string]any `json:"dps"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &v) != nil {
			return nil, errTuyaWrongKey
		}
		if v.DPS != nil {
			return v.DPS, nil
		}
		if v.Data.DPS != nil {
			return v.Data.DPS, nil
		}
	}
}

func tuyaBodyErr(body []byte) error {
	if bytes.Contains(body, []byte("data unvalid")) {
		return errTuyaDevice22
	}
	if len(body) > 0 && body[0] != '{' {
		return errTuyaWrongKey
	}
	return nil
}

func tuyaReading(dps map[string]any) (Reading, error) {
	on, ok := dps["1"].(bool)
	if !ok {
		return Reading{}, errors.New("The plug answered, but has no on/off switch at data point 1. Multi-outlet strips and non-plug devices are not supported.")
	}
	r := Reading{On: on}
	if p, ok := dps["19"].(float64); ok {
		w := p / 10
		r.Watts = &w
	}
	return r, nil
}

// --- AES ---

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, errTuyaKeyLen
	}
	return cipher.NewGCM(b)
}

func ecbEncrypt(key, plain []byte, pad bool) []byte {
	b, _ := aes.NewCipher(key) // key length is checked in tuyaDo
	if pad {
		n := 16 - len(plain)%16
		plain = append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	}
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += 16 {
		b.Encrypt(out[i:i+16], plain[i:i+16])
	}
	return out
}

// ecbDecrypt reverses ecbEncrypt with padding. Like tinytuya it only checks
// the pad length; ok is false when that is out of range (a wrong key).
func ecbDecrypt(key, ct []byte) ([]byte, bool) {
	b, _ := aes.NewCipher(key)
	if len(ct) == 0 || len(ct)%16 != 0 {
		return nil, false
	}
	out := make([]byte, len(ct))
	for i := 0; i < len(ct); i += 16 {
		b.Decrypt(out[i:i+16], ct[i:i+16])
	}
	n := int(out[len(out)-1])
	if n == 0 || n > 16 {
		return nil, false
	}
	return out[:len(out)-n], true
}
