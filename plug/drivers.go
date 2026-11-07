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
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// A plug is read and switched over the LAN only: Shelly and Tasmota over
// their HTTP APIs, Kasa over its TCP 9999 protocol, Tuya over TCP 6668. No maker's cloud is ever
// called.

// Reading is one plug poll. Watts and EnergyWh are nil for a plug without a
// meter. EnergyWh is the plug's lifetime counter, used as start and end marks.
type Reading struct {
	On       bool
	Watts    *float64
	EnergyWh *float64
}

var (
	errBadKind    = errors.New("Unknown plug type. Use shelly, tasmota, kasa, tapo, tuya, esphome, meross, sonoff, mystrom, wiz or home_assistant.")
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
	defer lockPlug(plugKey(g, host))()
	switch strings.ToLower(strings.TrimSpace(g.Kind)) {
	case config.PlugShelly:
		return shellyRead(ctx, host)
	case config.PlugTasmota:
		return tasmotaRead(ctx, host)
	case config.PlugKasa:
		return kasaRead(ctx, host, g)
	case config.PlugTapo:
		return klapRead(ctx, host, g)
	case config.PlugTuya:
		return tuyaRead(ctx, host, g)
	case config.PlugMyStrom:
		return mystromRead(ctx, host, g.Token)
	case config.PlugWiZ:
		return wizRead(ctx, host)
	case config.PlugSonoff:
		return sonoffRead(ctx, host)
	case config.PlugMeross:
		return merossRead(ctx, host, g)
	case config.PlugHA:
		return haPlugRead(ctx, g)
	case config.PlugESPHome:
		return esphomeRead(ctx, host, g)
	}
	return Reading{}, errBadKind
}

// Switch turns one plug on or off.
func Switch(ctx context.Context, g config.Plug, on bool) error {
	host, err := hostOf(g.Address)
	if err != nil {
		return err
	}
	defer lockPlug(plugKey(g, host))()
	switch strings.ToLower(strings.TrimSpace(g.Kind)) {
	case config.PlugShelly:
		return shellySwitch(ctx, host, on)
	case config.PlugTasmota:
		return tasmotaSwitch(ctx, host, on)
	case config.PlugKasa:
		return kasaSwitch(ctx, host, g, on)
	case config.PlugTapo:
		return klapSwitch(ctx, host, g, on)
	case config.PlugTuya:
		return tuyaSwitch(ctx, host, g, on)
	case config.PlugMyStrom:
		return mystromSwitch(ctx, host, g.Token, on)
	case config.PlugWiZ:
		return wizSwitch(ctx, host, on)
	case config.PlugSonoff:
		return sonoffSwitch(ctx, host, on)
	case config.PlugMeross:
		return merossSwitch(ctx, host, g, on)
	case config.PlugHA:
		return haPlugSwitch(ctx, g, on)
	case config.PlugESPHome:
		return esphomeSwitch(ctx, host, g, on)
	}
	return errBadKind
}

// plugLocks holds one *sync.Mutex per device key. A plug takes one TCP client
// or one KLAP session at a time, so Read, Switch and the poll loop must not overlap.
var plugLocks sync.Map

// lockPlug waits for the device's lock and returns the func that releases it.
func lockPlug(host string) func() {
	v, _ := plugLocks.LoadOrStore(host, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// plugKey is the lock key. HA plugs share one Home Assistant host but are
// independent of each other, so each is keyed by its own address.
func plugKey(g config.Plug, host string) string {
	if strings.ToLower(strings.TrimSpace(g.Kind)) == config.PlugHA {
		return g.Address
	}
	return host
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

// kasaKLAPHosts remembers the Kasa plugs that answered over KLAP.
var kasaKLAPHosts sync.Map

// kasaDo runs a Kasa request over whichever protocol the plug speaks: the old
// TCP 9999 one, or KLAP on newer firmware. KLAP goes first once it has worked
// for this host or when a login is set; the other is tried when the first
// gets no answer.
func kasaDo(host string, g config.Plug, legacy func() error, klap func(string) error) error {
	viaKLAP := func() error {
		err := klap(kasaKLAPHost(host))
		if err == nil {
			kasaKLAPHosts.Store(host, true)
		}
		return err
	}
	first, second := legacy, viaKLAP
	if _, ok := kasaKLAPHosts.Load(host); ok || strings.TrimSpace(g.Username) != "" {
		first, second = viaKLAP, legacy
	}
	wrongProtocol := func(err error) bool {
		return errors.Is(err, errNoAnswer) || errors.Is(err, errKlapNoKLAP) || errors.Is(err, errKlapBadReply)
	}
	err := first()
	if err == nil || !wrongProtocol(err) {
		return err
	}
	if err2 := second(); err2 == nil || !wrongProtocol(err2) {
		return err2
	}
	return errNoAnswer
}

func kasaRead(ctx context.Context, host string, g config.Plug) (Reading, error) {
	var r Reading
	err := kasaDo(host, g, func() error {
		body, err := kasaExchange(ctx, host, kasaReadRequest)
		if err == nil {
			r, err = parseKasa(body)
		}
		return err
	}, func(h string) (err error) {
		r, err = klapRead(ctx, h, g)
		return err
	})
	return r, err
}

// kasaKLAPHost drops a ":9999" the user typed for the old protocol.
func kasaKLAPHost(host string) string {
	if h, port, err := net.SplitHostPort(host); err == nil && port == "9999" {
		return h
	}
	return host
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

func kasaSwitch(ctx context.Context, host string, g config.Plug, on bool) error {
	return kasaDo(host, g, func() error {
		body, err := kasaExchange(ctx, host, kasaSwitchRequest(on))
		if err != nil {
			return err
		}
		return parseKasaSwitch(body)
	}, func(h string) error { return klapSwitch(ctx, h, g, on) })
}
