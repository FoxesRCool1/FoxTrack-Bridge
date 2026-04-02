package mqtt

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"foxtrack-bridge/webhook"
)

type Printer struct {
	Name       string
	IP         string
	Serial     string
	LANCode    string
	WebhookURL string
	APIKey     string
}

type TelemetryData struct {
	Status        string  `json:"status"`
	FileName      string  `json:"file_name"`
	Progress      int     `json:"progress"`
	Error         string  `json:"error"`
	PrinterID     string  `json:"printer_id"`
	Timestamp     int64   `json:"timestamp"`
	NozzleTemp    float64 `json:"nozzle_temp"`
	NozzleTarget  float64 `json:"nozzle_target"`
	BedTemp       float64 `json:"bed_temp"`
	BedTarget     float64 `json:"bed_target"`
	LightOn       bool    `json:"light_on"`
	TimeRemaining int     `json:"time_remaining"` // minutes
}

type BambuReport struct {
	Print BambuPrint `json:"print"`
}

type BambuPrint struct {
	GcodeState       string  `json:"gcode_state"`
	SubTaskName      string  `json:"subtask_name"`
	McPercent        int     `json:"mc_percent"`
	McPrintErrorCode string  `json:"mc_print_error_code"`
	NozzleTemper     float64 `json:"nozzle_temper"`
	NozzleTargetTemper float64 `json:"nozzle_target_temper"`
	BedTemper        float64 `json:"bed_temper"`
	BedTargetTemper  float64 `json:"bed_target_temper"`
	Lights           []struct {
		Node string `json:"node"`
		Mode string `json:"mode"`
	} `json:"lights_report"`
	McRemainingTime int `json:"mc_remaining_time"`
}

var (
	printerStates  = make(map[string]*TelemetryData)
	printerClients = make(map[string]mqtt.Client) // for sending control commands
	stateMutex     sync.RWMutex
	clientMutex    sync.RWMutex
)

func GetPrinterState(name string) *TelemetryData {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	if s, ok := printerStates[name]; ok {
		return s
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
		out[k] = v
	}
	return out
}

// SendCommand sends a control command to a named printer via MQTT.
// command is one of: "pause", "resume", "stop", "light_on", "light_off"
func SendCommand(printerName, command string) error {
	clientMutex.RLock()
	client, ok := printerClients[printerName]
	clientMutex.RUnlock()

	if !ok || !client.IsConnected() {
		return fmt.Errorf("printer %q not connected", printerName)
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

	topic := fmt.Sprintf("device/%s/request", serial)
	var payload string

	switch command {
	case "pause":
		payload = `{"print":{"sequence_id":"0","command":"pause"}}`
	case "resume":
		payload = `{"print":{"sequence_id":"0","command":"resume"}}`
	case "stop":
		payload = `{"print":{"sequence_id":"0","command":"stop"}}`
	case "light_on":
		payload = `{"system":{"sequence_id":"0","command":"ledctrl","led_node":"work_light","led_mode":"on","led_on_time":500,"led_off_time":500,"loop_times":0,"interval_time":0}}`
	case "light_off":
		payload = `{"system":{"sequence_id":"0","command":"ledctrl","led_node":"work_light","led_mode":"off","led_on_time":500,"led_off_time":500,"loop_times":0,"interval_time":0}}`
	default:
		return fmt.Errorf("unknown command: %q", command)
	}

	token := client.Publish(topic, 0, false, payload)
	token.WaitTimeout(5 * time.Second)
	log.Printf("[%s] sent command: %s", printerName, command)
	return nil
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

func ConnectPrinter(p Printer) {
	setSerial(p.Name, p.Serial)
	go func() {
		for {
			err := connectAndListen(p)
			if err != nil {
				log.Printf("[%s] disconnected: %v — retrying in 15s", p.Name, err)
			}

			state := GetPrinterState(p.Name)
			if time.Now().Unix()-state.Timestamp > 300 {
				UpdatePrinterState(p.Name, TelemetryData{
					Status:    "disconnected",
					PrinterID: p.Name,
				})
				if p.WebhookURL != "" && p.APIKey != "" {
					_ = webhook.Send(p.APIKey, p.WebhookURL, webhook.Payload{
						PrinterName: p.Name,
						Serial:      p.Serial,
						Status:      "disconnected",
						Timestamp:   time.Now().Unix(),
					})
				}
			}

			// Remove client on disconnect
			clientMutex.Lock()
			delete(printerClients, p.Name)
			clientMutex.Unlock()

			time.Sleep(15 * time.Second)
		}
	}()
}

func connectAndListen(p Printer) error {
	broker := fmt.Sprintf("ssl://%s:8883", p.IP)
	done := make(chan struct{})

	opts := mqtt.NewClientOptions()
	opts.AddBroker(broker)
	opts.SetUsername("bblp")
	opts.SetPassword(p.LANCode)
	opts.SetClientID(fmt.Sprintf("foxtrack-%s-%d", p.Serial, time.Now().UnixNano()))
	opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
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
		return fmt.Errorf("connect timed out")
	}
	log.Printf("[%s] MQTT connected", p.Name)

	// Store client for control commands
	clientMutex.Lock()
	printerClients[p.Name] = client
	clientMutex.Unlock()

	topic := fmt.Sprintf("device/%s/report", p.Serial)
	subToken := client.Subscribe(topic, 0, makeHandler(p))
	if subToken.WaitTimeout(10*time.Second) && subToken.Error() != nil {
		client.Disconnect(250)
		return subToken.Error()
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
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if client.IsConnected() {
					sendPushall(client, p.Name, requestTopic)
				}
			}
		}
	}()

	<-done
	client.Disconnect(250)
	return fmt.Errorf("connection lost")
}

func sendPushall(client mqtt.Client, printerName, requestTopic string) {
	payload := `{"pushing": {"sequence_id": "0", "command": "pushall"}}`
	token := client.Publish(requestTopic, 0, false, payload)
	token.WaitTimeout(5 * time.Second)
	log.Printf("[%s] sent pushall", printerName)
}

func makeHandler(p Printer) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var report BambuReport
		if err := json.Unmarshal(msg.Payload(), &report); err != nil {
			return
		}

		pr := report.Print
		if pr.GcodeState == "" {
			return
		}

		status := mapGcodeState(pr.GcodeState)

		// Parse light state
		lightOn := false
		for _, l := range pr.Lights {
			if l.Node == "work_light" && l.Mode == "on" {
				lightOn = true
			}
		}

		t := TelemetryData{
			Status:        status,
			FileName:      pr.SubTaskName,
			Progress:      pr.McPercent,
			Error:         pr.McPrintErrorCode,
			NozzleTemp:    pr.NozzleTemper,
			NozzleTarget:  pr.NozzleTargetTemper,
			BedTemp:       pr.BedTemper,
			BedTarget:     pr.BedTargetTemper,
			LightOn:       lightOn,
			TimeRemaining: pr.McRemainingTime,
		}
		UpdatePrinterState(p.Name, t)

		log.Printf("[%s] %s | %q | %d%% | nozzle:%.0f°C bed:%.0f°C",
			p.Name, status, pr.SubTaskName, pr.McPercent,
			pr.NozzleTemper, pr.BedTemper)

		if p.WebhookURL == "" || p.APIKey == "" {
			log.Printf("[%s] skipping webhook — API key or URL not configured", p.Name)
			return
		}

		go func() {
			payload := webhook.Payload{
				PrinterName: p.Name,
				Serial:      p.Serial,
				Status:      status,
				FileName:    pr.SubTaskName,
				Progress:    pr.McPercent,
				ErrorCode:   pr.McPrintErrorCode,
				Timestamp:   time.Now().Unix(),
			}
			if err := webhook.Send(p.APIKey, p.WebhookURL, payload); err != nil {
				log.Printf("[%s] webhook error: %v", p.Name, err)
			}
		}()
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
