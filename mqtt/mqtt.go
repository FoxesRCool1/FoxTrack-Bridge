package mqtt

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"foxtrack-bridge/capture"
	"foxtrack-bridge/history"
	"foxtrack-bridge/webhook"
)

// flexInt unmarshals a JSON value that may arrive as either a number or a numeric string.
// BambuLab firmware sends cooling_fan_speed as an int in telemetry but as a string in
// upgrade_state messages, causing parse failures with a plain int field.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexInt(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*f = 0
		return nil
	}
	n64, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt(n64)
	return nil
}

type Printer struct {
	Name            string
	IP              string
	Serial          string
	LANCode         string
	APIKey          string
	FoxTrack2APIKey string
}

// AmsSlot holds the state of a single AMS filament tray.
type AmsSlot struct {
	Slot      int    `json:"slot"`               // 0-indexed tray position
	Color     string `json:"color"`              // 6-char hex, e.g. "FF0000"
	Material  string `json:"material"`           // e.g. "PLA", "PETG"
	Active    bool   `json:"active"`             // currently printing from this slot
	Remaining int    `json:"remaining,omitempty"` // 0-100% filament remaining; 0 means not reported or empty
}

type TelemetryData struct {
	Status         string    `json:"status"`
	FileName       string    `json:"file_name"`
	Progress       int       `json:"progress"`
	Error          string    `json:"error"`
	PrinterID      string    `json:"printer_id"`
	Timestamp      int64     `json:"timestamp"`
	NozzleTemp     float64   `json:"nozzle_temp"`
	NozzleTarget   float64   `json:"nozzle_target"`
	BedTemp        float64   `json:"bed_temp"`
	BedTarget      float64   `json:"bed_target"`
	LightOn        bool      `json:"light_on"`
	TimeRemaining  int       `json:"time_remaining"`            // minutes
	SpeedLevel     int       `json:"speed_level"`               // 1=Silent 2=Standard 3=Sport 4=Ludicrous
	AMS            []AmsSlot `json:"ams,omitempty"`             // nil when no AMS
	ActiveExtruder  string  `json:"active_extruder,omitempty"`   // non-default active extruder for multi-toolhead printers
	CoolingFanPct   int     `json:"cooling_fan_pct,omitempty"`   // 0-100, absent when not reported
	FilamentUsedMM  float64 `json:"filament_used_mm,omitempty"`
	TotalPrintHours float64 `json:"total_print_hours,omitempty"` // lifetime hours: from printer for Bambu/Klipper, Bridge-tracked otherwise
	PrinterModel    string  `json:"printer_model,omitempty"`     // e.g. "X1 Carbon", "P1S", "Klipper"
}

type BambuReport struct {
	Print BambuPrint `json:"print"`
}

type BambuPrint struct {
	Msg                int     `json:"msg"`
	GcodeState         string  `json:"gcode_state"`
	SubTaskName        string  `json:"subtask_name"`
	McPercent          int     `json:"mc_percent"`
	McPrintErrorCode   string  `json:"mc_print_error_code"`
	NozzleTemper       float64 `json:"nozzle_temper"`
	NozzleTargetTemper float64 `json:"nozzle_target_temper"`
	BedTemper          float64 `json:"bed_temper"`
	BedTargetTemper    float64 `json:"bed_target_temper"`
	Lights             []struct {
		Node string `json:"node"`
		Mode string `json:"mode"`
	} `json:"lights_report"`
	McRemainingTime    int     `json:"mc_remaining_time"`
	SpdLvl             int     `json:"spd_lvl"`
	AmsStatus          int     `json:"ams_status"`
	CoolingFanSpeed    *flexInt `json:"cooling_fan_speed"`   // raw 0-15 value; nil when field absent (incremental update), 0 when fan is off
	TotalRunTimeSec    int64   `json:"total_run_time"`       // lifetime seconds on the printer; only present in pushall responses
	MachineType        string  `json:"machine_type"`         // model identifier, e.g. "X1C", "P1S"; only present in pushall responses
	Ams             *struct {
		AMS []struct {
			Tray []struct {
				ID       string `json:"id"`
				Color    string `json:"tray_color"` // 8-char RRGGBBAA
				Material string `json:"tray_type"`
				Remain   int    `json:"remain"`     // 0-100%; "remain" is training-data knowledge of Bambu's field name, unverified this session
			} `json:"tray"`
		} `json:"ams"`
		TrayNow string `json:"tray_now"` // currently loaded tray ("0"-"3"), "255" = none/external
	} `json:"ams"`
}

var (
	printerStates  = make(map[string]*TelemetryData)
	printerClients = make(map[string]mqtt.Client) // for sending control commands
	stateMutex     sync.RWMutex
	clientMutex    sync.RWMutex
	sequenceID     uint64
	sequenceMu     sync.Mutex

	// managedPrinters tracks the active connection goroutine per printer name.
	// Prevents duplicate goroutines when ConnectPrinter is called redundantly
	// and lets DisconnectPrinter stop a goroutine and wait for it to exit.
	managedPrinters   = make(map[string]*managedConn)
	managedPrintersMu sync.Mutex

	// snapshot throttle: last capture time and in-flight guard per printer.
	snapLastT      = make(map[string]int64)
	snapLastTMu    sync.Mutex
	snapInFlight   = make(map[string]bool)
	snapInFlightMu sync.Mutex

	// relay webhook dedup: unix time of the last relay send per printer,
	// used for the 60s heartbeat when telemetry is otherwise unchanged.
	webhookLastSent   = make(map[string]int64)
	webhookLastSentMu sync.Mutex
)

// managedConn is the cancellation handle for one printer's management goroutine,
// mirroring lan.Controller's cancelers. stop is closed to ask the goroutine to
// shut down; done is closed by the goroutine once it has fully exited, so
// DisconnectPrinter can block until no old goroutine can race a reconnect.
type managedConn struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func nextSequenceID() string {
	sequenceMu.Lock()
	defer sequenceMu.Unlock()
	sequenceID++
	return fmt.Sprintf("%d", sequenceID)
}

type printSession struct {
	StartTime   int64
	FileName    string
	NozzleTemp  float64
	BedTemp     float64
	TempSamples []history.TempSample
	lastSampleT int64 // unix time of last temp sample
}

var (
	activeSessions = make(map[string]*printSession)
	sessionMu      sync.Mutex
)

// copyTelemetry returns a deep copy of s so callers never hold live pointers
// into the state map, which is mutated concurrently by MQTT handlers.
func copyTelemetry(s *TelemetryData) *TelemetryData {
	c := *s
	if s.AMS != nil {
		c.AMS = append([]AmsSlot(nil), s.AMS...)
	}
	return &c
}

func GetPrinterState(name string) *TelemetryData {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	if s, ok := printerStates[name]; ok {
		return copyTelemetry(s)
	}
	return &TelemetryData{Status: "disconnected", PrinterID: name}
}

func UpdatePrinterState(name string, t TelemetryData) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	t.PrinterID = name
	t.Timestamp = time.Now().Unix()
	printerStates[name] = &t
}

func GetPrintersState() map[string]*TelemetryData {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	out := make(map[string]*TelemetryData, len(printerStates))
	for k, v := range printerStates {
		out[k] = copyTelemetry(v)
	}
	return out
}

// SendCommand sends a control command to a named printer via MQTT.
// command is one of: "pause", "resume", "stop", "light_on", "light_off", "start", "toggle_light"
func SendCommand(printerName, command string) error {
	return SendCommandWithArgs(printerName, command, nil)
}

// SendCommandWithArgs sends a command with optional arguments.
// For start prints, accepted args are:
// - file_name or file: printer-local path or URL
// - url: explicit URL
// - start_command: optional override for printer command name (default: project_file)
func SendCommandWithArgs(printerName, command string, args map[string]interface{}) error {
	clientMutex.RLock()
	client, ok := printerClients[printerName]
	clientMutex.RUnlock()

	if !ok {
		return fmt.Errorf("printer %q is not connected (no active MQTT session)", printerName)
	}
	if !client.IsConnected() {
		return fmt.Errorf("printer %q MQTT session dropped — reconnecting, try again in a moment", printerName)
	}

	// Find serial for the topic
	stateMutex.RLock()
	state, hasState := printerStates[printerName]
	stateMutex.RUnlock()
	_ = hasState

	// We store serial in PrinterID only if set — look it up from connected printer map
	serial := getSerial(printerName)
	if serial == "" {
		return fmt.Errorf("serial not found for %q", printerName)
	}
	_ = state

	// Over Bambu Cloud, current firmware rejects every command except the
	// light as unverified. Refuse early so the printer never sees them.
	cloud := IsCloudSerial(serial)
	if cloud && !cloudCommandAllowed(command) {
		return fmt.Errorf("%q is not available over Bambu Cloud — only the light can be controlled; switch the printer to LAN mode for full control", command)
	}

	topic := fmt.Sprintf("device/%s/request", serial)
	var payload string

	// ledPayload builds the ledctrl JSON for a single node with a fresh sequence_id.
	ledPayload := func(node, mode string) string {
		b, _ := json.Marshal(map[string]interface{}{
			"system": map[string]interface{}{
				"sequence_id":   nextSequenceID(),
				"command":       "ledctrl",
				"led_node":      node,
				"led_mode":      mode,
				"led_on_time":   500,
				"led_off_time":  500,
				"loop_times":    0,
				"interval_time": 0,
			},
		})
		return string(b)
	}

	// sendLights publishes ledctrl to both chamber_light and chamber_light2.
	// QoS 0: BambuLab's embedded broker does not reliably PUBACK QoS-1 publishes
	// on the request topic, so QoS 1 always times out. Fire-and-forget is safe on LAN.
	sendLights := func(mode string) {
		for _, node := range []string{"chamber_light", "chamber_light2"} {
			client.Publish(topic, 0, false, ledPayload(node, mode))
		}
	}

	// applyLightResult applies optimistic state and requests a fresh pushall.
	applyLightResult := func(wantOn bool) {
		stateMutex.Lock()
		if s, ok := printerStates[printerName]; ok {
			s.LightOn = wantOn
		}
		stateMutex.Unlock()
		if cloud {
			go cloudRequestPushall(serial) // rate-limited: at most one per minute per printer
		} else {
			go sendPushall(client, printerName, topic)
		}
	}

	switch command {
	case "pause":
		payload = `{"print":{"sequence_id":"0","command":"pause"}}`
	case "resume":
		payload = `{"print":{"sequence_id":"0","command":"resume"}}`
	case "stop":
		payload = `{"print":{"sequence_id":"0","command":"stop"}}`

	case "light_on":
		sendLights("on")
		log.Printf("[%s] sent command: light_on", printerName)
		applyLightResult(true)
		return nil

	case "light_off":
		sendLights("off")
		log.Printf("[%s] sent command: light_off", printerName)
		applyLightResult(false)
		return nil

	case "light":
		on := getBoolArg(args, "on")
		if on == nil {
			onVal := !(state != nil && state.LightOn)
			on = &onVal
		}
		mode := "off"
		if *on {
			mode = "on"
		}
		sendLights(mode)
		log.Printf("[%s] sent command: light (%s)", printerName, mode)
		applyLightResult(*on)
		return nil

	case "toggle_light":
		wantOn := !(state != nil && state.LightOn)
		mode := "off"
		if wantOn {
			mode = "on"
		}
		sendLights(mode)
		log.Printf("[%s] sent command: toggle_light (%s)", printerName, mode)
		applyLightResult(wantOn)
		return nil

	case "print_speed":
		level := getStringArg(args, "level")
		if level == "" {
			level = "2"
		}
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": "0",
				"command":     "print_speed",
				"param":       level,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build print_speed payload: %w", err)
		}
		payload = string(b)

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
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": "0",
				"command":     "gcode_line",
				"param":       fmt.Sprintf("M106 P1 S%d", s),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build fan_speed payload: %w", err)
		}
		payload = string(b)

	case "gcode":
		line := getStringArg(args, "line")
		if line == "" {
			return fmt.Errorf("gcode command requires args[\"line\"]")
		}
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": "0",
				"command":     "gcode_line",
				"param":       line,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build gcode payload: %w", err)
		}
		payload = string(b)

	case "start":
		target := getStringArg(args, "url")
		if target == "" {
			target = getStringArg(args, "file_name")
		}
		if target == "" {
			target = getStringArg(args, "file")
		}
		if target == "" {
			return fmt.Errorf("start command requires one of: url, file_name, or file")
		}
		if !strings.Contains(target, "://") {
			target = "file:///" + strings.TrimPrefix(target, "/")
		}
		startCommand := getStringArg(args, "start_command")
		if startCommand == "" {
			startCommand = "project_file"
		}
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": "0",
				"command":     startCommand,
				"url":         target,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build start payload: %w", err)
		}
		payload = string(b)

	case "ams_load":
		amsId, _ := strconv.Atoi(getStringArg(args, "ams"))
		trayId, _ := strconv.Atoi(getStringArg(args, "tray"))
		// Use actual nozzle temp so the printer knows whether it needs to heat up first.
		currTemp := 25
		if st := GetPrinterState(printerName); st != nil && st.NozzleTemp > 0 {
			currTemp = int(st.NozzleTemp)
		}
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": nextSequenceID(),
				"command":     "ams_change_filament",
				"tar_ams":     amsId,
				"tar_tray":    trayId,
				"curr_temp":   currTemp,
				"tar_temp":    220,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build ams_load payload: %w", err)
		}
		payload = string(b)

	case "ams_unload":
		// "param" key is required — BambuLab broker silently drops the message without it.
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id": nextSequenceID(),
				"command":     "ams_unload_filament",
				"param":       "",
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build ams_unload payload: %w", err)
		}
		payload = string(b)

	case "ams_filament_setting":
		amsId, _ := strconv.Atoi(getStringArg(args, "ams"))
		trayId, _ := strconv.Atoi(getStringArg(args, "tray"))
		color := getStringArg(args, "color")
		if len(color) == 6 {
			color = color + "FF"
		} else if len(color) != 8 {
			color = "FFFFFFFF"
		}
		material := strings.ToUpper(getStringArg(args, "material"))
		if material == "" {
			material = "PLA"
		}
		b, err := json.Marshal(map[string]interface{}{
			"print": map[string]interface{}{
				"sequence_id":     nextSequenceID(),
				"command":         "ams_filament_setting",
				"ams_id":          amsId,
				"tray_id":         trayId,
				"tray_color":      strings.ToUpper(color),
				"nozzle_temp_min": amsTempMin(material),
				"nozzle_temp_max": amsTempMax(material),
				"tray_type":       material,
				"setting_id":      "",
				"ctype":           0,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to build ams_filament_setting payload: %w", err)
		}
		payload = string(b)

	default:
		return fmt.Errorf("unknown command: %q", command)
	}

	// QoS 0: same reasoning as sendLights above — BambuLab broker drops QoS-1 PUBACKs.
	client.Publish(topic, 0, false, payload)
	log.Printf("[%s] sent command: %s", printerName, command)

	// After filament settings change, the printer won't broadcast an update automatically.
	// Send a pushall after a short delay so the UI reflects the new colors/material.
	if command == "ams_filament_setting" || command == "ams_unload" || command == "ams_load" {
		go func() {
			time.Sleep(400 * time.Millisecond)
			sendPushall(client, printerName, topic)
		}()
	}

	return nil
}

func amsTempMin(material string) int {
	switch material {
	case "PETG", "TPU":
		return 220
	case "ABS", "ASA":
		return 240
	case "PA", "PC":
		return 260
	default:
		return 190
	}
}

func amsTempMax(material string) int {
	switch material {
	case "PETG", "TPU":
		return 250
	case "ABS", "ASA":
		return 270
	case "PA", "PC":
		return 290
	default:
		return 230
	}
}

func getStringArg(args map[string]interface{}, key string) string {
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

func getBoolArg(args map[string]interface{}, key string) *bool {
	if args == nil {
		return nil
	}
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

// printerSerials maps name → serial for control commands
var (
	printerSerials = make(map[string]string)
	serialMutex    sync.RWMutex
)

func setSerial(name, serial string) {
	serialMutex.Lock()
	defer serialMutex.Unlock()
	printerSerials[name] = serial
}

func getSerial(name string) string {
	serialMutex.RLock()
	defer serialMutex.RUnlock()
	return printerSerials[name]
}

// DisconnectPrinter stops the management goroutine for the named printer and
// blocks until it has fully exited, so a subsequent ConnectPrinter call can
// start a fresh one (e.g. after IP/LAN code change) without racing the old
// goroutine's shutdown. No-op if the printer has no active goroutine.
func DisconnectPrinter(name string) {
	managedPrintersMu.Lock()
	mc := managedPrinters[name]
	managedPrintersMu.Unlock()
	if mc == nil {
		return
	}
	mc.stopOnce.Do(func() { close(mc.stop) })

	// Drop the current client so commands fail fast and the connect loop's
	// wait returns promptly instead of idling until the next keepalive.
	clientMutex.Lock()
	if c, ok := printerClients[name]; ok {
		c.Disconnect(250)
		delete(printerClients, name)
	}
	clientMutex.Unlock()

	<-mc.done
}

// RemovePrinterState deletes all per-printer state this package keeps for the
// named printer: telemetry, serial mapping, snapshot throttle, webhook dedup,
// and any active print session. Call after DisconnectPrinter when the printer
// is being removed (not just restarted).
func RemovePrinterState(name string) {
	stateMutex.Lock()
	delete(printerStates, name)
	stateMutex.Unlock()

	serialMutex.Lock()
	delete(printerSerials, name)
	serialMutex.Unlock()

	sessionMu.Lock()
	delete(activeSessions, name)
	sessionMu.Unlock()

	snapLastTMu.Lock()
	delete(snapLastT, name)
	snapLastTMu.Unlock()

	snapInFlightMu.Lock()
	delete(snapInFlight, name)
	snapInFlightMu.Unlock()

	webhookLastSentMu.Lock()
	delete(webhookLastSent, name)
	webhookLastSentMu.Unlock()
}

func ConnectPrinter(p Printer) {
	setSerial(p.Name, p.Serial)

	// Guard against duplicate goroutines. If a management goroutine is already
	// running for this printer (e.g. syncPrinterConnections called redundantly on
	// settings save), do nothing — the existing goroutine handles reconnects.
	managedPrintersMu.Lock()
	if managedPrinters[p.Name] != nil {
		managedPrintersMu.Unlock()
		return
	}
	mc := &managedConn{stop: make(chan struct{}), done: make(chan struct{})}
	managedPrinters[p.Name] = mc
	managedPrintersMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] MQTT connection goroutine panic: %v", p.Name, r)
			}
			managedPrintersMu.Lock()
			if managedPrinters[p.Name] == mc {
				delete(managedPrinters, p.Name)
			}
			managedPrintersMu.Unlock()
			close(mc.done)
		}()
		for {
			err := connectAndListen(p, mc.stop)

			// Remove client immediately so commands fail fast rather than
			// hitting a disconnected client.
			clientMutex.Lock()
			delete(printerClients, p.Name)
			clientMutex.Unlock()

			// Stop requested — exit without touching state, so a replacement
			// goroutine (or the delete handler) sees no stale writes.
			select {
			case <-mc.stop:
				log.Printf("[%s] MQTT connection stopped", p.Name)
				return
			default:
			}

			if err != nil {
				log.Printf("[%s] disconnected: %v — retrying in 5s", p.Name, err)
			}

			// Always update status to disconnected right away so the UI
			// disables controls instead of showing stale "idle/printing" state.
			UpdatePrinterState(p.Name, TelemetryData{
				Status:    "disconnected",
				PrinterID: p.Name,
				Timestamp: time.Now().Unix(),
			})

			select {
			case <-mc.stop:
				log.Printf("[%s] MQTT connection stopped", p.Name)
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}

// connectAndListen runs one MQTT session for p. It returns when the connection
// drops or when stop is closed; on stop it disconnects cleanly and returns nil.
func connectAndListen(p Printer, stop <-chan struct{}) error {
	broker := fmt.Sprintf("ssl://%s:8883", p.IP)
	done := make(chan struct{})

	opts := mqtt.NewClientOptions()
	opts.AddBroker(broker)
	opts.SetUsername("bblp")
	opts.SetPassword(p.LANCode)
	opts.SetClientID(fmt.Sprintf("foxtrack-%s-%d", p.Serial, time.Now().UnixNano()))
	opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(20 * time.Second) // under typical 30s NAT TCP-idle timeout
	opts.SetPingTimeout(5 * time.Second)
	opts.SetAutoReconnect(false)
	opts.SetCleanSession(true)

	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("[%s] connection lost: %v", p.Name, err)
		close(done)
	})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if token.WaitTimeout(15*time.Second) && token.Error() != nil {
		return token.Error()
	}
	if !client.IsConnected() {
		client.Disconnect(0) // clean up any paho background goroutines
		return fmt.Errorf("connect timed out")
	}
	log.Printf("[%s] MQTT connected", p.Name)

	// Store client for control commands
	clientMutex.Lock()
	printerClients[p.Name] = client
	clientMutex.Unlock()

	topic := fmt.Sprintf("device/%s/report", p.Serial)
	subToken := client.Subscribe(topic, 0, makeHandler(p))
	if !subToken.WaitTimeout(10 * time.Second) {
		client.Disconnect(250)
		return fmt.Errorf("subscribe to %s timed out", topic)
	}
	if err := subToken.Error(); err != nil {
		client.Disconnect(250)
		return err
	}
	log.Printf("[%s] subscribed to %s", p.Name, topic)

	UpdatePrinterState(p.Name, TelemetryData{
		Status:    "connected",
		PrinterID: p.Name,
		Timestamp: time.Now().Unix(),
	})

	requestTopic := fmt.Sprintf("device/%s/request", p.Serial)
	sendPushall(client, p.Name, requestTopic)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				return
			case <-ticker.C:
				if client.IsConnected() {
					sendPushall(client, p.Name, requestTopic)
				}
			}
		}
	}()

	select {
	case <-done:
		client.Disconnect(250)
		return fmt.Errorf("connection lost")
	case <-stop:
		client.Disconnect(250)
		return nil
	}
}

func sendPushall(client mqtt.Client, printerName, requestTopic string) {
	payload := `{"pushing": {"sequence_id": "0", "command": "pushall"}}`
	client.Publish(requestTopic, 0, false, payload)
	log.Printf("[%s] sent pushall", printerName)
}

// ShouldSendWebhook reports whether curr differs from prev in any field the
// cloud relay cares about. Shared by the Bambu MQTT path and the Klipper LAN
// path (lan.Controller) so both printer types deduplicate webhooks the same way.
func ShouldSendWebhook(prev, curr *TelemetryData) bool {
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

func makeHandler(p Printer) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] MQTT message handler panic (message dropped): %v", p.Name, r)
			}
		}()
		var report BambuReport
		if err := json.Unmarshal(msg.Payload(), &report); err != nil {
			log.Printf("[%s] MQTT parse error: %v | payload: %.120s", p.Name, err, msg.Payload())
			return
		}

		pr := report.Print

		// Ignore messages that carry no print or system data at all.
		// msg:1 is a wifi signal heartbeat — silent skip, expected every few seconds.
		hasData := pr.GcodeState != "" || pr.NozzleTemper != 0 || pr.BedTemper != 0 || len(pr.Lights) > 0 || pr.Ams != nil || pr.SpdLvl != 0 || pr.McPercent != 0 || pr.McRemainingTime != 0
		if !hasData {
			if pr.Msg != 1 {
				log.Printf("[%s] MQTT skip (no usable data) | gcode=%q nozzle=%.1f bed=%.1f | payload: %.120s", p.Name, pr.GcodeState, pr.NozzleTemper, pr.BedTemper, msg.Payload())
			}
			return
		}

		// Preserve previous state for fields absent in incremental updates.
		prev := GetPrinterState(p.Name)

		status := prev.Status
		if pr.GcodeState != "" {
			status = mapGcodeState(pr.GcodeState)
		}

		// Preserve file name across partial updates — BambuLab omits subtask_name
		// in incremental messages even when a print is running.
		fileName := prev.FileName
		if pr.SubTaskName != "" {
			fileName = pr.SubTaskName
		}
		// Clear file name when the printer is not printing.
		if status != "printing" && status != "paused" {
			fileName = ""
		}

		// Preserve temps when they are absent (reported as 0) in partial updates.
		nozzleTemp := prev.NozzleTemp
		if pr.NozzleTemper != 0 {
			nozzleTemp = pr.NozzleTemper
		}
		nozzleTarget := prev.NozzleTarget
		if pr.NozzleTargetTemper != 0 {
			nozzleTarget = pr.NozzleTargetTemper
		}
		bedTemp := prev.BedTemp
		if pr.BedTemper != 0 {
			bedTemp = pr.BedTemper
		}
		bedTarget := prev.BedTarget
		if pr.BedTargetTemper != 0 {
			bedTarget = pr.BedTargetTemper
		}
		progress := prev.Progress
		if pr.McPercent != 0 {
			progress = pr.McPercent
		}
		timeRemaining := prev.TimeRemaining
		if pr.McRemainingTime != 0 {
			timeRemaining = pr.McRemainingTime
		}
		// Clear transient print fields when the printer is no longer actively printing.
		if status != "printing" && status != "paused" {
			timeRemaining = 0
			progress = 0
		}

		// Parse light state; preserve previous value when lights_report is absent.
		// Printers report either "chamber_light" or "work_light" depending on model.
		lightOn := prev.LightOn
		if len(pr.Lights) > 0 {
			lightOn = false
			for _, l := range pr.Lights {
				if (l.Node == "chamber_light" || l.Node == "work_light") && l.Mode == "on" {
					lightOn = true
				}
			}
		}

		// Speed level: preserve previous when absent (spd_lvl==0 means not reported).
		speedLevel := prev.SpeedLevel
		if pr.SpdLvl != 0 {
			speedLevel = pr.SpdLvl
		}

		// AMS: parse slot data; preserve previous when ams field is absent.
		amsSlots := prev.AMS
		if pr.Ams != nil {
			if len(pr.Ams.AMS) > 0 {
				// Full tray list present — rebuild slots from scratch.
				amsSlots = nil
				trayNow := -1
				if n, err := strconv.Atoi(pr.Ams.TrayNow); err == nil && n < 254 {
					trayNow = n
				}
				for amsIdx, unit := range pr.Ams.AMS {
					for i, tray := range unit.Tray {
						globalSlot := amsIdx*4 + i
						color := tray.Color
						if len(color) >= 6 {
							color = color[:6] // strip alpha channel
						}
						amsSlots = append(amsSlots, AmsSlot{
							Slot:      globalSlot,
							Color:     color,
							Material:  tray.Material,
							Active:    globalSlot == trayNow,
							Remaining: tray.Remain,
						})
					}
				}
			} else if pr.Ams.TrayNow != "" {
				// Incremental update — only tray_now changed (e.g. at print start).
				// Update Active flags on a copy of the existing slot list rather than
				// mutating the previous state's slice in place.
				trayNow := -1
				if n, err := strconv.Atoi(pr.Ams.TrayNow); err == nil && n < 254 {
					trayNow = n
				}
				amsSlots = append([]AmsSlot(nil), amsSlots...)
				for i := range amsSlots {
					amsSlots[i].Active = amsSlots[i].Slot == trayNow
				}
			}
		}

		// Cooling fan: raw 0-15 → 0-100%. Preserve previous when not reported (0).
		coolingFanPct := prev.CoolingFanPct
		if pr.CoolingFanSpeed != nil {
			coolingFanPct = int(*pr.CoolingFanSpeed) * 100 / 15
		}

		// Total lifetime print hours and model — only present in pushall responses;
		// preserve previous values so incremental updates don't zero them out.
		totalPrintHours := prev.TotalPrintHours
		if pr.TotalRunTimeSec > 0 {
			totalPrintHours = float64(pr.TotalRunTimeSec) / 3600
		}
		printerModel := prev.PrinterModel
		if pr.MachineType != "" {
			printerModel = mapMachineType(pr.MachineType)
		}

		t := TelemetryData{
			Status:          status,
			FileName:        fileName,
			Progress:        progress,
			Error:           pr.McPrintErrorCode,
			NozzleTemp:      nozzleTemp,
			NozzleTarget:    nozzleTarget,
			BedTemp:         bedTemp,
			BedTarget:       bedTarget,
			LightOn:         lightOn,
			TimeRemaining:   timeRemaining,
			SpeedLevel:      speedLevel,
			AMS:             amsSlots,
			ActiveExtruder:  prev.ActiveExtruder,
			CoolingFanPct:   coolingFanPct,
			TotalPrintHours: totalPrintHours,
			PrinterModel:    printerModel,
		}

		// Track print sessions for history (including periodic temp samples).
		sessionMu.Lock()
		sess := activeSessions[p.Name]
		if prev.Status != "printing" && status == "printing" {
			now := time.Now().Unix()
			activeSessions[p.Name] = &printSession{
				StartTime:   now,
				FileName:    fileName, // use merged value; pr.SubTaskName may be absent in the state-change message
				NozzleTemp:  nozzleTemp,
				BedTemp:     bedTemp,
				TempSamples: []history.TempSample{{T: 0, NozzleTemp: nozzleTemp, BedTemp: bedTemp}},
				lastSampleT: now,
			}
			sessionMu.Unlock()
		} else if prev.Status == "printing" && status != "printing" && sess != nil {
			now := time.Now().Unix()
			result := status
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
			delete(activeSessions, p.Name)
			sessionMu.Unlock()
			go func() {
				if err := history.Append(rec); err != nil {
					log.Printf("[%s] history write error: %v", p.Name, err)
				}
				if p.FoxTrack2APIKey != "" {
					webhook.SendHistory(p.FoxTrack2APIKey, webhook.HistoryURLV2, webhook.HistoryPayload{
						Type:        "print_complete",
						PrinterName: p.Name,
						Serial:      p.Serial,
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
			if status == "printing" && sess != nil {
				if sess.FileName == "" && fileName != "" {
					sess.FileName = fileName
				}
				now := time.Now().Unix()
				if now-sess.lastSampleT >= 60 {
					sess.TempSamples = append(sess.TempSamples, history.TempSample{
						T:          now - sess.StartTime,
						NozzleTemp: nozzleTemp,
						BedTemp:    bedTemp,
					})
					sess.lastSampleT = now
				}
			}
			sessionMu.Unlock()
		}

		UpdatePrinterState(p.Name, t)

		log.Printf("[%s] %s | %q | %d%% | nozzle:%.0f°C bed:%.0f°C",
			p.Name, status, fileName, progress,
			nozzleTemp, bedTemp)

		// Deduplicate relay webhooks — Bambu pushes ~1 message/second while
		// printing. Terminal transitions (print complete/failed) always send
		// immediately; otherwise send on meaningful change, with a 60s
		// heartbeat so the cloud still sees the printer as alive.
		terminal := status != prev.Status && (status == "finished" || status == "error")
		sendWebhook := terminal || ShouldSendWebhook(prev, &t)
		if !sendWebhook {
			webhookLastSentMu.Lock()
			sendWebhook = time.Now().Unix()-webhookLastSent[p.Name] >= 60
			webhookLastSentMu.Unlock()
		}

		if sendWebhook {
			webhookLastSentMu.Lock()
			webhookLastSent[p.Name] = time.Now().Unix()
			webhookLastSentMu.Unlock()

			go func() {
				b := lightOn
				var relayAms []webhook.RelayAmsSlot
				for _, s := range amsSlots {
					relayAms = append(relayAms, webhook.RelayAmsSlot{
						Slot:      s.Slot,
						Color:     s.Color,
						Material:  s.Material,
						Remaining: s.Remaining,
						Active:    s.Active,
					})
				}
				relayPayload := webhook.RelayPayload{
					Print: webhook.RelayPrint{
						GcodeState:         pr.GcodeState,
						SubTaskName:        fileName,
						McPercent:          progress,
						NozzleTemper:       nozzleTemp,
						NozzleTargetTemper: nozzleTarget,
						BedTemper:          bedTemp,
						BedTargetTemper:    bedTarget,
						McRemainingTime:    timeRemaining,
						LightOn:            &b,
						Ams:                relayAms,
					},
				}
				if p.APIKey != "" {
					if err := webhook.SendRelay(p.APIKey, webhook.URL, p.Serial, p.Name, relayPayload); err != nil {
						log.Printf("[%s] webhook error (legacy): %v", p.Name, err)
					}
				} else {
					log.Printf("[%s] skipping webhook — API key not configured", p.Name)
				}
				if p.FoxTrack2APIKey != "" {
					if err := webhook.SendRelay(p.FoxTrack2APIKey, webhook.RelayURLV2, p.Serial, p.Name, relayPayload); err != nil {
						log.Printf("[%s] webhook error (v2): %v", p.Name, err)
					}
				}
			}()
		}

		if (t.Status == "printing" || t.Status == "paused") && p.FoxTrack2APIKey != "" {
			now := time.Now().Unix()
			snapLastTMu.Lock()
			eligible := now-snapLastT[p.Name] >= 25
			if eligible {
				snapLastT[p.Name] = now
			}
			snapLastTMu.Unlock()

			if eligible {
				snapInFlightMu.Lock()
				busy := snapInFlight[p.Name]
				if !busy {
					snapInFlight[p.Name] = true
				}
				snapInFlightMu.Unlock()

				if !busy {
					ip, lanCode, serial := p.IP, p.LANCode, p.Serial
					name, apiKey := p.Name, p.FoxTrack2APIKey
					go func() {
						defer func() {
							snapInFlightMu.Lock()
							delete(snapInFlight, name)
							snapInFlightMu.Unlock()
						}()
						frame, err := capture.BambuFrame(ip, lanCode, name)
						if err != nil {
							log.Printf("[%s] snapshot capture: %v", name, err)
							return
						}
						if err := webhook.SendSnapshot(apiKey, webhook.SnapshotURLV2, serial, name, frame); err != nil {
							log.Printf("[%s] snapshot send: %v", name, err)
						}
					}()
				}
			}
		}
	}
}

// mapMachineType converts BambuLab internal model codes to human-readable names.
// If the value is already a recognisable string it is returned as-is.
func mapMachineType(t string) string {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "X1C", "C11":
		return "X1 Carbon"
	case "X1E", "C12":
		return "X1E"
	case "X1", "C10":
		return "X1"
	case "P1P", "N1":
		return "P1P"
	case "P1S", "N2S", "N2":
		return "P1S"
	case "A1", "N4":
		return "A1"
	case "A1M", "A1MINI", "N4M":
		return "A1 Mini"
	case "H2D":
		return "H2D"
	default:
		return t // unknown code — pass through as-is
	}
}

func mapGcodeState(s string) string {
	switch s {
	case "IDLE":
		return "idle"
	case "RUNNING":
		return "printing"
	case "PAUSE":
		return "paused"
	case "FINISH":
		return "finished"
	case "FAILED":
		return "error"
	default:
		return s
	}
}
