package plug

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// --- Meross (HTTP POST /config, signed JSON) ---
//
// Every message is signed with md5(messageId + key + timestamp), where key is
// the Meross account key. Without the key the plug answers error 5001 with a
// header it signed itself, and sending the request again with that header is
// accepted (meross_lan's "key reply" method), so the key is optional. Models
// that list Appliance.Encrypt.ECDHE take only their ability list in the
// clear; everything else is AES-256-CBC with a key made from the device ID,
// the Meross key and the MAC address. Follows meross_lan's merossclient.

var (
	errMerossBad     = errors.New("That address answers, but not like a Meross plug.")
	errMerossKey     = errors.New("The Meross plug did not accept the key. Leave the key blank, or copy it again (see the plug help).")
	errMerossNeedKey = errors.New("This Meross plug encrypts its local traffic, so it needs the Meross key (see the plug help).")
	errMerossNeedMAC = errors.New("This Meross plug encrypts its local traffic: enter its MAC address, from the label on the plug or the device info in the Meross app.")
)

const merossErrInvalidKey = 5001

// merossDevice is what the ability list says about one plug, cached per host.
type merossDevice struct {
	uuid        string
	encrypted   bool
	togglex     bool
	electricity bool
}

var merossDevices sync.Map

type merossHeader struct {
	MessageID      string `json:"messageId"`
	Namespace      string `json:"namespace"`
	Method         string `json:"method"`
	PayloadVersion int    `json:"payloadVersion"`
	From           string `json:"from"`
	TriggerSrc     string `json:"triggerSrc"`
	Timestamp      int64  `json:"timestamp"`
	TimestampMs    int64  `json:"timestampMs"`
	Sign           string `json:"sign"`
}

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// merossSign is md5(messageId + key + timestamp), as hex.
func merossSign(messageID, key string, ts int64) string {
	return md5Hex(messageID + key + strconv.FormatInt(ts, 10))
}

// pySlice is Python's s[a:b], which never panics.
func pySlice(s string, a, b int) string {
	if a > len(s) {
		return ""
	}
	if b > len(s) {
		b = len(s)
	}
	return s[a:b]
}

// merossCipherKey is md5hex(uuid[3:22] + key[1:9] + mac + key[10:28]), used
// as a 32-byte AES-256 key.
func merossCipherKey(uuid, key, mac string) []byte {
	return []byte(md5Hex(pySlice(uuid, 3, 22) + pySlice(key, 1, 9) + mac + pySlice(key, 10, 28)))
}

var merossIV = []byte("0000000000000000")

// merossEncrypt zero-pads (a full block when already aligned) and returns
// base64 of AES-CBC with the fixed ASCII-zero IV.
func merossEncrypt(key, plain []byte) []byte {
	b, _ := aes.NewCipher(key)
	p := append(append([]byte(nil), plain...), make([]byte, 16-len(plain)%16)...)
	cipher.NewCBCEncrypter(b, merossIV).CryptBlocks(p, p)
	return []byte(base64.StdEncoding.EncodeToString(p))
}

func merossDecrypt(key, body []byte) ([]byte, error) {
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil || len(ct) == 0 || len(ct)%16 != 0 {
		return nil, errMerossKey
	}
	b, _ := aes.NewCipher(key)
	cipher.NewCBCDecrypter(b, merossIV).CryptBlocks(ct, ct)
	return bytes.TrimRight(ct, "\x00"), nil
}

func merossHost(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		return net.JoinHostPort(host, "80")
	}
	return host
}

// merossCall sends one message and returns its payload. cipherKey is nil
// for a message sent in the clear.
func merossCall(ctx context.Context, host, key string, cipherKey []byte, ns, method string, payload any) (json.RawMessage, *merossHeader, error) {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	h := merossHeader{
		MessageID: hex.EncodeToString(id), Namespace: ns, Method: method, PayloadVersion: 1,
		From: "http://" + host + "/config", TriggerSrc: "FoxTrackBridge", Timestamp: time.Now().Unix(),
	}
	h.Sign = merossSign(h.MessageID, key, h.Timestamp)
	for attempt := 0; ; attempt++ {
		reply, err := merossPost(ctx, host, cipherKey, map[string]any{"header": h, "payload": payload})
		if err != nil {
			return nil, nil, err
		}
		var v struct {
			Header  merossHeader    `json:"header"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(reply, &v) != nil {
			return nil, nil, errMerossBad
		}
		if v.Header.Method != "ERROR" {
			return v.Payload, &v.Header, nil
		}
		var e struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(v.Payload, &e)
		if e.Error.Code != merossErrInvalidKey {
			return nil, nil, fmt.Errorf("The Meross plug refused %s (error %d).", ns, e.Error.Code)
		}
		if key != "" || attempt > 0 {
			return nil, nil, errMerossKey
		}
		// No key: send it again with the header the plug signed itself.
		h.MessageID, h.Timestamp, h.Sign = v.Header.MessageID, v.Header.Timestamp, v.Header.Sign
	}
}

func merossPost(ctx context.Context, host string, cipherKey []byte, msg any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, _ := json.Marshal(msg)
	ctype := "application/json"
	if cipherKey != nil {
		body, ctype = merossEncrypt(cipherKey, body), "application/octet-stream"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+host+"/config", bytes.NewReader(body))
	if err != nil {
		return nil, errBadAddress
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errNoAnswer
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if resp.StatusCode != 200 {
		return nil, errMerossBad
	}
	if cipherKey != nil {
		return merossDecrypt(cipherKey, data)
	}
	return data, nil
}

// merossDiscover reads the ability list (always in the clear) once per host.
func merossDiscover(ctx context.Context, host, key string) (*merossDevice, error) {
	if v, ok := merossDevices.Load(host); ok {
		return v.(*merossDevice), nil
	}
	p, h, err := merossCall(ctx, host, key, nil, "Appliance.System.Ability", "GET", map[string]any{})
	if err != nil {
		return nil, err
	}
	var a struct {
		Ability map[string]json.RawMessage `json:"ability"`
	}
	if json.Unmarshal(p, &a) != nil || a.Ability == nil {
		return nil, errMerossBad
	}
	d := &merossDevice{}
	// The reply's "from" is /appliance/<device id>/publish.
	if parts := strings.Split(h.From, "/"); len(parts) >= 3 {
		d.uuid = parts[2]
	}
	_, d.encrypted = a.Ability["Appliance.Encrypt.ECDHE"]
	_, d.togglex = a.Ability["Appliance.Control.ToggleX"]
	_, d.electricity = a.Ability["Appliance.Control.Electricity"]
	merossDevices.Store(host, d)
	return d, nil
}

// arpMAC finds the MAC of an IP in the Linux ARP table, which holds it once
// the Bridge has talked to the plug. Empty when not found (other systems,
// Docker bridge networking).
func arpMAC(ip string) string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) >= 4 && fields[0] == ip && fields[3] != "00:00:00:00:00:00" {
			return strings.ToLower(fields[3])
		}
	}
	return ""
}

// merossDo discovers the plug and calls fn with the cipher key (nil in the
// clear), rediscovering once if a cached plug stops answering.
func merossDo(ctx context.Context, host string, g config.Plug, fn func(d *merossDevice, key string, ck []byte) error) error {
	host = merossHost(host)
	key := strings.TrimSpace(g.LocalKey)
	for attempt := 0; attempt < 2; attempt++ {
		d, err := merossDiscover(ctx, host, key)
		if err != nil {
			return err
		}
		var ck []byte
		if d.encrypted {
			if key == "" {
				return errMerossNeedKey
			}
			mac := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(g.MAC), "-", ":"))
			if mac == "" {
				ip, _, _ := net.SplitHostPort(host)
				mac = arpMAC(ip)
			}
			if mac == "" {
				return errMerossNeedMAC
			}
			ck = merossCipherKey(d.uuid, key, mac)
		}
		err = fn(d, key, ck)
		if err == nil || attempt == 1 || errors.Is(err, errMerossKey) {
			return err
		}
		merossDevices.Delete(host)
	}
	return errNoAnswer
}

func merossRead(ctx context.Context, host string, g config.Plug) (Reading, error) {
	var r Reading
	err := merossDo(ctx, host, g, func(d *merossDevice, key string, ck []byte) error {
		p, _, err := merossCall(ctx, merossHost(host), key, ck, "Appliance.System.All", "GET", map[string]any{})
		if err != nil {
			return err
		}
		on, ok := parseMerossOnOff(p)
		if !ok {
			return errMerossBad
		}
		r = Reading{On: on}
		if d.electricity {
			if p, _, err := merossCall(ctx, merossHost(host), key, ck, "Appliance.Control.Electricity", "GET", map[string]any{}); err == nil {
				var e struct {
					Electricity struct {
						Power *float64 `json:"power"` // milliwatts
					} `json:"electricity"`
				}
				if json.Unmarshal(p, &e) == nil && e.Electricity.Power != nil {
					w := *e.Electricity.Power / 1000
					r.Watts = &w
				}
			}
		}
		return nil
	})
	return r, err
}

// parseMerossOnOff reads channel 0 from System.All: all.digest.togglex (a
// list, or one object), all.digest.toggle, or the older all.control.toggle.
func parseMerossOnOff(p json.RawMessage) (bool, bool) {
	var v struct {
		All struct {
			Digest struct {
				ToggleX json.RawMessage `json:"togglex"`
				Toggle  *struct {
					OnOff int `json:"onoff"`
				} `json:"toggle"`
			} `json:"digest"`
			Control struct {
				Toggle *struct {
					OnOff int `json:"onoff"`
				} `json:"toggle"`
			} `json:"control"`
		} `json:"all"`
	}
	if json.Unmarshal(p, &v) != nil {
		return false, false
	}
	type ch struct {
		Channel int  `json:"channel"`
		OnOff   *int `json:"onoff"`
	}
	var list []ch
	if json.Unmarshal(v.All.Digest.ToggleX, &list) != nil {
		var one ch
		if json.Unmarshal(v.All.Digest.ToggleX, &one) == nil {
			list = []ch{one}
		}
	}
	for _, c := range list {
		if c.Channel == 0 && c.OnOff != nil {
			return *c.OnOff == 1, true
		}
	}
	if t := v.All.Digest.Toggle; t != nil {
		return t.OnOff == 1, true
	}
	if t := v.All.Control.Toggle; t != nil {
		return t.OnOff == 1, true
	}
	return false, false
}

func merossSwitch(ctx context.Context, host string, g config.Plug, on bool) error {
	state := 0
	if on {
		state = 1
	}
	return merossDo(ctx, host, g, func(d *merossDevice, key string, ck []byte) error {
		ns, payload := "Appliance.Control.Toggle", map[string]any{"toggle": map[string]int{"onoff": state}}
		if d.togglex {
			ns, payload = "Appliance.Control.ToggleX", map[string]any{"togglex": map[string]int{"channel": 0, "onoff": state}}
		}
		_, _, err := merossCall(ctx, merossHost(host), key, ck, ns, "SET", payload)
		return err
	})
}
