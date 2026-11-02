package plug

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"foxtrack-bridge/config"
)

// A plug is read and switched over the LAN only: Shelly and Tasmota over
// their HTTP APIs, Kasa over its TCP 9999 protocol. No maker's cloud is ever
// called.

// Reading is one plug poll. Watts and EnergyWh are nil for a plug without a
// meter. EnergyWh is the plug's lifetime counter, used as start and end marks.
type Reading struct {
	On       bool
	Watts    *float64
	EnergyWh *float64
}

var (
	errKasaKLAP   = errors.New("This Kasa plug uses newer firmware that the Bridge cannot talk to yet. Older Kasa plugs (HS110, KP115, EP25 on old firmware) work.")
	errBadKind    = errors.New("Unknown plug type. Use shelly, tasmota or kasa.")
	errBadAddress = errors.New("Enter the plug's address on your network, e.g. 192.168.1.80.")
	errNoAnswer   = errors.New("The plug did not answer. Check that it is on and on this network.")
)

const requestTimeout = 4 * time.Second

var httpClient = &http.Client{Timeout: requestTimeout}

// Read polls one plug.
func Read(ctx context.Context, g config.Plug) (Reading, error) {
	host, err := hostOf(g.Address)
	if err != nil {
		return Reading{}, err
	}
	switch strings.ToLower(strings.TrimSpace(g.Kind)) {
	case config.PlugShelly:
		return shellyRead(ctx, host)
	case config.PlugTasmota:
		return tasmotaRead(ctx, host)
	case config.PlugKasa:
		return kasaRead(ctx, host)
	}
	return Reading{}, errBadKind
}

// Switch turns one plug on or off.
func Switch(ctx context.Context, g config.Plug, on bool) error {
	host, err := hostOf(g.Address)
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(g.Kind)) {
	case config.PlugShelly:
		return shellySwitch(ctx, host, on)
	case config.PlugTasmota:
		return tasmotaSwitch(ctx, host, on)
	case config.PlugKasa:
		return kasaSwitch(ctx, host, on)
	}
	return errBadKind
}

// hostOf accepts "192.168.1.80", "192.168.1.80:80", "http://plug.local" and
// returns host[:port] with no scheme or path.
func hostOf(addr string) (string, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return "", errBadAddress
	}
	if strings.Contains(a, "://") {
		u, err := url.Parse(a)
		if err != nil || u.Host == "" {
			return "", errBadAddress
		}
		a = u.Host
	}
	a = strings.TrimSuffix(strings.Split(a, "/")[0], "/")
	if a == "" {
		return "", errBadAddress
	}
	return a, nil
}

func getJSON(ctx context.Context, u string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, nil
}

// --- Shelly (Gen2/Plus first, Gen1 as the fallback) ---

func shellyRead(ctx context.Context, host string) (Reading, error) {
	status, body, err := getJSON(ctx, "http://"+host+"/rpc/Switch.GetStatus?id=0")
	if err != nil {
		return Reading{}, err
	}
	if status == http.StatusOK {
		return parseShellyGen2(body)
	}
	status, body, err = getJSON(ctx, "http://"+host+"/status")
	if err != nil {
		return Reading{}, err
	}
	if status != http.StatusOK {
		return Reading{}, fmt.Errorf("Shelly answered HTTP %d. Check the address.", status)
	}
	return parseShellyGen1(body)
}

// parseShellyGen2 reads /rpc/Switch.GetStatus: output, apower (W), aenergy.total (Wh).
func parseShellyGen2(body []byte) (Reading, error) {
	var v struct {
		Output  *bool    `json:"output"`
		APower  *float64 `json:"apower"`
		AEnergy *struct {
			Total *float64 `json:"total"`
		} `json:"aenergy"`
	}
	if json.Unmarshal(body, &v) != nil || v.Output == nil {
		return Reading{}, errors.New("That address answers, but not like a Shelly plug.")
	}
	r := Reading{On: *v.Output, Watts: v.APower}
	if v.AEnergy != nil {
		r.EnergyWh = v.AEnergy.Total
	}
	return r, nil
}

// parseShellyGen1 reads /status: relays[0].ison, meters[0].power (W),
// meters[0].total (Watt-minutes, so divided by 60).
func parseShellyGen1(body []byte) (Reading, error) {
	var v struct {
		Relays []struct {
			IsOn bool `json:"ison"`
		} `json:"relays"`
		Meters []struct {
			Power *float64 `json:"power"`
			Total *float64 `json:"total"`
		} `json:"meters"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Relays) == 0 {
		return Reading{}, errors.New("That address answers, but not like a Shelly plug.")
	}
	r := Reading{On: v.Relays[0].IsOn}
	if len(v.Meters) > 0 {
		r.Watts = v.Meters[0].Power
		if v.Meters[0].Total != nil {
			wh := *v.Meters[0].Total / 60
			r.EnergyWh = &wh
		}
	}
	return r, nil
}

func shellySwitch(ctx context.Context, host string, on bool) error {
	status, _, err := getJSON(ctx, fmt.Sprintf("http://%s/rpc/Switch.Set?id=0&on=%t", host, on))
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	turn := "off"
	if on {
		turn = "on"
	}
	status, _, err = getJSON(ctx, "http://"+host+"/relay/0?turn="+turn)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Shelly answered HTTP %d.", status)
	}
	return nil
}

// --- Tasmota ---

func tasmotaRead(ctx context.Context, host string) (Reading, error) {
	status, body, err := getJSON(ctx, "http://"+host+"/cm?cmnd=Power")
	if err != nil {
		return Reading{}, err
	}
	if status != http.StatusOK {
		return Reading{}, fmt.Errorf("Tasmota answered HTTP %d. Check the address.", status)
	}
	on, ok := parseTasmotaPower(body)
	if !ok {
		return Reading{}, errors.New("That address answers, but not like a Tasmota plug.")
	}
	r := Reading{On: on}
	if st, b, err := getJSON(ctx, "http://"+host+"/cm?cmnd=Status%208"); err == nil && st == http.StatusOK {
		r.Watts, r.EnergyWh = parseTasmotaEnergy(b)
	}
	return r, nil
}

// parseTasmotaPower reads {"POWER":"ON"} (or POWER1 on multi-relay builds).
func parseTasmotaPower(body []byte) (bool, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return false, false
	}
	for _, k := range []string{"POWER", "POWER1"} {
		if s, ok := m[k].(string); ok {
			return strings.EqualFold(s, "ON"), true
		}
	}
	return false, false
}

// parseTasmotaEnergy reads Status 8: StatusSNS.ENERGY.Power (W) and Total (kWh).
func parseTasmotaEnergy(body []byte) (*float64, *float64) {
	var v struct {
		StatusSNS struct {
			Energy *struct {
				Power *float64 `json:"Power"`
				Total *float64 `json:"Total"`
			} `json:"ENERGY"`
		} `json:"StatusSNS"`
	}
	if json.Unmarshal(body, &v) != nil || v.StatusSNS.Energy == nil {
		return nil, nil
	}
	var wh *float64
	if v.StatusSNS.Energy.Total != nil {
		x := *v.StatusSNS.Energy.Total * 1000
		wh = &x
	}
	return v.StatusSNS.Energy.Power, wh
}

func tasmotaSwitch(ctx context.Context, host string, on bool) error {
	cmd := "Power%20Off"
	if on {
		cmd = "Power%20On"
	}
	status, _, err := getJSON(ctx, "http://"+host+"/cm?cmnd="+cmd)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Tasmota answered HTTP %d.", status)
	}
	return nil
}

// --- Kasa (TCP 9999, XOR "autokey" cipher) ---

const kasaKey = 171

// kasaEncrypt applies the autokey XOR and the 4-byte big-endian length prefix.
func kasaEncrypt(plain []byte) []byte {
	out := make([]byte, 4+len(plain))
	binary.BigEndian.PutUint32(out, uint32(len(plain)))
	key := byte(kasaKey)
	for i, b := range plain {
		key ^= b
		out[4+i] = key
	}
	return out
}

// kasaDecrypt reverses kasaEncrypt on a body without its length prefix.
func kasaDecrypt(cipher []byte) []byte {
	out := make([]byte, len(cipher))
	key := byte(kasaKey)
	for i, c := range cipher {
		out[i] = key ^ c
		key = c
	}
	return out
}

func kasaExchange(ctx context.Context, host string, request string) ([]byte, error) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "9999")
	}
	d := net.Dialer{Timeout: requestTimeout}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && strings.Contains(strings.ToLower(err.Error()), "refused") {
			return nil, errKasaKLAP
		}
		return nil, errNoAnswer
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	if _, err := conn.Write(kasaEncrypt([]byte(request))); err != nil {
		return nil, errNoAnswer
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, errNoAnswer
	}
	n := binary.BigEndian.Uint32(head[:])
	if n > 1<<20 {
		return nil, errors.New("The plug sent an answer that is too long.")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, errNoAnswer
	}
	return kasaDecrypt(body), nil
}

func kasaRead(ctx context.Context, host string) (Reading, error) {
	body, err := kasaExchange(ctx, host, `{"system":{"get_sysinfo":{}},"emeter":{"get_realtime":{}}}`)
	if err != nil {
		return Reading{}, err
	}
	return parseKasa(body)
}

// parseKasa reads get_sysinfo.relay_state and get_realtime (power_mw or
// power, total_wh or total in kWh). A plug without a meter answers an
// err_code on the emeter part: then Watts and EnergyWh stay nil.
func parseKasa(body []byte) (Reading, error) {
	var v struct {
		System struct {
			Info *struct {
				RelayState *int `json:"relay_state"`
			} `json:"get_sysinfo"`
		} `json:"system"`
		Emeter struct {
			Realtime *struct {
				PowerMW *float64 `json:"power_mw"`
				Power   *float64 `json:"power"`
				TotalWh *float64 `json:"total_wh"`
				Total   *float64 `json:"total"`
				ErrCode int      `json:"err_code"`
			} `json:"get_realtime"`
		} `json:"emeter"`
	}
	if json.Unmarshal(body, &v) != nil || v.System.Info == nil || v.System.Info.RelayState == nil {
		return Reading{}, errors.New("That address answers, but not like a Kasa plug.")
	}
	r := Reading{On: *v.System.Info.RelayState == 1}
	if rt := v.Emeter.Realtime; rt != nil && rt.ErrCode == 0 {
		switch {
		case rt.PowerMW != nil:
			w := *rt.PowerMW / 1000
			r.Watts = &w
		case rt.Power != nil:
			r.Watts = rt.Power
		}
		switch {
		case rt.TotalWh != nil:
			r.EnergyWh = rt.TotalWh
		case rt.Total != nil:
			wh := *rt.Total * 1000
			r.EnergyWh = &wh
		}
	}
	return r, nil
}

func kasaSwitch(ctx context.Context, host string, on bool) error {
	state := 0
	if on {
		state = 1
	}
	body, err := kasaExchange(ctx, host, fmt.Sprintf(`{"system":{"set_relay_state":{"state":%d}}}`, state))
	if err != nil {
		return err
	}
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
