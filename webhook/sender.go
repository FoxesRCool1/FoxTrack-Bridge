package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

var relayHTTPClient = &http.Client{Timeout: 10 * time.Second}

// Payload is the JSON body sent to FoxTrack on every status change.
// Keep this in sync with the FoxTrack webhook endpoint schema.
type Payload struct {
	PrinterName   string  `json:"printer_name"`  // Matches the name in FoxTrack
	Serial        string  `json:"serial"`        // BambuLab serial number
	Status        string  `json:"status"`        // idle | printing | paused | finished | error | disconnected
	FileName      string  `json:"file_name"`     // Current file, empty if idle
	Progress      int     `json:"progress"`      // 0–100
	ErrorCode     string  `json:"error_code"`    // Empty string if no error
	Timestamp     int64   `json:"timestamp"`     // Unix seconds (UTC)
	NozzleTemp    float64 `json:"nozzle_temp"`   // Celsius
	NozzleTarget  float64 `json:"nozzle_target"` // Celsius
	BedTemp       float64 `json:"bed_temp"`      // Celsius
	BedTarget     float64 `json:"bed_target"`    // Celsius
	LightOn       bool    `json:"light_on"`
	TimeRemaining int     `json:"time_remaining"` // Minutes
}

// RelayPayload mirrors the Bambu relay payload shape.
type RelayPayload struct {
	Print RelayPrint `json:"print"`
}

type RelayPrint struct {
	GcodeState         string  `json:"gcode_state"`
	SubTaskName        string  `json:"subtask_name"`
	McPercent          int     `json:"mc_percent"`
	NozzleTemper       float64 `json:"nozzle_temper"`
	NozzleTargetTemper float64 `json:"nozzle_target_temper"`
	BedTemper          float64 `json:"bed_temper"`
	BedTargetTemper    float64 `json:"bed_target_temper"`
}

// Send posts a Payload to the FoxTrack webhook URL.
// The API key is sent as a Bearer token.
func Send(apiKey, webhookURL string, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := relayHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FoxTrack webhook returned HTTP %d", resp.StatusCode)
	}

	log.Printf("[webhook] Sent status for %s → %s (%d%%)", p.PrinterName, p.Status, p.Progress)
	return nil
}

// SendRelay posts a Bambu-shaped relay payload to the configured webhook endpoint.
func SendRelay(apiKey, webhookURL, printerSerial, printerName string, p RelayPayload) error {
	if !strings.HasPrefix(strings.ToLower(webhookURL), "https://") {
		return fmt.Errorf("relay webhook URL must use HTTPS")
	}

	body, err := json.Marshal(p)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Printer-Serial", printerSerial)
	req.Header.Set("X-Printer-Name", printerName)

	resp, err := relayHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("FoxTrack relay webhook returned HTTP %d", resp.StatusCode)
	}

	log.Printf("[relay] Sent relay payload for %s (%d%%)", printerName, p.Print.McPercent)
	return nil
}
