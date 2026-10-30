package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// URL is the FoxTrack primary telemetry endpoint. Same for all users. Do not expose in UI or config.
const URL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bambu-local-relay"

// SyncURL is the FoxTrack printer registration endpoint, called on bridge startup.
const SyncURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bambu-local-sync"

// SnapshotURL is the FoxTrack endpoint that accepts periodic JPEG snapshots from the bridge.
const SnapshotURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-snapshot"

// HistoryURL is the FoxTrack endpoint that receives print-job completion events.
const HistoryURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-history"

// BridgeCommandsURL is the FoxTrack endpoint for polling and acknowledging pending bridge commands.
const BridgeCommandsURL = "https://vcnedcbtnhpmjgneahyk.supabase.co/functions/v1/bridge-commands"

// defaultV2Base is the current FoxTrack project (cyifamyotqkwcjbbpnhq).
const defaultV2Base = "https://cyifamyotqkwcjbbpnhq.supabase.co"

// v2BaseEnv points the four V2 URLs at another FoxTrack project (the beta one,
// or a local Supabase stack) for testing. Ignored unless it passes URLAllowed.
const v2BaseEnv = "FOXTRACK_SUPABASE_URL"

var v2Base = resolveV2Base(os.Getenv(v2BaseEnv))

// The V2 URLs are set once at startup and never change afterwards.
var (
	RelayURLV2          = v2Base + "/functions/v1/bambu-local-relay"
	SnapshotURLV2       = v2Base + "/functions/v1/bridge-snapshot"
	HistoryURLV2        = v2Base + "/functions/v1/bridge-history"
	BridgeCommandsURLV2 = v2Base + "/functions/v1/bridge-commands"
)

// resolveV2Base returns the FoxTrack base URL to use for the given override
// ("" = none) and logs when the override is active or refused.
func resolveV2Base(override string) string {
	override = strings.TrimSpace(override)
	if override == "" {
		return defaultV2Base
	}
	if !URLAllowed(override) {
		log.Printf("Warning: ignoring %s=%q: it must be https://..., or http://127.0.0.1 or http://localhost", v2BaseEnv, override)
		return defaultV2Base
	}
	log.Printf("[config] FoxTrack URL override active (%s): %s", v2BaseEnv, override)
	return strings.TrimRight(override, "/")
}

// URLAllowed reports whether raw is an https URL, or an http URL to this
// computer (127.0.0.1, localhost, ::1), which is only used for local testing.
func URLAllowed(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		h := strings.ToLower(u.Hostname())
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

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

// NoteBridgeCommandsReply takes FoxTrack's answer to the command poll, so a
// refused token or plan shows on the dashboard before any printer has
// reported, and clears as soon as FoxTrack accepts the token again. A printer
// limit is the relay's alone and stays until the relay clears it.
func NoteBridgeCommandsReply(status int, body []byte) {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		notePermanentReject(BridgeCommandsURLV2, status, body, "")
		return
	}
	if status < 200 || status >= 300 {
		return
	}
	relayProblemMu.Lock()
	defer relayProblemMu.Unlock()
	if relayProblem != nil && (relayProblem.Kind == "unauthorized" || relayProblem.Kind == "plan_limit") {
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
		msg := "FoxTrack did not accept this Bridge token. Generate a new one in FoxTrack under Settings > Integrations > FoxTrack Bridge, then paste it into Settings here."
		setRelayProblem("unauthorized", msg, printer)
		return msg

	case status == 403 && payload.Error == "printer_limit_reached":
		msg := fmt.Sprintf("Your FoxTrack plan (%s) allows %s printers and you already have %d. New printers will not appear until you upgrade or remove one.",
			planName(payload.Plan), formatLimit(payload.MaxLimit), payload.CurrentCount)
		setRelayProblem("printer_limit", msg, printer)
		return msg

	case status == 403:
		// verify_bridge_token raises plan_limit for a workspace without the
		// Bridge feature; the receiver forwards its text.
		msg := strings.TrimSpace(payload.Message)
		if msg == "" {
			msg = "FoxTrack Bridge needs the Pro or Farm plan. Nothing is sent to FoxTrack until this workspace upgrades."
		}
		msg = strings.TrimPrefix(msg, "plan_limit: ")
		setRelayProblem("plan_limit", msg, printer)
		return msg
	}

	return ""
}

// planName is the plan as FoxTrack shows it: the plan value "enterprise" is
// called Farm there.
func planName(plan string) string {
	switch plan {
	case "enterprise", "lifetime":
		return "Farm"
	case "":
		return "current"
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}

func formatLimit(max *int) string {
	if max == nil {
		return "unlimited"
	}
	return fmt.Sprintf("%d", *max)
}

// readCappedBody reads at most 2 KB of an error response: enough for the JSON
// the receivers return, without pulling an unbounded body into memory.
func readCappedBody(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return b
}

// relayState orders the relay posts of one printer to one endpoint. FoxTrack
// replaces its stored status with every post, so an older payload landing after
// a newer one (a late retry, or two goroutines racing) would roll the printer
// back to a status it already left.
type relayState struct {
	seq       atomic.Uint64 // last sequence handed out, one per SendRelay call
	mu        sync.Mutex    // serialises sends for this printer
	delivered uint64        // highest sequence FoxTrack accepted; guarded by mu
}

var relayStates sync.Map // webhookURL + "\x00" + serial -> *relayState

func relayStateFor(webhookURL, serial string) *relayState {
	v, _ := relayStates.LoadOrStore(webhookURL+"\x00"+serial, &relayState{})
	return v.(*relayState)
}

// retryItem is a relay call: the first attempt, or one waiting to be retried.
type retryItem struct {
	apiKey, webhookURL, serial, name string
	payload                          RelayPayload
	attempts                         int
	createdAt                        time.Time
	st                               *relayState
	seq                              uint64
}

// relayBackoffs are the waits before each relay retry. A var so tests can
// shrink them.
var relayBackoffs = []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}

var pendingRelayRetries atomic.Int32

// deliverRelay sends item unless a newer payload for the same printer has
// already been delivered, in which case it reports skipped and sends nothing.
func deliverRelay(item retryItem) (err error, skipped bool) {
	item.st.mu.Lock()
	defer item.st.mu.Unlock()
	if item.st.delivered > item.seq {
		return nil, true
	}
	if err = doSendRelay(item.apiKey, item.webhookURL, item.serial, item.name, item.payload); err == nil {
		item.st.delivered = item.seq
	}
	return err, false
}

// scheduleRelayRetry runs item after its backoff on its own timer, so one
// printer's wait never delays another's. At most maxPendingRetries wait at once.
func scheduleRelayRetry(item retryItem) {
	if pendingRelayRetries.Add(1) > maxPendingRetries {
		pendingRelayRetries.Add(-1)
		log.Printf("[relay-retry] queue full, dropping retry for %s", item.name)
		return
	}
	time.AfterFunc(relayBackoffs[item.attempts-1], func() {
		pendingRelayRetries.Add(-1)
		runRelayRetry(item)
	})
}

// runRelayRetry retries a failed relay call. Items older than 5 minutes are
// discarded, and so are items a newer delivered payload has replaced.
func runRelayRetry(item retryItem) {
	if time.Since(item.createdAt) > 5*time.Minute {
		log.Printf("[relay-retry] dropping stale payload for %s (created %s ago)", item.name, time.Since(item.createdAt).Round(time.Second))
		return
	}
	err, skipped := deliverRelay(item)
	if skipped {
		log.Printf("[relay-retry] dropping payload for %s: a newer one was already sent", item.name)
		return
	}
	if err == nil {
		return
	}
	if errors.Is(err, errNotRetryable) {
		log.Printf("[relay-retry] dropping payload for %s: %v", item.name, err)
		return
	}
	if item.attempts < len(relayBackoffs) {
		item.attempts++
		scheduleRelayRetry(item)
	} else {
		log.Printf("[relay-retry] giving up on payload for %s after %d attempts", item.name, item.attempts)
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

	// Spool identity, Bambu only. TagUID and TrayUUID are blank for a spool
	// without a Bambu RFID tag.
	TagUID        string `json:"tag_uid,omitempty"`
	TrayUUID      string `json:"tray_uuid,omitempty"`
	TraySubBrands string `json:"tray_sub_brands,omitempty"` // e.g. "PLA Basic"
	TrayWeight    int    `json:"tray_weight,omitempty"`     // spool net weight in grams; 0 when unknown
	TrayInfoIdx   string `json:"tray_info_idx,omitempty"`   // Bambu filament profile id, e.g. "GFA00"
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
	LightOn            *bool          `json:"light_on,omitempty"`       // nil when not reported (Klipper)
	Ams                []RelayAmsSlot `json:"ams,omitempty"`            // nil for Klipper or printers without AMS
	ExternalSpool      *RelayAmsSlot  `json:"external_spool,omitempty"` // Bambu external spool (slot 254); nil until reported

	// Alarms is what the printer reports as wrong right now. Sent on EVERY
	// push, for every printer kind, as [] when there is none: FoxTrack clears
	// an open alarm the moment a push leaves it out, so a missing key would
	// clear and reopen them. doSendRelay turns nil into [].
	Alarms []RelayAlarm `json:"alarms"`
	// Plug is the smart plug the printer is on, when one is set in the Bridge.
	// Left out for a printer without one.
	Plug *RelayPlug `json:"plug,omitempty"`

	// What FoxTrack's Print dialog needs to list this printer.
	PrinterKind        string   `json:"printer_kind,omitempty"`        // "bambu" | "klipper" | "prusalink" | "octoprint" | "home_assistant"
	PrinterModel       string   `json:"printer_model,omitempty"`       // "Bambu Lab P1S", "Klipper"
	BridgeVersion      string   `json:"bridge_version,omitempty"`      // version.AppVersion
	BridgeCapabilities []string `json:"bridge_capabilities,omitempty"` // ["print_file", "set_filament"]; empty over Bambu Cloud
}

// RelayAlarm is one open alarm, in the shape FoxTrack's printer_alarms
// trigger reads. Code is the stable id FoxTrack keys the alarm on; with none,
// it keys on Message, so that text must stay the same from push to push.
type RelayAlarm struct {
	Source   string  `json:"source"`   // "hms" | "print_error" | "message"
	Code     *string `json:"code"`     // at most 40 chars, or null
	Severity string  `json:"severity"` // "fatal" | "serious" | "common" | "info"
	Message  *string `json:"message"`  // plain words, at most 300 chars, or null
}

// Alarm severities and sources, as FoxTrack stores them.
const (
	AlarmFatal   = "fatal"
	AlarmSerious = "serious"
	AlarmCommon  = "common"
	AlarmInfo    = "info"

	AlarmSourceHMS        = "hms"
	AlarmSourcePrintError = "print_error"
	AlarmSourceMessage    = "message"
)

const (
	maxAlarmCodeChars    = 40
	maxAlarmMessageChars = 300
	maxAlarms            = 20
)

// MessageAlarm builds a text-only alarm (Klipper, PrusaLink, OctoPrint).
func MessageAlarm(severity, message string) RelayAlarm {
	msg := TruncateRunes(strings.TrimSpace(message), maxAlarmMessageChars)
	return RelayAlarm{Source: AlarmSourceMessage, Severity: severity, Message: &msg}
}

// CodeAlarm builds an alarm keyed on a code; message may be "" for none.
func CodeAlarm(source, code, severity, message string) RelayAlarm {
	c := TruncateRunes(code, maxAlarmCodeChars)
	a := RelayAlarm{Source: source, Code: &c, Severity: severity}
	if m := TruncateRunes(strings.TrimSpace(message), maxAlarmMessageChars); m != "" {
		a.Message = &m
	}
	return a
}

// CleanAlarms applies FoxTrack's rules so a payload never flaps: never nil,
// no item without a code or a message, at most 20 items.
func CleanAlarms(in []RelayAlarm) []RelayAlarm {
	out := []RelayAlarm{}
	for _, a := range in {
		hasCode := a.Code != nil && strings.TrimSpace(*a.Code) != ""
		hasMsg := a.Message != nil && strings.TrimSpace(*a.Message) != ""
		if !hasCode && !hasMsg {
			continue
		}
		switch a.Severity {
		case AlarmFatal, AlarmSerious, AlarmCommon, AlarmInfo:
		default:
			a.Severity = AlarmCommon
		}
		switch a.Source {
		case AlarmSourceHMS, AlarmSourcePrintError, AlarmSourceMessage:
		default:
			a.Source = AlarmSourceMessage
		}
		out = append(out, a)
		if len(out) == maxAlarms {
			break
		}
	}
	return out
}

// TruncateRunes cuts s to at most n runes.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// RelayPlug is the smart plug state FoxTrack reads as status.plug.
type RelayPlug struct {
	On        bool     `json:"on"`
	Watts     *float64 `json:"watts"` // null when the plug has no meter
	Kind      string   `json:"kind"`  // "shelly" | "tasmota" | "kasa"
	AutoOff   bool     `json:"auto_off"`
	Reachable bool     `json:"reachable"` // false when the last poll failed; On is then the last known state
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

	// Filament use. PercentAtEnd is sent even at 0 (a print cancelled while
	// heating used none); only a Bridge older than v2.5.1 leaves it out.
	PercentAtEnd   int               `json:"percent_at_end"`             // progress when the print left printing; 100 when finished
	FilamentUsedMM float64           `json:"filament_used_mm,omitempty"` // Klipper: print_stats.filament_used at the end
	Filaments      []HistoryFilament `json:"filaments,omitempty"`        // Bambu: every AMS slot plus the external spool (254)
	// EnergyWh is what the printer's smart plug measured over the print, in
	// Wh to one decimal. Left out for a printer without a plug.
	EnergyWh *float64 `json:"energy_wh,omitempty"`
}

// HistoryFilament pairs one slot's spool at the start and end of a print, so
// FoxTrack can work out the grams used. Remain is a percent (0-100), -1 when
// the printer does not know; both are sent even when equal.
type HistoryFilament struct {
	Slot        int    `json:"slot"`
	Material    string `json:"material,omitempty"`
	Color       string `json:"color,omitempty"`
	TagUID      string `json:"tag_uid,omitempty"`
	TrayWeight  int    `json:"tray_weight,omitempty"`
	RemainStart int    `json:"remain_start"`
	RemainEnd   int    `json:"remain_end"`
}

// historyRetryItem holds a failed SendHistory call awaiting retry.
type historyRetryItem struct {
	apiKey     string
	historyURL string
	payload    HistoryPayload
	attempts   int
	createdAt  time.Time
}

// A finished print is worth keeping through a long FoxTrack outage, so history
// retries back off 2s, 8s, 32s... up to 10 minutes and last 24 hours. Vars so
// tests can shrink them.
var (
	historyBackoffBase = 2 * time.Second
	historyBackoffMax  = 10 * time.Minute
	historyMaxAge      = 24 * time.Hour
)

// maxPendingRetries bounds each retry set (relay, history) so an endless
// outage cannot grow memory without limit.
const maxPendingRetries = 64

var pendingHistoryRetries atomic.Int32

// historyDelay is the wait before retry number attempts (1-based).
func historyDelay(attempts int) time.Duration {
	d := historyBackoffBase
	for i := 1; i < attempts && d < historyBackoffMax; i++ {
		d *= 4
	}
	if d > historyBackoffMax {
		d = historyBackoffMax
	}
	return d
}

// scheduleHistoryRetry runs item after its backoff on its own timer. Items
// older than historyMaxAge are discarded.
// ponytail: retries live in memory only and do not survive a restart; persist
// them to disk if losing a history post across a restart ever matters.
func scheduleHistoryRetry(item historyRetryItem) {
	if pendingHistoryRetries.Add(1) > maxPendingRetries {
		pendingHistoryRetries.Add(-1)
		log.Printf("[history-retry] queue full, dropping retry for %s", item.payload.PrinterName)
		return
	}
	time.AfterFunc(historyDelay(item.attempts), func() {
		pendingHistoryRetries.Add(-1)
		runHistoryRetry(item)
	})
}

func runHistoryRetry(item historyRetryItem) {
	if time.Since(item.createdAt) > historyMaxAge {
		log.Printf("[history-retry] giving up on payload for %s (created %s ago)",
			item.payload.PrinterName, time.Since(item.createdAt).Round(time.Second))
		return
	}
	err := doSendHistory(item.apiKey, item.historyURL, item.payload)
	if err == nil {
		return
	}
	if errors.Is(err, errNotRetryable) {
		log.Printf("[history-retry] dropping payload for %s: %v", item.payload.PrinterName, err)
		return
	}
	item.attempts++
	scheduleHistoryRetry(item)
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
// and queues a retry with capped exponential backoff on failure.
func SendHistory(apiKey, historyURL string, p HistoryPayload) {
	if err := doSendHistory(apiKey, historyURL, p); err != nil {
		if errors.Is(err, errNotRetryable) {
			log.Printf("[history] %s: %v", p.PrinterName, err)
			return
		}
		log.Printf("[history] send failed for %s: %v; queuing retry", p.PrinterName, err)
		scheduleHistoryRetry(historyRetryItem{
			apiKey:     apiKey,
			historyURL: historyURL,
			payload:    p,
			attempts:   1,
			createdAt:  time.Now(),
		})
	}
}

// doSendRelay performs the actual HTTP POST for a relay payload.
func doSendRelay(apiKey, webhookURL, printerSerial, printerName string, p RelayPayload) error {
	p.Print.Alarms = CleanAlarms(p.Print.Alarms)
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
// Single attempt only: failures are logged by the caller, not queued for retry.
func SendSnapshot(apiKey, snapshotURL, serial, name string, jpegBytes []byte) error {
	if len(jpegBytes) == 0 {
		return fmt.Errorf("snapshot frame is empty")
	}
	// The receiver refuses anything over 2 MB with HTTP 413. A high-resolution
	// Klipper webcam can exceed that, and uploading a doomed frame every 25
	// seconds wastes the user's upstream bandwidth, so stop here and say why.
	if len(jpegBytes) > MaxSnapshotBytes {
		return fmt.Errorf("snapshot frame is %d bytes, over the %d byte limit; lower the camera resolution or JPEG quality", len(jpegBytes), MaxSnapshotBytes)
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

// UploadURLV2 is the FoxTrack endpoint that takes a sliced file from the
// virtual printer (bridge-upload).
var UploadURLV2 = v2Base + "/functions/v1/bridge-upload"

// SendUpload posts one sliced file to FoxTrack and returns FoxTrack's status
// code and body. One attempt, no retry: the slicer is waiting for the answer.
// body is streamed; size must be its exact length.
func SendUpload(ctx context.Context, apiKey, uploadURL, fileName string, body io.Reader, size int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, body)
	if err != nil {
		return 0, nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-File-Name", url.PathEscape(fileName))
	resp, err := uploadHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, readCappedBody(resp.Body), nil
}

// uploadHTTPClient has no overall timeout: a 50 MB file over a slow uplink
// takes a while, and the caller's context ends it.
var uploadHTTPClient = &http.Client{}

// SendRelay posts a relay payload and queues a retry with backoff on failure.
// Each call gets the next sequence number for its printer; see relayState.
func SendRelay(apiKey, webhookURL, printerSerial, printerName string, p RelayPayload) error {
	if !URLAllowed(webhookURL) {
		return fmt.Errorf("relay webhook URL must use HTTPS")
	}
	st := relayStateFor(webhookURL, printerSerial)
	return sendRelaySeq(retryItem{
		apiKey: apiKey, webhookURL: webhookURL, serial: printerSerial, name: printerName,
		payload: p, attempts: 1, createdAt: time.Now(), st: st, seq: st.seq.Add(1),
	})
}

// sendRelaySeq makes the first attempt for item and queues a retry on failure.
func sendRelaySeq(item retryItem) error {
	err, _ := deliverRelay(item)
	if err == nil {
		return nil
	}
	// A revoked token or a plan gate answers the same way every time;
	// retrying it three times only delays the dashboard warning.
	if errors.Is(err, errNotRetryable) {
		log.Printf("[relay] %s: %v", item.name, err)
		return err
	}
	scheduleRelayRetry(item)
	return err
}
