package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// URL is the FoxTrack primary telemetry endpoint. Same for all users — do not expose in UI or config.
const URL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bambu-local-relay"

// SyncURL is the FoxTrack printer registration endpoint, called on bridge startup.
const SyncURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bambu-local-sync"

// SnapshotURL is the FoxTrack endpoint that accepts periodic JPEG snapshots from the bridge.
const SnapshotURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-snapshot"

// HistoryURL is the FoxTrack endpoint that receives print-job completion events.
const HistoryURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-history"

// BridgeCommandsURL is the FoxTrack endpoint for polling and acknowledging pending bridge commands.
const BridgeCommandsURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-commands"

// V2 constants point at the new FoxTrack project (cyifamyotqkwcjbbpnhq).
const RelayURLV2 = "https://cyifamyotqkwcjbbpnhq.supabase.co/functions/v1/bambu-local-relay"
const SnapshotURLV2 = "https://cyifamyotqkwcjbbpnhq.supabase.co/functions/v1/bridge-snapshot"
const HistoryURLV2 = "https://cyifamyotqkwcjbbpnhq.supabase.co/functions/v1/bridge-history"
const BridgeCommandsURLV2 = "https://cyifamyotqkwcjbbpnhq.supabase.co/functions/v1/bridge-commands"

// MaxSnapshotBytes mirrors the 2 MB cap the FoxTrack bridge-snapshot endpoint
// enforces. Keep the two in step.
const MaxSnapshotBytes = 2 * 1024 * 1024

var relayHTTPClient = &http.Client{Timeout: 10 * time.Second}

// --- Relay health -----------------------------------------------------------

// RelayProblem describes a rejection FoxTrack will keep returning until the
// user does something about it: a revoked or mistyped token, a workspace that
// lost the Bridge feature, or a printer count over the plan's limit. Retrying
// cannot clear any of them, so the bridge records them for the dashboard
// instead of burning three attempts and dropping the payload in silence.
type RelayProblem struct {
	Kind    string `json:"kind"`    // "plan_limit" | "printer_limit" | "unauthorized"
	Message string `json:"message"` // shown to the user as-is
	Printer string `json:"printer"` // the printer whose send was refused
	Since   string `json:"since"`   // RFC 3339, when this problem was first seen
}

var (
	relayProblemMu sync.Mutex
	relayProblem   *RelayProblem
)

// errNotRetryable marks a rejection the retry queue must drop rather than
// re-send, because the next attempt gets the same answer.
var errNotRetryable = errors.New("not retryable")

// RelayHealth returns the current unrecoverable relay problem, or nil when the
// most recent send to the current FoxTrack project succeeded.
func RelayHealth() *RelayProblem {
	relayProblemMu.Lock()
	defer relayProblemMu.Unlock()
	if relayProblem == nil {
		return nil
	}
	p := *relayProblem
	return &p
}

// setRelayProblem records a problem, keeping the original Since so the
// dashboard can say how long the relay has been refused.
func setRelayProblem(kind, message, printer string) {
	relayProblemMu.Lock()
	defer relayProblemMu.Unlock()
	if relayProblem != nil && relayProblem.Kind == kind {
		relayProblem.Message = message
		relayProblem.Printer = printer
		return
	}
	relayProblem = &RelayProblem{
		Kind:    kind,
		Message: message,
		Printer: printer,
		Since:   time.Now().UTC().Format(time.RFC3339),
	}
	log.Printf("[relay-health] %s: %s", kind, message)
}

func clearRelayProblem() {
	relayProblemMu.Lock()
	defer relayProblemMu.Unlock()
	if relayProblem != nil {
		log.Printf("[relay-health] cleared (%s resolved)", relayProblem.Kind)
		relayProblem = nil
	}
}

// isCurrentProject reports whether u belongs to the current FoxTrack project.
// Health is tracked only for those: the bridge still dual-writes to the legacy
// project, and a stale legacy token must not raise an alarm about a perfectly
// healthy current connection.
func isCurrentProject(u string) bool {
	for _, known := range currentProjectURLs {
		if u == known {
			return true
		}
	}
	return false
}

// currentProjectURLs is a var, not a switch, so tests can point it at a local
// httptest server.
var currentProjectURLs = []string{RelayURLV2, SnapshotURLV2, HistoryURLV2, BridgeCommandsURLV2}

// notePermanentReject inspects a non-2xx response. When the status means the
// user must act, it records the problem and returns a message; otherwise it
// returns "" and the caller retries as usual.
func notePermanentReject(endpoint string, status int, body []byte, printer string) string {
	if !isCurrentProject(endpoint) {
		return ""
	}

	var payload struct {
		Error        string `json:"error"`
		Message      string `json:"message"`
		CurrentCount int    `json:"currentCount"`
		MaxLimit     *int   `json:"maxLimit"`
		Plan         string `json:"plan"`
	}
	_ = json.Unmarshal(body, &payload)

	switch {
	case status == 401:
		msg := "FoxTrack rejected this bridge token. Generate a new one in FoxTrack under Settings → Bridge, then paste it into Settings here."
		setRelayProblem("unauthorized", msg, printer)
		return msg

	case status == 403 && payload.Error == "printer_limit_reached":
		msg := fmt.Sprintf("Your FoxTrack plan (%s) allows %s printers and you already have %d. New printers will not appear until you upgrade or remove one.",
			payload.Plan, formatLimit(payload.MaxLimit), payload.CurrentCount)
		setRelayProblem("printer_limit", msg, printer)
		return msg

	case status == 403:
		// verify_bridge_token raises plan_limit for a workspace without the
		// Bridge feature; the receiver forwards its text.
		msg := strings.TrimSpace(payload.Message)
		if msg == "" {
			msg = "FoxTrack Bridge requires a Pro or Enterprise plan. Telemetry is paused until this workspace upgrades."
		}
		msg = strings.TrimPrefix(msg, "plan_limit: ")
		setRelayProblem("plan_limit", msg, printer)
		return msg
	}

	return ""
}

func formatLimit(max *int) string {
	if max == nil {
		return "unlimited"
	}
	return fmt.Sprintf("%d", *max)
}

// readCappedBody reads at most 2 KB of an error response — enough for the JSON
// the receivers return, without pulling an unbounded body into memory.
func readCappedBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return b
}

// retryItem holds a failed relay call that should be retried.
type retryItem struct {
	apiKey, webhookURL, serial, name string
	payload                          RelayPayload
	attempts                         int
	after                            time.Time
	createdAt                        time.Time
}

// retryQueue is a bounded channel for failed relay payloads awaiting retry.
// If full, new failures are dropped to prevent unbounded memory growth.
var retryQueue = make(chan retryItem, 64)

func init() {
	go retryWorker()
	go historyRetryWorker()
}

// retryWorker retries failed relay calls with exponential backoff (2s, 8s, 30s).
// Items older than 5 minutes are discarded.
func retryWorker() {
	for item := range retryQueue {
		wait := time.Until(item.after)
		if wait > 0 {
			time.Sleep(wait)
		}
		if time.Since(item.createdAt) > 5*time.Minute {
			log.Printf("[relay-retry] dropping stale payload for %s (created %s ago)", item.name, time.Since(item.createdAt).Round(time.Second))
			continue
		}
		if err := doSendRelay(item.apiKey, item.webhookURL, item.serial, item.name, item.payload); err != nil {
			if errors.Is(err, errNotRetryable) {
				log.Printf("[relay-retry] dropping payload for %s: %v", item.name, err)
				continue
			}
			backoffs := []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}
			if item.attempts < len(backoffs) {
				next := item
				next.attempts++
				next.after = time.Now().Add(backoffs[item.attempts])
				select {
				case retryQueue <- next:
				default:
					log.Printf("[relay-retry] queue full, dropping retry for %s", item.name)
				}
			} else {
				log.Printf("[relay-retry] giving up on payload for %s after %d attempts", item.name, item.attempts)
			}
		}
	}
}

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

// RelayAmsSlot carries per-tray AMS state in the relay payload.
// Defined here (not in the mqtt package) to avoid a circular import.
type RelayAmsSlot struct {
	Slot      int    `json:"slot"`
	Color     string `json:"color"`     // 6-char hex, e.g. "FF0000"
	Material  string `json:"material"`  // e.g. "PLA", "PETG"
	Remaining int    `json:"remaining"` // 0-100% filament remaining
	Active    bool   `json:"active"`    // currently printing from this slot
}

type RelayPrint struct {
	GcodeState         string         `json:"gcode_state"`
	SubTaskName        string         `json:"subtask_name"`
	McPercent          int            `json:"mc_percent"`
	NozzleTemper       float64        `json:"nozzle_temper"`
	NozzleTargetTemper float64        `json:"nozzle_target_temper"`
	BedTemper          float64        `json:"bed_temper"`
	BedTargetTemper    float64        `json:"bed_target_temper"`
	McRemainingTime    int            `json:"mc_remaining_time,omitempty"` // minutes
	ActiveExtruder     string         `json:"active_extruder,omitempty"`
	LightOn            *bool          `json:"light_on,omitempty"` // nil when not reported (Klipper)
	Ams                []RelayAmsSlot `json:"ams,omitempty"`      // nil for Klipper or printers without AMS
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

// HistoryPayload is sent to FoxTrack when a print job completes.
// StartTime, EndTime, and Timestamp are RFC 3339 strings (Go time.Time default
// JSON encoding) so that Postgres can cast them to timestamptz directly.
type HistoryPayload struct {
	Type        string  `json:"type"` // always "print_complete"
	PrinterName string  `json:"printer_name"`
	Serial      string  `json:"serial"`
	FileName    string  `json:"file_name"`
	NozzleTemp  float64 `json:"nozzle_temp"`
	BedTemp     float64 `json:"bed_temp"`
	StartTime   string  `json:"start_time"` // RFC 3339
	EndTime     string  `json:"end_time"`   // RFC 3339
	Duration    int64   `json:"duration"`   // whole seconds
	Result      string  `json:"result"`     // "finished" | "cancelled" | "error"
	Timestamp   string  `json:"timestamp"`  // RFC 3339
}

// historyRetryItem holds a failed SendHistory call awaiting retry.
type historyRetryItem struct {
	apiKey     string
	historyURL string
	payload    HistoryPayload
	attempts   int
	after      time.Time
	createdAt  time.Time
}

// historyRetryQueue is a bounded channel for history payloads awaiting retry.
var historyRetryQueue = make(chan historyRetryItem, 64)

// historyRetryWorker retries failed SendHistory calls with exponential backoff
// (2s, 8s, 30s). Items older than 5 minutes are discarded.
func historyRetryWorker() {
	for item := range historyRetryQueue {
		if wait := time.Until(item.after); wait > 0 {
			time.Sleep(wait)
		}
		if time.Since(item.createdAt) > 5*time.Minute {
			log.Printf("[history-retry] dropping stale payload for %s (created %s ago)",
				item.payload.PrinterName, time.Since(item.createdAt).Round(time.Second))
			continue
		}
		if err := doSendHistory(item.apiKey, item.historyURL, item.payload); err != nil {
			if errors.Is(err, errNotRetryable) {
				log.Printf("[history-retry] dropping payload for %s: %v", item.payload.PrinterName, err)
				continue
			}
			backoffs := []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}
			if item.attempts < len(backoffs) {
				next := item
				next.attempts++
				next.after = time.Now().Add(backoffs[item.attempts])
				select {
				case historyRetryQueue <- next:
				default:
					log.Printf("[history-retry] queue full, dropping retry for %s", item.payload.PrinterName)
				}
			} else {
				log.Printf("[history-retry] giving up on payload for %s after %d attempts", item.payload.PrinterName, item.attempts)
			}
		}
	}
}

// doSendHistory performs the actual HTTP POST for a history payload.
func doSendHistory(apiKey, historyURL string, p HistoryPayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", historyURL, bytes.NewBuffer(body))
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
		if msg := notePermanentReject(historyURL, resp.StatusCode, readCappedBody(resp.Body), p.PrinterName); msg != "" {
			return fmt.Errorf("%w: %s", errNotRetryable, msg)
		}
		return fmt.Errorf("FoxTrack history endpoint returned HTTP %d", resp.StatusCode)
	}
	log.Printf("[history] sent %s for %s (%s)", p.Result, p.PrinterName, p.FileName)
	return nil
}

// SendHistory posts a HistoryPayload to the FoxTrack bridge-history endpoint
// and queues a retry with exponential backoff on failure.
func SendHistory(apiKey, historyURL string, p HistoryPayload) {
	if err := doSendHistory(apiKey, historyURL, p); err != nil {
		if errors.Is(err, errNotRetryable) {
			log.Printf("[history] %s: %v", p.PrinterName, err)
			return
		}
		log.Printf("[history] send failed for %s: %v — queuing retry", p.PrinterName, err)
		select {
		case historyRetryQueue <- historyRetryItem{
			apiKey:     apiKey,
			historyURL: historyURL,
			payload:    p,
			attempts:   1,
			after:      time.Now().Add(2 * time.Second),
			createdAt:  time.Now(),
		}:
		default:
			log.Printf("[history] retry queue full, dropping payload for %s", p.PrinterName)
		}
	}
}

// doSendRelay performs the actual HTTP POST for a relay payload.
func doSendRelay(apiKey, webhookURL, printerSerial, printerName string, p RelayPayload) error {
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
		if msg := notePermanentReject(webhookURL, resp.StatusCode, readCappedBody(resp.Body), printerName); msg != "" {
			return fmt.Errorf("%w: %s", errNotRetryable, msg)
		}
		return fmt.Errorf("FoxTrack relay webhook returned HTTP %d", resp.StatusCode)
	}
	if isCurrentProject(webhookURL) {
		clearRelayProblem()
	}
	log.Printf("[relay] Sent relay payload for %s (%d%%)", printerName, p.Print.McPercent)
	return nil
}

// SendSnapshot POSTs a raw JPEG frame to the FoxTrack snapshot endpoint.
// Single attempt only — failures are logged by the caller, not queued for retry.
func SendSnapshot(apiKey, snapshotURL, serial, name string, jpegBytes []byte) error {
	if len(jpegBytes) == 0 {
		return fmt.Errorf("snapshot frame is empty")
	}
	// The receiver refuses anything over 2 MB with HTTP 413. A high-resolution
	// Klipper webcam can exceed that, and uploading a doomed frame every 25
	// seconds wastes the user's upstream bandwidth — so stop here and say why.
	if len(jpegBytes) > MaxSnapshotBytes {
		return fmt.Errorf("snapshot frame is %d bytes, over the %d byte limit — lower the camera resolution or JPEG quality", len(jpegBytes), MaxSnapshotBytes)
	}

	req, err := http.NewRequest("POST", snapshotURL, bytes.NewReader(jpegBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Printer-Serial", serial)
	req.Header.Set("X-Printer-Name", name)
	resp, err := relayHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg := notePermanentReject(snapshotURL, resp.StatusCode, readCappedBody(resp.Body), name); msg != "" {
			return fmt.Errorf("%w: %s", errNotRetryable, msg)
		}
		return fmt.Errorf("snapshot endpoint returned HTTP %d", resp.StatusCode)
	}
	log.Printf("[snapshot] sent frame for %s (%d bytes)", name, len(jpegBytes))
	return nil
}

// SendRelay posts a relay payload and queues a retry with backoff on failure.
func SendRelay(apiKey, webhookURL, printerSerial, printerName string, p RelayPayload) error {
	if !strings.HasPrefix(strings.ToLower(webhookURL), "https://") {
		return fmt.Errorf("relay webhook URL must use HTTPS")
	}
	if err := doSendRelay(apiKey, webhookURL, printerSerial, printerName, p); err != nil {
		// A revoked token or a plan gate answers the same way every time;
		// retrying it three times only delays the dashboard warning.
		if errors.Is(err, errNotRetryable) {
			log.Printf("[relay] %s: %v", printerName, err)
			return err
		}
		select {
		case retryQueue <- retryItem{
			apiKey:     apiKey,
			webhookURL: webhookURL,
			serial:     printerSerial,
			name:       printerName,
			payload:    p,
			attempts:   1,
			after:      time.Now().Add(2 * time.Second),
			createdAt:  time.Now(),
		}:
		default:
			log.Printf("[relay] retry queue full, dropping payload for %s", printerName)
		}
		return err
	}
	return nil
}
