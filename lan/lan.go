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

	"foxtrack-bridge/capture"
	configpkg "foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

type printSession struct {
	StartTime   int64
	FileName    string
	NozzleTemp  float64
	BedTemp     float64
	TempSamples []history.TempSample
	lastSampleT int64
}

type Controller struct {
	mu             sync.RWMutex
	states         map[string]*mqttpkg.TelemetryData
	printers       map[string]configpkg.Printer
	cancelers      map[string]chan struct{}
	sessionMu      sync.Mutex
	sessions       map[string]*printSession
	snapLastT      map[string]int64
	snapLastTMu    sync.Mutex
	snapInFlight   map[string]bool
	snapInFlightMu sync.Mutex
}

func NewController() *Controller {
	return &Controller{
		states:       map[string]*mqttpkg.TelemetryData{},
		printers:     map[string]configpkg.Printer{},
		cancelers:    map[string]chan struct{}{},
		sessions:     map[string]*printSession{},
		snapLastT:    map[string]int64{},
		snapInFlight: map[string]bool{},
	}
}

func (c *Controller) SyncPrinters(printers []configpkg.Printer, foxAPIKey, fox2APIKey string) {
	keep := make(map[string]bool)
	for _, p := range printers {
		if isBambuPrinter(p) || strings.TrimSpace(p.MoonrakerURL) == "" {
			continue
		}
		keep[p.Name] = true
		c.AddOrUpdatePrinter(p, foxAPIKey, fox2APIKey)
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

func (c *Controller) AddOrUpdatePrinter(p configpkg.Printer, foxAPIKey, fox2APIKey string) {
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

	go c.pollLoop(p, foxAPIKey, fox2APIKey, stop)
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
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}

	return fmt.Errorf("camera unavailable")
}

func (c *Controller) pollLoop(p configpkg.Printer, foxAPIKey, fox2APIKey string, stop <-chan struct{}) {
	timer := time.NewTimer(0) // fire immediately on first iteration
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[%s] poll panic: %v", p.Name, r)
					timer.Reset(15 * time.Second)
				}
			}()

			t, relayPayload, err := fetchKlipperTelemetry(p)
			if err != nil {
				t = mqttpkg.TelemetryData{Status: "disconnected", Error: err.Error()}
			}

			t.PrinterID = p.Name
			t.Timestamp = time.Now().Unix()

			// Capture previous state before overwriting, then detect print transitions.
			c.mu.Lock()
			prev := c.states[p.Name]
			prevStatus := ""
			if prev != nil {
				prevStatus = prev.Status
			}
			c.states[p.Name] = &t
			c.mu.Unlock()

			c.sessionMu.Lock()
			sess := c.sessions[p.Name]
			if prevStatus != "printing" && t.Status == "printing" {
				now := time.Now().Unix()
				c.sessions[p.Name] = &printSession{
					StartTime:   now,
					FileName:    t.FileName,
					NozzleTemp:  t.NozzleTemp,
					BedTemp:     t.BedTemp,
					TempSamples: []history.TempSample{{T: 0, NozzleTemp: t.NozzleTemp, BedTemp: t.BedTemp}},
					lastSampleT: now,
				}
				c.sessionMu.Unlock()
			} else if prevStatus == "printing" && t.Status != "printing" && sess != nil {
				now := time.Now().Unix()
				result := t.Status
				if result != "finished" && result != "error" {
					result = "cancelled"
				}
				rec := history.Record{
					PrinterName: p.Name,
					FileName:    sess.FileName,
					NozzleTemp:  sess.NozzleTemp,
					BedTemp:     sess.BedTemp,
					StartTime:   sess.StartTime,
					EndTime:     now,
					Duration:    now - sess.StartTime,
					Result:      result,
					TempSamples: sess.TempSamples,
				}
				delete(c.sessions, p.Name)
				c.sessionMu.Unlock()
				go func() {
					if err := history.Append(rec); err != nil {
						log.Printf("[%s] history write error: %v", p.Name, err)
					}
					if fox2APIKey != "" {
						webhook.SendHistory(fox2APIKey, webhook.HistoryURLV2, webhook.HistoryPayload{
							Type:        "print_complete",
							PrinterName: p.Name,
							Serial:      p.Name,
							FileName:    rec.FileName,
							NozzleTemp:  rec.NozzleTemp,
							BedTemp:     rec.BedTemp,
							StartTime:   time.Unix(rec.StartTime, 0).UTC().Format(time.RFC3339),
							EndTime:     time.Unix(rec.EndTime, 0).UTC().Format(time.RFC3339),
							Duration:    rec.Duration,
							Result:      rec.Result,
							Timestamp:   time.Now().UTC().Format(time.RFC3339),
						})
					}
				}()
			} else {
				// Ongoing print — sample temps once per minute.
				if t.Status == "printing" && sess != nil {
					if sess.FileName == "" && t.FileName != "" {
						sess.FileName = t.FileName
					}
					now := time.Now().Unix()
					if now-sess.lastSampleT >= 60 {
						sess.TempSamples = append(sess.TempSamples, history.TempSample{
							T:          now - sess.StartTime,
							NozzleTemp: t.NozzleTemp,
							BedTemp:    t.BedTemp,
						})
						sess.lastSampleT = now
					}
				}
				c.sessionMu.Unlock()
			}

			if err == nil && mqttpkg.ShouldSendWebhook(prev, &t) {
				if foxAPIKey != "" {
					if err := webhook.SendRelay(foxAPIKey, webhook.URL, p.Name, p.Name, relayPayload); err != nil {
						log.Printf("[%s] webhook error: %v", p.Name, err)
					}
				}
				if fox2APIKey != "" {
					if err := webhook.SendRelay(fox2APIKey, webhook.RelayURLV2, p.Name, p.Name, relayPayload); err != nil {
						log.Printf("[%s] webhook error (v2): %v", p.Name, err)
					}
				}
			}

			if fox2APIKey != "" && (t.Status == "printing" || t.Status == "paused") && strings.TrimSpace(p.WebcamURL) != "" {
				now := time.Now().Unix()
				c.snapLastTMu.Lock()
				eligible := now-c.snapLastT[p.Name] >= 25
				if eligible {
					c.snapLastT[p.Name] = now
				}
				c.snapLastTMu.Unlock()

				if eligible {
					c.snapInFlightMu.Lock()
					busy := c.snapInFlight[p.Name]
					if !busy {
						c.snapInFlight[p.Name] = true
					}
					c.snapInFlightMu.Unlock()

					if !busy {
						webcamURL := strings.TrimSpace(p.WebcamURL)
						name := p.Name
						apiKey := fox2APIKey
						go func() {
							defer func() {
								c.snapInFlightMu.Lock()
								delete(c.snapInFlight, name)
								c.snapInFlightMu.Unlock()
							}()
							frame, err := capture.KlipperFrame(webcamURL)
							if err != nil {
								log.Printf("[%s] snapshot capture: %v", name, err)
								return
							}
							if err := webhook.SendSnapshot(apiKey, webhook.SnapshotURLV2, name, name, frame); err != nil {
								log.Printf("[%s] snapshot send: %v", name, err)
							}
						}()
					}
				}
			}

			// Adaptive poll interval: faster while printing, slower when idle/disconnected.
			var interval time.Duration
			switch t.Status {
			case "printing":
				interval = 2 * time.Second
			case "paused":
				interval = 5 * time.Second
			case "disconnected":
				interval = 5 * time.Second
			default:
				interval = 8 * time.Second
			}
			timer.Reset(interval)
		}()

		select {
		case <-stop:
			return
		default:
		}
	}
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

// klipperHours caches the Moonraker lifetime print hours per printer.
// It is re-fetched at most once every 5 minutes to avoid per-poll overhead.
var (
	klipperHoursCache   = make(map[string]float64)
	klipperHoursUpdated = make(map[string]time.Time)
	klipperHoursMu      sync.Mutex
)

func klipperPrintHours(p configpkg.Printer) float64 {
	klipperHoursMu.Lock()
	if t, ok := klipperHoursUpdated[p.Name]; ok && time.Since(t) < 5*time.Minute {
		h := klipperHoursCache[p.Name]
		klipperHoursMu.Unlock()
		return h
	}
	klipperHoursMu.Unlock()

	client := &http.Client{Timeout: 3 * time.Second, Transport: insecureTransport()}
	m, err := sendJSONRequest(client, "GET", moonrakerURL(p, "/server/history/summary"), moonrakerAuthHeaders(p), nil)
	klipperHoursMu.Lock()
	defer klipperHoursMu.Unlock()
	klipperHoursUpdated[p.Name] = time.Now() // mark updated even on error to avoid retry every poll
	if err == nil {
		totals := nestedMap(m, "result", "job_totals")
		if secs := floatAny(anyFromMap(totals, "total_print_time")); secs > 0 {
			klipperHoursCache[p.Name] = secs / 3600
		}
	}
	return klipperHoursCache[p.Name]
}

func fetchKlipperTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	u := moonrakerURL(p, "/printer/objects/query?print_stats&heater_bed&extruder&extruder1&extruder2&extruder3&toolhead&fan&display_status&virtual_sdcard")
	m, err := sendJSONRequest(client, "GET", u, moonrakerAuthHeaders(p), nil)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}

	status := nestedMap(m, "result", "status")
	printStats := nestedMapAny(status, "print_stats")
	bed := nestedMapAny(status, "heater_bed")
	displayStatus := nestedMapAny(status, "display_status")
	virtualSD := nestedMapAny(status, "virtual_sdcard")

	// Determine active extruder. Klipper reports the current toolhead extruder name
	// (e.g. "extruder", "extruder1", "extruder2") via the toolhead object.
	toolhead := nestedMapAny(status, "toolhead")
	activeExtruderName := stringAny(anyFromMap(toolhead, "extruder"))
	if activeExtruderName == "" {
		activeExtruderName = "extruder"
	}
	extruder := nestedMapAny(status, activeExtruderName)
	if len(extruder) == 0 {
		extruder = nestedMapAny(status, "extruder")
		activeExtruderName = "extruder"
	}

	// Only expose active_extruder when it differs from the default single-extruder name.
	activeExtruderField := ""
	if activeExtruderName != "extruder" {
		activeExtruderField = activeExtruderName
	}

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

	fanSpeed := floatAny(anyFromMap(nestedMapAny(status, "fan"), "speed")) // 0.0-1.0
	coolingFanPct := int(fanSpeed * 100)
	filamentUsedMM := floatAny(anyFromMap(printStats, "filament_used"))

	relay := webhook.RelayPayload{
		Print: webhook.RelayPrint{
			GcodeState:         mapMoonrakerRelayState(stateRaw),
			SubTaskName:        fileName,
			McPercent:          progress,
			NozzleTemper:       floatAny(anyFromMap(extruder, "temperature")),
			NozzleTargetTemper: floatAny(anyFromMap(extruder, "target")),
			BedTemper:          floatAny(anyFromMap(bed, "temperature")),
			BedTargetTemper:    floatAny(anyFromMap(bed, "target")),
			McRemainingTime:    remaining,
			ActiveExtruder:     activeExtruderField,
		},
	}

	return mqttpkg.TelemetryData{
		Status:          state,
		FileName:        fileName,
		Progress:        progress,
		NozzleTemp:      floatAny(anyFromMap(extruder, "temperature")),
		NozzleTarget:    floatAny(anyFromMap(extruder, "target")),
		BedTemp:         floatAny(anyFromMap(bed, "temperature")),
		BedTarget:       floatAny(anyFromMap(bed, "target")),
		TimeRemaining:   remaining,
		ActiveExtruder:  activeExtruderField,
		CoolingFanPct:   coolingFanPct,
		FilamentUsedMM:  filamentUsedMM,
		TotalPrintHours: klipperPrintHours(p),
		PrinterModel:    "Klipper",
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
	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
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
	case "fan_speed":
		pct := 0
		if v, ok := args["pct"].(float64); ok {
			pct = int(v)
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		s := pct * 255 / 100
		script := fmt.Sprintf("M106 S%d", s)
		gcodeURL := moonrakerURL(p, "/printer/gcode/script?script="+url.QueryEscape(script))
		_, err := sendJSONRequest(client, "POST", gcodeURL, headers, nil)
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

	// If the user specified a webcam URL, try it first before guessing.
	if strings.TrimSpace(p.WebcamURL) != "" {
		return []string{strings.TrimSpace(p.WebcamURL)}
	}

	base := strings.TrimRight(p.MoonrakerURL, "/")
	out := []string{
		base + "/webcam/?action=stream",
		base + "/webcam/?action=snapshot",
		base + "/snapshot",
	}
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Hostname() != "" {
		// Derive the plain host (no port) to try port-80 nginx/crowsnest paths
		// and port-8080 mjpg-streamer paths, which are common in Mainsail/Fluidd setups.
		host := u.Scheme + "://" + u.Hostname()
		out = append(out,
			host+"/webcam/?action=stream",
			host+"/webcam/?action=snapshot",
			host+":8080/?action=stream",
			host+":8080/stream",
		)
	}

	return out
}

// sharedInsecureTransport is created once and reused by every Moonraker and
// webcam request. An http.Transport owns a connection pool, so building a fresh
// one per request meant every poll opened a new keep-alive connection that the
// discarded transport never closed — with a 2-8s poll interval those idle
// sockets accumulated for the life of the process until the bridge ran out of
// file descriptors. One shared transport reuses connections and, unlike a bare
// &http.Transport{}, retires idle ones on a timer. Transports are safe for
// concurrent use by design.
var sharedInsecureTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 4,
	IdleConnTimeout:     90 * time.Second,
}

func insecureTransport() *http.Transport {
	return sharedInsecureTransport
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
