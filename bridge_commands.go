package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

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
	CommandID    string `json:"command_id"`
	Status       string `json:"status"`                 // "running" (print_file only), "done" or "failed"
	ErrorMessage string `json:"error_message,omitempty"` // driver error message when failed
}

// bridgeCommandsReply is the bridge-commands GET reply. Watched and
// PollAfterMs are missing on FoxTrack servers from before 2026-09-30; Bridge
// then keeps its old pace (package pace).
type bridgeCommandsReply struct {
	Commands    []bridgeCommand `json:"commands"`
	Watched     *bool           `json:"watched"`
	PollAfterMs int64           `json:"poll_after_ms"`
}

// httpStatusError is a non-2xx answer from FoxTrack.
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// errNoMatchingPrinter is returned by executeBridgeCommand when no local printer
// matches the command's external_id. The poll loop skips the command silently:
// it may be intended for a different Bridge instance in a multi-Bridge workspace.
var errNoMatchingPrinter = errors.New("no matching printer")

var bridgeCommandsHTTPClient = &http.Client{Timeout: 8 * time.Second}

// pollBridgeCommands runs as a single long-lived goroutine. It fetches pending
// commands from FoxTrack, executes each locally, and POSTs the result back,
// then waits as long as FoxTrack asked (package pace): about 5 s while someone
// has a FoxTrack printer page open, 30 s otherwise, and the old 4 s on servers
// that do not say. A missing or empty API key silently skips each cycle.
func pollBridgeCommands() {
	// Brief startup delay: lets the server bind and load its initial config.
	time.Sleep(3 * time.Second)

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

			for _, cmd := range reply.Commands {
				if cmd.Command == "print_file" {
					// Long job: accept it here, run it off the poll loop.
					startPrintFile(apiKey, cmd)
					continue
				}
				execErr := executeBridgeCommand(cmd)
				if errors.Is(execErr, errNoMatchingPrinter) {
					// Not our command. Leave it pending for another Bridge instance.
					continue
				}
				result := bridgeCommandResult{CommandID: cmd.ID, Status: "done"}
				if execErr != nil {
					result.Status = "failed"
					result.ErrorMessage = execErr.Error()
					log.Printf("[bridge-commands] %s %s/%s failed: %v", cmd.ID, cmd.ExternalID, cmd.Command, execErr)
				} else {
					log.Printf("[bridge-commands] %s %s/%s done", cmd.ID, cmd.ExternalID, cmd.Command)
				}
				if err := ackBridgeCommand(apiKey, result); err != nil {
					log.Printf("[bridge-commands] ack error for %s: %v", cmd.ID, err)
				}
			}
		}()

		time.Sleep(delay)
	}
}

func fetchBridgeCommands(apiKey string) (bridgeCommandsReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", webhook.BridgeCommandsURLV2, nil)
	if err != nil {
		return bridgeCommandsReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Bridge-Capabilities", "print_file")
	req.Header.Set("X-Bridge-Version", version.AppVersion)

	resp, err := bridgeCommandsHTTPClient.Do(req)
	if err != nil {
		return bridgeCommandsReply{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bridgeCommandsReply{}, &httpStatusError{code: resp.StatusCode}
	}

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

// executeBridgeCommand resolves external_id to a local printer and dispatches
// the command directly to the appropriate driver, with no HTTP roundtrip through
// the bridge's own server.
//
// Bambu printers: external_id matches p.Serial (not p.Name)
// Klipper printers: external_id matches p.Name
func executeBridgeCommand(cmd bridgeCommand) error {
	configMutex.RLock()
	var matchedName string
	var isBambu bool
	for _, p := range configStore.Printers {
		if isBambuPrinterConfig(p) {
			if p.Serial == cmd.ExternalID {
				matchedName = p.Name
				isBambu = true
				break
			}
		} else {
			if p.Name == cmd.ExternalID {
				matchedName = p.Name
				break
			}
		}
	}
	configMutex.RUnlock()

	if matchedName == "" {
		return errNoMatchingPrinter
	}

	if isBambu {
		return mqttpkg.SendCommandWithArgs(matchedName, cmd.Command, cmd.Args)
	}
	return lanCtrl.SendCommand(matchedName, cmd.Command, cmd.Args)
}
