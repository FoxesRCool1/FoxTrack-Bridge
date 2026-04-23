package lan

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

type Controller struct {
	mu        sync.RWMutex
	states    map[string]*mqttpkg.TelemetryData
	printers  map[string]configpkg.Printer
	cancelers map[string]chan struct{}
}

func NewController() *Controller {
	return &Controller{
		states:    map[string]*mqttpkg.TelemetryData{},
		printers:  map[string]configpkg.Printer{},
		cancelers: map[string]chan struct{}{},
	}
}

func (c *Controller) SyncPrinters(printers []configpkg.Printer, webhookURL, foxAPIKey string) {
	keep := make(map[string]bool)
	for _, p := range printers {
		if isBambuPrinter(p) || strings.TrimSpace(p.MoonrakerURL) == "" {
			continue
		}
		keep[p.Name] = true
		c.AddOrUpdatePrinter(p, webhookURL, foxAPIKey)
	}

	c.mu.Lock()
	for name, stop := range c.cancelers {
		if !keep[name] {
			close(stop)
			delete(c.cancelers, name)
			delete(c.printers, name)
			delete(c.states, name)
		}
	}
	c.mu.Unlock()
}

func (c *Controller) AddOrUpdatePrinter(p configpkg.Printer, webhookURL, foxAPIKey string) {
	if p.Name == "" {
		return
	}
	if isBambuPrinter(p) || strings.TrimSpace(p.MoonrakerURL) == "" {
		return
	}

	c.mu.Lock()
	if stop, ok := c.cancelers[p.Name]; ok {
		close(stop)
		delete(c.cancelers, p.Name)
	}
	stop := make(chan struct{})
	c.cancelers[p.Name] = stop
	c.printers[p.Name] = p
	c.mu.Unlock()

	go c.pollLoop(p, webhookURL, foxAPIKey, stop)
}

func (c *Controller) RemovePrinter(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stop, ok := c.cancelers[name]; ok {
		close(stop)
		delete(c.cancelers, name)
	}
	delete(c.printers, name)
	delete(c.states, name)
}

func (c *Controller) GetStates() map[string]*mqttpkg.TelemetryData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*mqttpkg.TelemetryData, len(c.states))
	for k, v := range c.states {
		copyV := *v
		out[k] = &copyV
	}
	return out
}

func (c *Controller) SendCommand(name, command string, args map[string]interface{}) error {
	c.mu.RLock()
	p, ok := c.printers[name]
	state := c.states[name]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("printer %q not found", name)
	}

	return c.sendKlipperCommand(p, state, command, args)
}

func (c *Controller) ProxyCamera(w http.ResponseWriter, _ *http.Request, name string) error {
	c.mu.RLock()
	p, ok := c.printers[name]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("printer not found")
	}

	candidates := cameraCandidates(p)
	client := &http.Client{Timeout: 8 * time.Second, Transport: insecureTransport()}
	for _, candidate := range candidates {
		req, err := http.NewRequest("GET", candidate, nil)
		if err != nil {
			continue
		}
		applyMoonrakerAuth(req, p)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			continue
		}

		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}

	return fmt.Errorf("camera unavailable")
}

func (c *Controller) pollLoop(p configpkg.Printer, webhookURL, foxAPIKey string, stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		default:
		}

		t, relayPayload, err := fetchKlipperTelemetry(p)
		if err != nil {
			t = mqttpkg.TelemetryData{Status: "disconnected", Error: err.Error()}
		}

		t.PrinterID = p.Name
		t.Timestamp = time.Now().Unix()

		c.mu.Lock()
		c.states[p.Name] = &t
		c.mu.Unlock()

		if webhookURL != "" && foxAPIKey != "" && err == nil {
			if err := webhook.SendRelay(foxAPIKey, webhookURL, p.MoonrakerURL, p.Name, relayPayload); err != nil {
				log.Printf("[%s] webhook error: %v", p.Name, err)
			}
		}

		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func shouldSendWebhook(prev, curr *mqttpkg.TelemetryData) bool {
	if prev == nil {
		return true
	}
	return prev.Status != curr.Status ||
		prev.FileName != curr.FileName ||
		prev.Progress != curr.Progress ||
		prev.Error != curr.Error ||
		int(prev.NozzleTemp) != int(curr.NozzleTemp) ||
		int(prev.BedTemp) != int(curr.BedTemp) ||
		prev.LightOn != curr.LightOn ||
		prev.TimeRemaining != curr.TimeRemaining
}

func sendJSONRequest(client *http.Client, method, u string, headers map[string]string, body io.Reader) (map[string]interface{}, error) {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fetchKlipperTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	u := moonrakerURL(p, "/printer/objects/query?print_stats&heater_bed&extruder&display_status&virtual_sdcard")
	m, err := sendJSONRequest(client, "GET", u, moonrakerAuthHeaders(p), nil)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}

	status := nestedMap(m, "result", "status")
	printStats := nestedMapAny(status, "print_stats")
	extruder := nestedMapAny(status, "extruder")
	bed := nestedMapAny(status, "heater_bed")
	displayStatus := nestedMapAny(status, "display_status")
	virtualSD := nestedMapAny(status, "virtual_sdcard")

	stateRaw := lowerString(stringAny(anyFromMap(printStats, "state")))
	state := mapKlipperState(stateRaw)
	fileName := stringAny(anyFromMap(printStats, "filename"))
	progressF := floatAny(anyFromMap(displayStatus, "progress"))
	if progressF == 0 {
		progressF = floatAny(anyFromMap(virtualSD, "progress"))
	}
	progress := int(progressF * 100)
	if progress > 100 {
		progress = 100
	}
	if progress < 0 {
		progress = 0
	}

	printDuration := int(floatAny(anyFromMap(printStats, "print_duration")))
	remaining := 0
	if progress > 0 && progress < 100 && printDuration > 0 {
		total := int(float64(printDuration) / (float64(progress) / 100.0))
		if total > printDuration {
			remaining = (total - printDuration) / 60
		}
	}

	relay := webhook.RelayPayload{
		Print: webhook.RelayPrint{
			GcodeState:         mapMoonrakerRelayState(stateRaw),
			SubTaskName:        fileName,
			McPercent:          progress,
			NozzleTemper:       floatAny(anyFromMap(extruder, "temperature")),
			NozzleTargetTemper: floatAny(anyFromMap(extruder, "target")),
			BedTemper:          floatAny(anyFromMap(bed, "temperature")),
			BedTargetTemper:    floatAny(anyFromMap(bed, "target")),
		},
	}

	return mqttpkg.TelemetryData{
		Status:        state,
		FileName:      fileName,
		Progress:      progress,
		NozzleTemp:    floatAny(anyFromMap(extruder, "temperature")),
		NozzleTarget:  floatAny(anyFromMap(extruder, "target")),
		BedTemp:       floatAny(anyFromMap(bed, "temperature")),
		BedTarget:     floatAny(anyFromMap(bed, "target")),
		TimeRemaining: remaining,
	}, relay, nil
}

func mapKlipperState(state string) string {
	switch state {
	case "printing":
		return "printing"
	case "paused":
		return "paused"
	case "complete", "completed":
		return "finished"
	case "error":
		return "error"
	case "standby", "ready":
		return "idle"
	default:
		if state == "" {
			return "connected"
		}
		return state
	}
}

func mapMoonrakerRelayState(state string) string {
	switch state {
	case "printing":
		return "RUNNING"
	case "paused":
		return "PAUSE"
	case "complete", "completed":
		return "FINISH"
	case "error":
		return "FAILED"
	case "standby", "ready":
		return "IDLE"
	default:
		if state == "" {
			return "IDLE"
		}
		return strings.ToUpper(state)
	}
}

func (c *Controller) sendKlipperCommand(p configpkg.Printer, state *mqttpkg.TelemetryData, command string, args map[string]interface{}) error {
	client := &http.Client{Timeout: 6 * time.Second}
	headers := moonrakerAuthHeaders(p)

	switch command {
	case "pause":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/pause"), headers, nil)
		return err
	case "resume":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/resume"), headers, nil)
		return err
	case "stop":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/cancel"), headers, nil)
		return err
	case "start":
		filename := getArgString(args, "file_name")
		if filename == "" {
			filename = getArgString(args, "file")
		}
		if filename == "" {
			return fmt.Errorf("start requires file_name")
		}
		startURL := moonrakerURL(p, "/printer/print/start?filename="+url.QueryEscape(filename))
		_, err := sendJSONRequest(client, "POST", startURL, headers, nil)
		return err
	case "light", "toggle_light", "light_on", "light_off":
		desiredOn := false
		hasDesired := false
		if command == "light_on" {
			desiredOn = true
			hasDesired = true
		}
		if command == "light_off" {
			hasDesired = true
		}
		if !hasDesired {
			if v, ok := args["on"].(bool); ok {
				desiredOn = v
				hasDesired = true
			}
		}
		if !hasDesired {
			desiredOn = !(state != nil && state.LightOn)
		}
		device, err := c.findKlipperLightDevice(client, p)
		if err != nil {
			return err
		}
		action := "off"
		if desiredOn {
			action = "on"
		}
		return setKlipperLight(client, p, device, action)
	default:
		return fmt.Errorf("unsupported command for klipper: %s", command)
	}
}

func (c *Controller) findKlipperLightDevice(client *http.Client, p configpkg.Printer) (string, error) {
	u := moonrakerURL(p, "/machine/device_power/devices")
	m, err := sendJSONRequest(client, "GET", u, moonrakerAuthHeaders(p), nil)
	if err != nil {
		return "", err
	}

	result, ok := m["result"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid device list")
	}
	devices, ok := result["devices"].([]interface{})
	if !ok || len(devices) == 0 {
		return "", fmt.Errorf("no controllable power devices found")
	}

	var fallback string
	for _, d := range devices {
		name := ""
		switch v := d.(type) {
		case string:
			name = v
		case map[string]interface{}:
			name = stringAny(v["device"])
			if name == "" {
				name = stringAny(v["name"])
			}
		}
		if name == "" {
			continue
		}
		if fallback == "" {
			fallback = name
		}
		lc := strings.ToLower(name)
		if strings.Contains(lc, "light") || strings.Contains(lc, "led") || strings.Contains(lc, "lamp") {
			return name, nil
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("no valid power device name found")
	}
	return fallback, nil
}

func setKlipperLight(client *http.Client, p configpkg.Printer, device, action string) error {
	headers := moonrakerAuthHeaders(p)
	queries := []string{
		moonrakerURL(p, "/machine/device_power/device?device="+url.QueryEscape(device)+"&action="+url.QueryEscape(action)),
		moonrakerURL(p, "/machine/device_power/set?device="+url.QueryEscape(device)+"&action="+url.QueryEscape(action)),
	}
	for _, q := range queries {
		_, err := sendJSONRequest(client, "POST", q, headers, nil)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("failed to toggle light device %q", device)
}

func cameraCandidates(p configpkg.Printer) []string {
	if isBambuPrinter(p) {
		return []string{fmt.Sprintf("https://%s:6000/mjpeg/1", p.IP)}
	}

	base := strings.TrimRight(p.MoonrakerURL, "/")
	out := []string{
		base + "/webcam/?action=stream",
		base + "/webcam/?action=snapshot",
		base + "/snapshot",
	}
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
		hostBase := u.Scheme + "://" + u.Host
		out = append(out,
			hostBase+"/webcam/?action=stream",
			hostBase+"/webcam/?action=snapshot",
			hostBase+"/snapshot",
		)
	}

	return out
}

func insecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

func getArgString(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func moonrakerURL(p configpkg.Printer, path string) string {
	base := strings.TrimSpace(strings.TrimRight(p.MoonrakerURL, "/"))
	if base == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return base + path
	}
	return base + "/" + path
}

func moonrakerAuthHeaders(p configpkg.Printer) map[string]string {
	headers := map[string]string{}
	if strings.TrimSpace(p.APIKey) != "" {
		headers["X-Api-Key"] = p.APIKey
	}
	return headers
}

func applyMoonrakerAuth(req *http.Request, p configpkg.Printer) {
	for k, v := range moonrakerAuthHeaders(p) {
		req.Header.Set(k, v)
	}
}

func isBambuPrinter(p configpkg.Printer) bool {
	if strings.TrimSpace(p.Serial) == "" {
		return false
	}
	if strings.TrimSpace(p.LANCode) == "" {
		return false
	}
	if strings.TrimSpace(p.MoonrakerURL) != "" {
		return false
	}
	return true
}

func nestedMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	cur := m
	for _, k := range keys {
		v, ok := cur[k]
		if !ok {
			return map[string]interface{}{}
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return map[string]interface{}{}
		}
		cur = next
	}
	return cur
}

func nestedMapAny(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	v, ok := m[key]
	if !ok {
		return map[string]interface{}{}
	}
	next, ok := v.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	return next
}

func anyFromMap(m map[string]interface{}, key string) interface{} {
	if m == nil {
		return nil
	}
	return m[key]
}

func stringAny(v interface{}) string {
	s, ok := v.(string)
	if ok {
		return s
	}
	return ""
}

func lowerString(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func floatAny(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}
