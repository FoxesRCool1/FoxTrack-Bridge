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
	Status    string `json:"status"`
	FileName  string `json:"file_name"`
	Progress  int    `json:"progress"`
	Error     string `json:"error"`
	PrinterID string `json:"printer_id"`
	Timestamp int64  `json:"timestamp"`
}

type BambuReport struct {
	Print BambuPrint `json:"print"`
}

type BambuPrint struct {
	GcodeState       string `json:"gcode_state"`
	SubTaskName      string `json:"subtask_name"`
	McPercent        int    `json:"mc_percent"`
	McPrintErrorCode string `json:"mc_print_error_code"`
}

var (
	printerStates = make(map[string]*TelemetryData)
	stateMutex    sync.RWMutex
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

// ConnectPrinter starts a persistent background goroutine for one printer.
func ConnectPrinter(p Printer) {
	go func() {
		for {
			err := connectAndListen(p)
			if err != nil {
				log.Printf("[%s] disconnected: %v — retrying in 15s", p.Name, err)
			}

			// Only tell FoxTrack the printer is offline if we haven't
			// had a real telemetry update in the last 5 minutes.
			// BambuLab drops the MQTT connection frequently (even mid-print)
			// so a brief dropout must not overwrite the real status.
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

			time.Sleep(15 * time.Second)
		}
	}()
}

func connectAndListen(p Printer) error {
	broker := fmt.Sprintf("ssl://%s:8883", p.IP)

	// done is closed when the connection is lost
	done := make(chan struct{})

	opts := mqtt.NewClientOptions()
	opts.AddBroker(broker)
	opts.SetUsername("bblp")
	opts.SetPassword(p.LANCode)
	opts.SetClientID(fmt.Sprintf("foxtrack-%s-%d", p.Serial, time.Now().UnixNano()))
	opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetKeepAlive(30 * time.Second)   // ping every 30s to keep connection alive
	opts.SetPingTimeout(10 * time.Second)
	opts.SetAutoReconnect(false)
	opts.SetCleanSession(true)

	// Use connection-lost handler instead of polling IsConnected()
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("[%s] connection lost: %v", p.Name, err)
		close(done)
	})

	// Re-subscribe automatically if the library reconnects (shouldn't happen
	// with AutoReconnect=false, but belt-and-suspenders)
	topic := fmt.Sprintf("device/%s/report", p.Serial)
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		c.Subscribe(topic, 0, makeHandler(p))
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

	// Subscribe
	subToken := client.Subscribe(topic, 0, makeHandler(p))
	if subToken.WaitTimeout(10*time.Second) && subToken.Error() != nil {
		client.Disconnect(250)
		return subToken.Error()
	}
	log.Printf("[%s] subscribed to %s", p.Name, topic)

	// Mark connecting in local state — do NOT push this to FoxTrack
	// because "connected" isn't a real print status and confuses the UI
	UpdatePrinterState(p.Name, TelemetryData{
		Status:    "connected",
		PrinterID: p.Name,
		Timestamp: time.Now().Unix(),
	})

	// Block until connection-lost fires
	<-done
	client.Disconnect(250)
	return fmt.Errorf("connection lost")
}

func makeHandler(p Printer) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		var report BambuReport
		if err := json.Unmarshal(msg.Payload(), &report); err != nil {
			return
		}

		pr := report.Print
		if pr.GcodeState == "" {
			return // ignore messages that aren't status updates
		}

		status := mapGcodeState(pr.GcodeState)

		// Only act if something actually changed — BambuLab sends the same
		// status repeatedly; no need to hammer the webhook every time
		currentState := GetPrinterState(p.Name)
		changed := status != currentState.Status || pr.SubTaskName != currentState.FileName || pr.McPercent != currentState.Progress

		t := TelemetryData{
			Status:   status,
			FileName: pr.SubTaskName,
			Progress: pr.McPercent,
			Error:    pr.McPrintErrorCode,
		}
		UpdatePrinterState(p.Name, t)

		log.Printf("[%s] %s | %q | %d%%", p.Name, status, pr.SubTaskName, pr.McPercent)

		if !changed {
			return // skip webhook if nothing changed
		}

		if p.WebhookURL != "" && p.APIKey != "" {
			payload := webhook.Payload{
				PrinterName: p.Name,
				Serial:      p.Serial,
				Status:      status,
				FileName:    pr.SubTaskName,
				Progress:    pr.McPercent,
				ErrorCode:   pr.McPrintErrorCode,
				Timestamp:   time.Now().Unix(),
			}
			go func(payload webhook.Payload) {
				if err := webhook.Send(p.APIKey, p.WebhookURL, payload); err != nil {
					log.Printf("[%s] webhook error: %v", p.Name, err)
				}
			}(payload)
		} else {
			log.Printf("[%s] skipping webhook — API key or URL not configured", p.Name)
		}
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
