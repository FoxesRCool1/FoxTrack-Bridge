package main

import (
	"context"
	"crypto/tls"
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

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/config"
)

// POST /api/test checks a printer the way the Bridge will use it, so the
// dashboard can say exactly what is wrong before anything is saved. It never
// writes config and never touches the connections of printers already running.

// Result codes of a connection check.
const (
	probeOK          = "ok"
	probeUnreachable = "unreachable"
	probeAuth        = "auth"
	probeSerial      = "serial"
	probeNotFound    = "not_found"
	probeBadInput    = "bad_input"
	probeUnsupported = "unsupported"
)

const (
	// probeTimeout bounds one whole check.
	probeTimeout = 8 * time.Second
	// bambuPushall is the request mqtt.go sends to make a printer report.
	bambuPushall = `{"pushing": {"sequence_id": "0", "command": "pushall"}}`
)

// bambuProbePort is the printer's MQTT port. A variable so tests can point the
// probe at a fake broker.
var bambuProbePort = "8883"

// probeHTTP skips certificate checks like the pollers do (printers use
// self-signed certificates) and keeps no idle connections between checks.
var probeHTTP = &http.Client{
	Timeout:   probeTimeout,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
}

// probeResult is the response of POST /api/test. Reachable and Detail are the
// fields the older dashboard reads.
type probeResult struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail,omitempty"`
}

func probeResultOf(code, message, hint string) probeResult {
	// Every answer but "no answer" and "nothing to test" means the printer was reached.
	return probeResult{
		OK: code == probeOK || code == probeUnsupported, Code: code, Message: message, Hint: hint,
		Reachable: code != probeUnreachable && code != probeBadInput,
	}
}

func probeUnreachableAt(what, addr string, err error, hint string) probeResult {
	r := probeResultOf(probeUnreachable, fmt.Sprintf("Can't reach %s at %s.", what, addr), hint)
	r.Detail = err.Error()
	return r
}

// handleTest takes the body of POST /api/printers plus an optional "id". With an
// id, a blank secret is filled from that saved printer, as PUT /api/printers/{id}
// keeps it. The older dashboard sends only {"ip"} or one printer URL.
func handleTest(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	var req config.Printer
	var keys map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.Unmarshal(body, &keys) // the body just parsed as an object
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	json.NewEncoder(w).Encode(probePrinter(ctx, req, len(keys) == 1 && strings.TrimSpace(req.IP) != ""))
}

// probePrinter runs the check that fits the printer's type. reachOnly is the
// older dashboard's {"ip"}-only body: it asks for a reachability check of a
// Bambu printer, with no serial number or access code to test.
func probePrinter(ctx context.Context, p config.Printer, reachOnly bool) probeResult {
	if saved, ok := savedPrinter(p.ID); ok {
		keep := func(v *string, savedValue string) {
			if strings.TrimSpace(*v) == "" {
				*v = savedValue
			}
		}
		keep(&p.LANCode, saved.LANCode)
		keep(&p.APIKey, saved.APIKey)
		keep(&p.HomeAssistantToken, saved.HomeAssistantToken)
		if saved.IsCloud() {
			p.Connection = config.ConnectionCloud
		}
	}
	if p.IsCloud() {
		return probeResultOf(probeUnsupported, "Bambu Cloud printers connect through your Bambu account.", "")
	}
	switch p.Kind() {
	case config.KindKlipper:
		return probeMoonraker(ctx, p)
	case config.KindPrusaLink:
		return probePrusaLink(ctx, p)
	case config.KindHomeAssistant:
		return probeHomeAssistant(ctx, p)
	}
	return probeBambu(ctx, p, reachOnly)
}

// savedPrinter returns the saved printer with this ID.
func savedPrinter(id string) (config.Printer, bool) {
	if id == "" {
		return config.Printer{}, false
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore != nil {
		for _, p := range configStore.Printers {
			if p.ID == id {
				return p, true
			}
		}
	}
	return config.Printer{}, false
}

// probeBase trims an address the way the pollers do and says whether it names
// a scheme, which they need.
func probeBase(raw string) (string, bool) {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	lower := strings.ToLower(base)
	return base, strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func probeBadAddress(what string) probeResult {
	return probeResultOf(probeBadInput, fmt.Sprintf("The %s address needs to start with http:// or https://.", what), "")
}

// probeGet fetches base+path. A refused connection, a timeout and a server error
// come back as a finished unreachable result.
func probeGet(ctx context.Context, what, base, path, hint string, headers map[string]string) (int, []byte, *probeResult) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		r := probeBadAddress(what)
		return 0, nil, &r
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := probeHTTP.Do(req)
	if err == nil && resp.StatusCode >= 500 {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		r := probeUnreachableAt(what, base, err, hint)
		return 0, nil, &r
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, nil
}

func probeRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func probeMoonraker(ctx context.Context, p config.Printer) probeResult {
	base, ok := probeBase(p.MoonrakerURL)
	if !ok {
		return probeBadAddress("Moonraker")
	}
	headers := map[string]string{}
	if key := strings.TrimSpace(p.APIKey); key != "" {
		headers["X-Api-Key"] = key
	}
	status, body, fail := probeGet(ctx, "Moonraker", base, "/printer/info", "Moonraker usually listens on port 7125, e.g. http://192.168.1.60:7125", headers)
	if fail != nil {
		return *fail
	}
	if probeRefused(status) {
		return probeResultOf(probeAuth, "Moonraker refused the API key.", "Check the API key, or leave it blank if Moonraker trusts this computer's address.")
	}
	var info struct {
		Result *struct {
			State    string `json:"state"`
			Hostname string `json:"hostname"`
		} `json:"result"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &info) != nil || info.Result == nil {
		return probeResultOf(probeNotFound, "That address answers, but it isn't Moonraker.", "Moonraker usually listens on port 7125, e.g. http://192.168.1.60:7125")
	}
	name := info.Result.Hostname
	if name == "" {
		name = "Moonraker"
	}
	return probeResultOf(probeOK, fmt.Sprintf("Connected to %s (%s).", name, info.Result.State), "")
}

func probePrusaLink(ctx context.Context, p config.Printer) probeResult {
	base, ok := probeBase(p.PrusaLinkURL)
	if !ok {
		return probeBadAddress("PrusaLink")
	}
	headers := map[string]string{}
	if key := strings.TrimSpace(p.APIKey); key != "" {
		headers["X-Api-Key"] = key
	}
	status, body, fail := probeGet(ctx, "PrusaLink", base, "/api/version", "Check that the printer is on and PrusaLink is enabled, e.g. http://192.168.1.50", headers)
	if fail != nil {
		return *fail
	}
	if probeRefused(status) {
		return probeResultOf(probeAuth, "PrusaLink refused the API key.", "On the printer: Settings → Network → PrusaLink shows the API key.")
	}
	if status != http.StatusOK {
		return probeResultOf(probeNotFound, "That address answers, but it isn't PrusaLink.", "Check that the printer is on and PrusaLink is enabled, e.g. http://192.168.1.50")
	}
	// Some firmware serves /api/version without a key; the poller's endpoint does not.
	if st, _, fail := probeGet(ctx, "PrusaLink", base, "/api/v1/status", "", headers); fail != nil {
		return *fail
	} else if probeRefused(st) {
		return probeResultOf(probeAuth, "PrusaLink refused the API key.", "On the printer: Settings → Network → PrusaLink shows the API key.")
	}
	var info struct {
		Hostname string `json:"hostname"`
	}
	json.Unmarshal(body, &info)
	name := info.Hostname
	if name == "" {
		name = "the printer"
	}
	return probeResultOf(probeOK, fmt.Sprintf("Connected to %s.", name), "")
}

func probeHomeAssistant(ctx context.Context, p config.Printer) probeResult {
	const tokenHint = "Create a long-lived access token on your Home Assistant profile page (Security tab)."
	const addrHint = "Home Assistant usually listens on port 8123, e.g. http://homeassistant.local:8123"
	base, ok := probeBase(p.HomeAssistantURL)
	if !ok {
		return probeBadAddress("Home Assistant")
	}
	token, entity := strings.TrimSpace(p.HomeAssistantToken), strings.TrimSpace(p.HAStateEntity)
	if token == "" {
		return probeResultOf(probeBadInput, "The access token is missing.", tokenHint)
	}
	if entity == "" {
		return probeResultOf(probeBadInput, "The state entity is missing.", "Enter the entity that shows the printer's state, e.g. sensor.printer_status.")
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	refused := probeResultOf(probeAuth, "Home Assistant refused the token.", tokenHint)

	status, _, fail := probeGet(ctx, "Home Assistant", base, "/api/", addrHint, headers)
	if fail != nil {
		return *fail
	}
	if probeRefused(status) {
		return refused
	}
	if status != http.StatusOK {
		return probeResultOf(probeNotFound, "That address answers, but it isn't Home Assistant.", addrHint)
	}
	status, body, fail := probeGet(ctx, "Home Assistant", base, "/api/states/"+url.PathEscape(entity), addrHint, headers)
	if fail != nil {
		return *fail
	}
	var st struct {
		State string `json:"state"`
	}
	switch {
	case probeRefused(status):
		return refused
	case status == http.StatusOK && json.Unmarshal(body, &st) == nil:
		return probeResultOf(probeOK, fmt.Sprintf("Connected. %s is %s.", entity, st.State), "")
	}
	return probeResultOf(probeNotFound, fmt.Sprintf("Home Assistant has no entity %s.", entity), "Copy the entity ID from Home Assistant under Settings → Devices & services → Entities.")
}

func probeBambu(ctx context.Context, p config.Printer, reachOnly bool) probeResult {
	ip, serial, code := strings.TrimSpace(p.IP), strings.TrimSpace(p.Serial), strings.TrimSpace(p.LANCode)
	switch {
	case ip == "":
		return probeResultOf(probeBadInput, "The printer's IP address is missing.", "It is on the printer screen under Settings → WLAN (or Network).")
	case serial == "" && !reachOnly:
		return probeResultOf(probeBadInput, "The serial number is missing.", "It is on the printer screen under Settings → Device, or on the sticker on the back.")
	case code == "" && !reachOnly:
		return probeResultOf(probeBadInput, "The access code is missing.", "On the printer screen: Settings → WLAN (or Network) → Access Code.")
	}

	addr := net.JoinHostPort(ip, bambuProbePort)
	conn, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return probeUnreachableAt("the printer", ip, err, "Check that the printer is on and on the same network. The IP is on the printer screen under Settings → WLAN (or Network).")
	}
	conn.Close()
	if reachOnly {
		return probeResultOf(probeOK, fmt.Sprintf("The printer answered at %s.", ip), "Enter its serial number and access code to check those too.")
	}
	return probeBambuMQTT(addr, ip, serial, code)
}

// probeBambuMQTT logs in to the printer's broker as mqtt.go does, asks for a
// report and waits briefly for the answer.
func probeBambuMQTT(addr, ip, serial, code string) probeResult {
	deadline := time.Now().Add(probeTimeout)
	// wait caps a step at d, but never past the check's deadline (nor under half a second).
	wait := func(d time.Duration) time.Duration { return min(d, max(time.Until(deadline), 500*time.Millisecond)) }

	lost := make(chan struct{})
	var lostOnce sync.Once
	opts := paho.NewClientOptions().
		AddBroker("ssl://" + addr).
		SetUsername("bblp").
		SetPassword(code).
		SetClientID("foxtrack-bridge-test-" + config.NewPrinterID()[:8]).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}).
		SetProtocolVersion(4). // MQTT 3.1.1; unset, a refusal is retried as 3.1
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false).
		SetCleanSession(true).
		SetConnectionLostHandler(func(paho.Client, error) { lostOnce.Do(func() { close(lost) }) })
	client := paho.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(wait(6 * time.Second)) {
		return probeUnreachableAt("the printer", ip, errors.New("connect timed out"), "Check that the printer is on and on the same network.")
	}
	if err := token.Error(); err != nil {
		if errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) || errors.Is(err, packets.ErrorRefusedNotAuthorised) {
			return probeResultOf(probeAuth, "The printer rejected the access code.", "On the printer screen: Settings → WLAN (or Network) → Access Code. It changes when LAN mode is toggled.")
		}
		return probeUnreachableAt("the printer", ip, err, "Check that the printer is on and on the same network.")
	}
	defer client.Disconnect(250)

	badSerial := probeResultOf(probeSerial, fmt.Sprintf("The printer accepted the access code but not the serial number %s.", serial), "On the printer screen: Settings → Device, or the sticker on the back.")
	topic := "device/" + serial + "/report"
	got := make(chan struct{}, 1)
	sub := client.Subscribe(topic, 0, func(paho.Client, paho.Message) {
		select {
		case got <- struct{}{}:
		default:
		}
	})
	if !sub.WaitTimeout(wait(3*time.Second)) || sub.Error() != nil {
		return badSerial
	}
	if st, ok := sub.(*paho.SubscribeToken); ok && st.Result()[topic] == 0x80 {
		return badSerial // the broker refused the subscription
	}
	client.Publish("device/"+serial+"/request", 0, false, bambuPushall)

	timer := time.NewTimer(wait(4 * time.Second))
	defer timer.Stop()
	select {
	case <-got:
		return probeResultOf(probeOK, fmt.Sprintf("Connected to %s. The printer answered.", serial), "")
	case <-lost:
		return badSerial
	case <-timer.C:
		return probeResultOf(probeOK, "Connected. The access code works.", "The printer sent no report yet, so the serial number is not confirmed.")
	}
}

// probeURL is the address a connection test GETs for an HTTP printer, or ""
// for a Bambu printer. The assistant's connection-test tool uses it.
func probeURL(p config.Printer) string {
	trim := func(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }
	switch p.Kind() {
	case config.KindKlipper:
		return trim(p.MoonrakerURL) + "/printer/info"
	case config.KindPrusaLink:
		return trim(p.PrusaLinkURL) + "/api/version"
	case config.KindHomeAssistant:
		return trim(p.HomeAssistantURL) + "/api/"
	}
	return ""
}
