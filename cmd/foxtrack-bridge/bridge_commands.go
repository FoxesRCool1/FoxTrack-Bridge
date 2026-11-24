package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/pace"
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

type bridgeCommand struct {
	ID         string                 `json:"id"`
	ExternalID string                 `json:"external_id"`
	Command    string                 `json:"command"`
	Args       map[string]interface{} `json:"args"`
}

type bridgeCommandResult struct {
	CommandID    string          `json:"command_id"`
	Status       string          `json:"status"`                  // "running" (commands that run off the poll loop), "done" or "failed"
	ErrorMessage string          `json:"error_message,omitempty"` // driver error message when failed
	Data         json.RawMessage `json:"data,omitempty"`          // list_files: the files; FoxTrack stores it in bridge_commands.result
}

// bridgeCommandsReply is the bridge-commands GET reply. Watched and
// PollAfterMs are missing on FoxTrack servers from before 2026-09-30; Bridge
// then keeps its old pace (package pace). AppAlerts is missing on FoxTrack
// servers that do not send it; nil means absent.
type bridgeCommandsReply struct {
	Commands    []bridgeCommand     `json:"commands"`
	Watched     *bool               `json:"watched"`
	PollAfterMs int64               `json:"poll_after_ms"`
	AppAlerts   map[string][]string `json:"app_alerts"`
}

// httpStatusError is a non-2xx answer from FoxTrack.
type httpStatusError struct {
	code int
	body []byte // first 2 KB, for FoxTrack's reason
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// errNoMatchingPrinter is returned by executeBridgeCommand when no local printer
// matches the command's external_id. The poll loop skips the command silently:
// it may be intended for a different Bridge instance in a multi-Bridge workspace.
var errNoMatchingPrinter = errors.New("no matching printer")

var bridgeCommandsHTTPClient = &http.Client{Timeout: 8 * time.Second}

// bridgeCommandsWake cuts the poll's wait short, so a newly saved token is
// tried at once instead of after up to 5 minutes of refused-token backoff.
var bridgeCommandsWake = make(chan struct{}, 1)

func wakeBridgeCommands() {
	select {
	case bridgeCommandsWake <- struct{}{}:
	default: // a wake is already pending
	}
}

// pollBridgeCommands runs as a single long-lived goroutine. It fetches pending
// commands from FoxTrack, executes each locally, and POSTs the result back,
// then waits as long as FoxTrack asked (package pace): about 5 s while someone
// has a FoxTrack printer page open, 30 s otherwise, and the old 4 s on servers
// that do not say. A missing or empty API key silently skips each cycle.
func pollBridgeCommands() { pollBridgeCommandsUntil(nil) }

// pollBridgeCommandsUntil is the poll loop. It returns once stop is closed;
// the Bridge passes nil (never), tests pass a channel they close at cleanup.
func pollBridgeCommandsUntil(stop <-chan struct{}) {
	// Brief startup delay: lets the server bind and load its initial config.
	select {
	case <-time.After(3 * time.Second):
	case <-stop:
		return
	}

	for {
		delay := pace.PollLegacy
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[bridge-commands] panic: %v", r)
				}
			}()

			configMutex.RLock()
			apiKey := ""
			if configStore != nil {
				apiKey = configStore.FoxTrack2APIKey
			}
			configMutex.RUnlock()

			if apiKey == "" {
				return
			}

			reply, err := fetchBridgeCommands(apiKey)
			if err != nil {
				status := 0
				var statusErr *httpStatusError
				if errors.As(err, &statusErr) {
					status = statusErr.code
					if status == http.StatusUnauthorized || status == http.StatusForbidden {
						pace.Refused()
					}
				}
				delay = pace.ErrorDelay(status)
				log.Printf("[bridge-commands] fetch error: %v", err)
				return
			}
			pace.Update(reply.Watched != nil, reply.Watched != nil && *reply.Watched, time.Now().Unix())
			delay = pace.PollDelay(reply.PollAfterMs)
			if reply.AppAlerts != nil {
				storeAppAlerts(reply.AppAlerts, time.Now())
			}

			for _, cmd := range reply.Commands {
				if cmd.Command == "print_file" {
					// Long job: accept it here, run it off the poll loop.
					startPrintFile(apiKey, cmd)
					continue
				}
				if isFilesCommand(cmd.Command) {
					startFilesCommand(apiKey, cmd)
					continue
				}
				if isConsoleCommand(cmd.Command) {
					startConsoleCommand(apiKey, cmd)
					continue
				}
				startQuickCommand(apiKey, cmd)
			}
		}()

		select {
		case <-time.After(delay):
		case <-bridgeCommandsWake:
		case <-stop:
			return
		}
	}
}

// startQuickCommand runs a quick command (pause, resume, stop, the light, the
// plug, set_filament) on the poll loop, once per command id. FoxTrack re-sends
// a command on every poll until its ack lands, so without the dedupe a lost
// ack ran it again about 5 seconds later: a stale plug_off after the user had
// turned the plug back on. A command that failed to run is marked seen too:
// its failed ack is retried, the command is not. The ack retries run off the
// poll loop (ackWithRetry waits up to 3 minutes).
func startQuickCommand(apiKey string, cmd bridgeCommand) {
	if printJobs.hasSeen(cmd.ID) {
		return
	}
	execErr := executeBridgeCommand(cmd)
	if errors.Is(execErr, errNoMatchingPrinter) {
		// Not our command. Leave it pending for another Bridge instance.
		return
	}
	printJobs.markSeen(cmd.ID)
	result := bridgeCommandResult{CommandID: cmd.ID, Status: "done"}
	if execErr != nil {
		result.Status = "failed"
		result.ErrorMessage = execErr.Error()
		log.Printf("[bridge-commands] %s %s/%s failed: %v", cmd.ID, cmd.ExternalID, cmd.Command, execErr)
	} else {
		log.Printf("[bridge-commands] %s %s/%s done", cmd.ID, cmd.ExternalID, cmd.Command)
	}
	printJobs.wg.Add(1)
	go func() {
		defer printJobs.wg.Done()
		ackWithRetry(apiKey, result, func(format string, a ...interface{}) {
			log.Printf("[bridge-commands] %s %s/%s: %s", cmd.ID, cmd.ExternalID, cmd.Command, fmt.Sprintf(format, a...))
		})
	}()
}

func fetchBridgeCommands(apiKey string) (bridgeCommandsReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", webhook.BridgeCommandsURLV2, nil)
	if err != nil {
		return bridgeCommandsReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Bridge-Capabilities", bridgeCapabilities)
	req.Header.Set("X-Bridge-Version", version.AppVersion)

	configMutex.RLock()
	var nt *config.Notifications
	if configStore != nil {
		nt = configStore.Notifications
	}
	configMutex.RUnlock()
	req.Header.Set("X-Bridge-Notify", notifyHeader(nt))

	resp, err := bridgeCommandsHTTPClient.Do(req)
	if err != nil {
		return bridgeCommandsReply{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		webhook.NoteBridgeCommandsReply(resp.StatusCode, body)
		return bridgeCommandsReply{}, &httpStatusError{code: resp.StatusCode, body: body}
	}
	webhook.NoteBridgeCommandsReply(resp.StatusCode, nil)

	var reply bridgeCommandsReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return bridgeCommandsReply{}, err
	}
	return reply, nil
}

func ackBridgeCommand(apiKey string, result bridgeCommandResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", webhook.BridgeCommandsURLV2, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := bridgeCommandsHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{code: resp.StatusCode}
	}
	return nil
}

// isConsoleCommand reports the printer console commands. They run off the
// poll loop: a Klipper home answers only when G28 is done (up to 2 minutes),
// which would hold up pause and stop for every other printer.
func isConsoleCommand(command string) bool {
	return command == "home" || command == "set_nozzle_temp" || command == "set_bed_temp"
}

// startConsoleCommand runs on the poll loop: match, dedupe, ack "running", go. Deduped on the
// command id and acked with retries like the files commands, so a lost ack
// never runs a second G28 or re-applies a temperature the user since changed.
func startConsoleCommand(apiKey string, cmd bridgeCommand) {
	if printJobs.hasSeen(cmd.ID) {
		return
	}
	p, _, _, ok := findBridgePrinter(cmd.ExternalID)
	if !ok {
		return // another Bridge's printer: leave it pending
	}
	// "running" first: a Klipper home can take longer than the 2 minutes
	// FoxTrack leaves a command pending before it expires it.
	if !acceptBridgeCommand(apiKey, cmd, p.Name) {
		return
	}
	printJobs.wg.Add(1)
	go func() {
		defer printJobs.wg.Done()
		result := bridgeCommandResult{CommandID: cmd.ID, Status: "done"}
		if err := executeBridgeCommand(cmd); err != nil {
			result.Status = "failed"
			result.ErrorMessage = truncateChars(err.Error(), maxErrorChars)
			log.Printf("[bridge-commands] %s %s/%s failed: %v", cmd.ID, p.Name, cmd.Command, err)
		} else {
			log.Printf("[bridge-commands] %s %s/%s done", cmd.ID, p.Name, cmd.Command)
		}
		ackWithRetry(apiKey, result, func(format string, a ...interface{}) {
			log.Printf("[bridge-commands] %s %s/%s: %s", cmd.ID, p.Name, cmd.Command, fmt.Sprintf(format, a...))
		})
	}()
}

var errHomeWhilePrinting = errors.New("The printer is printing. Home it when the print has finished.")

var errHomeUnknown = errors.New("The printer is not reporting its state, so it is not homed. Try again when it shows as connected.")

// refuseHomeMidPrint stops a home command while a print runs: G28 mid-print
// drives the toolhead into the part. Temperatures stay allowed: users tune
// them during a print. Both FoxTrack and the dashboard go through this.
func refuseHomeMidPrint(name, command string) error {
	if command != "home" {
		return nil
	}
	// Allowed only when the printer reports a settled state: a print that is
	// starting (Bambu PREPARE) or a printer not reporting yet may be mid-print.
	s, ok := printerLiveState(name)
	if !ok || s == nil {
		return errHomeUnknown
	}
	if !mqttpkg.Settled(s.Status) {
		return errHomeWhilePrinting
	}
	return nil
}

// executeBridgeCommand resolves external_id to a local printer and dispatches
// the command directly to the appropriate driver, with no HTTP roundtrip through
// the bridge's own server.
//
// Bambu printers: external_id matches p.Serial (not p.Name)
// Other printers: external_id matches p.ID, or p.Name (see matchesExternalID)
func executeBridgeCommand(cmd bridgeCommand) error {
	configMutex.RLock()
	var matchedName string
	var isBambu bool
	if configStore != nil {
		for _, p := range configStore.Printers {
			if bambu := isBambuPrinterConfig(p); matchesExternalID(p, bambu, cmd.ExternalID) {
				matchedName = p.Name
				isBambu = bambu
				break
			}
		}
	}
	configMutex.RUnlock()

	if matchedName == "" {
		return errNoMatchingPrinter
	}

	if isPlugCommand(cmd.Command) {
		return runPlugCommandForFoxTrack(matchedName, cmd.Command)
	}
	if err := refuseHomeMidPrint(matchedName, cmd.Command); err != nil {
		return err
	}
	if isBambu {
		return mqttpkg.SendCommandWithArgs(matchedName, cmd.Command, cmd.Args)
	}
	return lanCtrl.SendCommand(matchedName, cmd.Command, cmd.Args)
}
