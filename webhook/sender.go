package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Payload is the JSON body sent to FoxTrack on every status change.
// Keep this in sync with the FoxTrack webhook endpoint schema.
type Payload struct {
	PrinterName string `json:"printer_name"` // Matches the name in FoxTrack
	Serial      string `json:"serial"`       // BambuLab serial number
	Status      string `json:"status"`       // idle | printing | paused | finished | error | disconnected
	FileName    string `json:"file_name"`    // Current file, empty if idle
	Progress    int    `json:"progress"`     // 0–100
	ErrorCode   string `json:"error_code"`   // Empty string if no error
	Timestamp   int64  `json:"timestamp"`    // Unix seconds (UTC)
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

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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
