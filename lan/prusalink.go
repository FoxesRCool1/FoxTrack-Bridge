package lan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

// PrusaLink (Prusa MK4, XL, MINI, Core One) shares the Klipper poll loop,
// history and relay pacing in lan.go; only the HTTP calls differ. It takes the
// same X-Api-Key header as Moonraker, so moonrakerAuthHeaders serves both.

var (
	errPrusaUnreachable = errors.New("Bridge could not reach the printer while sending the file. Check that the printer is on and PrusaLink is enabled.")
	errPrusaRefused     = errors.New("The printer refused the file. Check that a USB drive is in the printer and has free space.")
	errNoPrusaJob       = errors.New("The printer has no print running.")
)

func prusaLinkURL(p configpkg.Printer, path string) string {
	return strings.TrimRight(strings.TrimSpace(p.PrusaLinkURL), "/") + path
}

func fetchPrusaLinkTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	m, err := sendJSONRequest(client, "GET", prusaLinkURL(p, "/api/v1/status"), headers, nil)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}
	printer := nestedMap(m, "printer")
	job := nestedMap(m, "job")

	fileName := ""
	if len(job) > 0 {
		// The file name is only on /api/v1/job. Without it the print still reports.
		if j, err := sendJSONRequest(client, "GET", prusaLinkURL(p, "/api/v1/job"), headers, nil); err == nil {
			file := nestedMap(j, "file")
			if fileName = stringAny(file["display_name"]); fileName == "" {
				fileName = stringAny(file["name"])
			}
		}
	}

	t := mqttpkg.TelemetryData{
		Status:        normalizeState(stringAny(printer["state"])),
		FileName:      fileName,
		Progress:      clampPercent(floatAny(job["progress"])),
		NozzleTemp:    floatAny(printer["temp_nozzle"]),
		NozzleTarget:  floatAny(printer["target_nozzle"]),
		BedTemp:       floatAny(printer["temp_bed"]),
		BedTarget:     floatAny(printer["target_bed"]),
		TimeRemaining: max(0, int(floatAny(job["time_remaining"]))/60),
		PrinterModel:  "PrusaLink",
	}
	return t, relayFor(t, configpkg.KindPrusaLink, []string{"print_file"}), nil
}

// sendPrusaLinkCommand runs pause, resume or stop on the current job. The job
// id is read fresh, so a command never lands on a print that already ended.
func sendPrusaLinkCommand(p configpkg.Printer, command string) error {
	method, action := "PUT", ""
	switch command {
	case "pause":
		action = "/pause"
	case "resume":
		action = "/resume"
	case "stop":
		method = "DELETE"
	default:
		return fmt.Errorf("unsupported command for PrusaLink: %s", command)
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	m, err := sendJSONRequest(client, "GET", prusaLinkURL(p, "/api/v1/status"), headers, nil)
	if err != nil {
		return err
	}
	id, ok := nestedMap(m, "job")["id"]
	if !ok {
		return errNoPrusaJob
	}
	_, err = sendJSONRequest(client, method, prusaLinkURL(p, fmt.Sprintf("/api/v1/job/%d%s", int64(floatAny(id)), action)), headers, nil)
	return err
}

// prusaLinkUpload puts the file on the printer's USB drive and starts it in one
// request (PUT /api/v1/files/usb/<name>, Print-After-Upload). PrusaLink answers
// 201 once the file is stored; it has no started/queued reply like Moonraker.
func prusaLinkUpload(ctx context.Context, p configpkg.Printer, localPath, remoteName string, logf func(string, ...any)) error {
	if err := keyPreflight(ctx, p, prusaLinkURL(p, "/api/v1/status")); err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		logf("open file: %v", err)
		return errLocalFile
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		logf("stat file: %v", err)
		return errLocalFile
	}
	target := prusaLinkURL(p, "/api/v1/files/usb/"+url.PathEscape(SanitizeRemoteName(remoteName)))
	req, err := http.NewRequestWithContext(ctx, "PUT", target, f)
	if err != nil {
		logf("upload request: %v", err)
		return errNoAddress
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Print-After-Upload", "?1")
	req.Header.Set("Overwrite", "?1")
	applyMoonrakerAuth(req, p)

	resp, err := (&http.Client{Timeout: uploadTimeout, Transport: insecureTransport()}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // the job's timeout: the caller words it
		}
		logf("upload failed: %v", err)
		return errPrusaUnreachable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errMoonKey
	case resp.StatusCode == http.StatusConflict:
		return errPrinterBusy
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		logf("upload refused: HTTP %d %.300s", resp.StatusCode, body)
		return errPrusaRefused
	}
	return nil
}

// normalizeState turns a PrusaLink or Home Assistant state word into the
// status words the Klipper path uses. Unknown words pass through lowercased.
func normalizeState(raw string) string {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "idle", "standby", "ready", "operational":
		return "idle"
	case "printing", "running", "prepare":
		return "printing"
	case "paused", "pausing", "pause", "attention":
		return "paused"
	case "finished", "finish", "complete", "completed":
		return "finished"
	case "error", "failed":
		return "error"
	case "stopped", "cancelled", "canceled":
		return "cancelled"
	case "offline", "unavailable", "unknown", "":
		return "disconnected"
	default:
		return s
	}
}

// relayGcodeState maps a status word to the Bambu gcode_state FoxTrack reads.
func relayGcodeState(status string) string {
	switch status {
	case "printing":
		return "RUNNING"
	case "paused":
		return "PAUSE"
	case "finished":
		return "FINISH"
	case "error":
		return "FAILED"
	case "idle":
		return "IDLE"
	}
	return strings.ToUpper(status) // "cancelled" -> CANCELLED, as unmapped Moonraker states
}

// relayFor builds the FoxTrack relay payload from polled telemetry.
func relayFor(t mqttpkg.TelemetryData, kind string, caps []string) webhook.RelayPayload {
	return webhook.RelayPayload{Print: webhook.RelayPrint{
		GcodeState:         relayGcodeState(t.Status),
		SubTaskName:        t.FileName,
		McPercent:          t.Progress,
		NozzleTemper:       t.NozzleTemp,
		NozzleTargetTemper: t.NozzleTarget,
		BedTemper:          t.BedTemp,
		BedTargetTemper:    t.BedTarget,
		McRemainingTime:    t.TimeRemaining,
		PrinterKind:        kind,
		PrinterModel:       t.PrinterModel,
		BridgeVersion:      version.AppVersion,
		BridgeCapabilities: caps,
	}}
}

func clampPercent(f float64) int {
	return int(math.Max(0, math.Min(100, f)))
}
