package lan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// OctoPrint shares the PrusaLink shape: a polled HTTP printer that takes the
// API key as X-Api-Key (moonrakerAuthHeaders) and plain .gcode uploads.

var (
	errOctoOffline     = errors.New("OctoPrint is running but is not connected to the printer. Connect it in OctoPrint.")
	errOctoUnreachable = errors.New("Bridge could not reach OctoPrint while sending the file. Check that it is on.")
	errOctoRefused     = errors.New("OctoPrint refused the file. Check its free space and that the printer is connected and idle.")
)

func octoPrintURL(p configpkg.Printer, path string) string {
	return strings.TrimRight(strings.TrimSpace(p.OctoPrintURL), "/") + path
}

func fetchOctoPrintTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	printer, err := sendJSONRequest(client, "GET", octoPrintURL(p, "/api/printer"), headers, nil)
	if err != nil {
		if err.Error() == "HTTP 409" {
			return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, errOctoOffline
		}
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}
	job, err := sendJSONRequest(client, "GET", octoPrintURL(p, "/api/job"), headers, nil)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}
	t := octoPrintTelemetry(printer, job)
	t.PrinterModel = octoPrintModel(p)
	if t.Status == "disconnected" {
		// OctoPrint answered but has lost the printer: a failed poll, so a
		// running print gets pollGrace instead of ending as cancelled.
		return t, webhook.RelayPayload{}, errOctoOffline
	}
	return t, relayFor(t, configpkg.KindOctoPrint, []string{"print_file", "alarms", "console", "files"}), nil
}

// octoPrintTelemetry maps /api/printer and /api/job into the Bridge's
// telemetry. Pure, so tests feed it recorded answers.
func octoPrintTelemetry(printer, job map[string]interface{}) mqttpkg.TelemetryData {
	state := nestedMap(printer, "state")
	flags := nestedMap(state, "flags")
	flag := func(k string) bool { b, _ := flags[k].(bool); return b }
	temps := nestedMap(printer, "temperature")
	tool := nestedMap(temps, "tool0")
	bed := nestedMap(temps, "bed")
	progress := nestedMap(job, "progress")
	completion, hasCompletion := progress["completion"].(float64)

	var status string
	switch {
	case flag("error") || flag("closedOrError"):
		status = "error"
	case flag("cancelling"):
		status = "cancelled"
	case flag("paused") || flag("pausing"):
		status = "paused"
	case flag("printing") || flag("finishing"):
		status = "printing"
	case flag("operational") && hasCompletion && completion >= 99.95:
		// OctoPrint has no "finished" state: it goes back to Operational with
		// the finished job still selected at 100 %. A cancelled job keeps its
		// partial completion, so this says finished only for a complete one.
		status = "finished"
	case flag("operational"):
		status = "idle"
	default:
		status = normalizeState(stringAny(state["text"]))
	}

	file := nestedMap(nestedMap(job, "job"), "file")
	fileName := stringAny(file["display"])
	if fileName == "" {
		fileName = stringAny(file["name"])
	}
	// The job stays selected after it ends, so finished, error and cancelled
	// keep its name and progress, as Klipper does. Only an idle printer has none.
	keepJob := mqttpkg.InPrint(status) || status == "finished" || status == "error" || status == "cancelled"
	if !keepJob {
		fileName = ""
	}
	pct := 0
	if hasCompletion && keepJob {
		pct = clampPercent(completion)
	}
	remaining := 0
	if mqttpkg.InPrint(status) {
		remaining = max(0, int(floatAny(progress["printTimeLeft"]))/60)
	}

	return mqttpkg.TelemetryData{
		Status:        status,
		FileName:      fileName,
		Progress:      pct,
		NozzleTemp:    floatAny(tool["actual"]),
		NozzleTarget:  floatAny(tool["target"]),
		BedTemp:       floatAny(bed["actual"]),
		BedTarget:     floatAny(bed["target"]),
		TimeRemaining: remaining,
		Alarms:        octoPrintAlarms(state),
	}
}

// octoPrintAlarms turns state.flags.error into one serious alarm with the
// server's own text.
func octoPrintAlarms(state map[string]interface{}) []webhook.RelayAlarm {
	flags := nestedMap(state, "flags")
	if b, _ := flags["error"].(bool); !b {
		return nil
	}
	msg := strings.TrimSpace(stringAny(state["error"]))
	if msg == "" {
		msg = strings.TrimSpace(stringAny(state["text"]))
	}
	if msg == "" {
		msg = "OctoPrint reports a printer error."
	}
	return []webhook.RelayAlarm{webhook.MessageAlarm(webhook.AlarmSerious, msg)}
}

// sendOctoPrintCommand runs pause, resume or stop on the current job, and the
// console commands (Bridge 2.8) on the printer: nozzle and bed targets through
// /api/printer/tool and /api/printer/bed, home through /api/printer/printhead.
func sendOctoPrintCommand(p configpkg.Printer, command string, args map[string]interface{}) error {
	path, body, err := octoPrintCommandRequest(command, args)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	headers["Content-Type"] = "application/json"
	_, err = sendJSONRequest(client, "POST", octoPrintURL(p, path), headers, strings.NewReader(body))
	if err != nil && err.Error() == "HTTP 409" {
		if mqttpkg.IsConsoleCommand(command) {
			return errors.New("OctoPrint is not connected to the printer.")
		}
		return errors.New("OctoPrint has no print to do that to right now.")
	}
	return err
}

// octoPrintCommandRequest is the OctoPrint API path and JSON body for a
// command. Pure, so tests check the mapping without a server.
func octoPrintCommandRequest(command string, args map[string]interface{}) (path, body string, err error) {
	switch command {
	case "pause":
		return "/api/job", `{"command":"pause","action":"pause"}`, nil
	case "resume":
		return "/api/job", `{"command":"pause","action":"resume"}`, nil
	case "stop":
		return "/api/job", `{"command":"cancel"}`, nil
	case "set_nozzle_temp":
		c, err := mqttpkg.CelsiusArg(args, mqttpkg.NozzleMaxC)
		if err != nil {
			return "", "", err
		}
		return "/api/printer/tool", fmt.Sprintf(`{"command":"target","targets":{"tool0":%d}}`, c), nil
	case "set_bed_temp":
		c, err := mqttpkg.CelsiusArg(args, mqttpkg.BedMaxC)
		if err != nil {
			return "", "", err
		}
		return "/api/printer/bed", fmt.Sprintf(`{"command":"target","target":%d}`, c), nil
	case "home":
		return "/api/printer/printhead", `{"command":"home","axes":["x","y","z"]}`, nil
	}
	return "", "", fmt.Errorf("unsupported command for OctoPrint: %s", command)
}

// octoPrintUpload sends the file to OctoPrint's local storage and starts it
// (POST /api/files/local, select=true, print=true). OctoPrint answers 201.
func octoPrintUpload(ctx context.Context, p configpkg.Printer, localPath, remoteName string, logf func(string, ...any)) error {
	if err := keyPreflight(ctx, p, octoPrintURL(p, "/api/version")); err != nil {
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
	remote := SanitizeRemoteName(remoteName)

	// Multipart by hand around the open file (see UploadAndPrint): the file is
	// never held in memory and the request has a Content-Length.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, kv := range [][2]string{{"select", "true"}, {"print", "true"}} {
		_ = mw.WriteField(kv[0], kv[1])
	}
	_, _ = mw.CreateFormFile("file", remote)
	tail := []byte("\r\n--" + mw.Boundary() + "--\r\n")

	req, err := http.NewRequestWithContext(ctx, "POST", octoPrintURL(p, "/api/files/local"), io.MultiReader(&head, f, bytes.NewReader(tail)))
	if err != nil {
		logf("upload request: %v", err)
		return errNoAddress
	}
	req.ContentLength = int64(head.Len()) + st.Size() + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	applyMoonrakerAuth(req, p)

	resp, err := (&http.Client{Timeout: uploadTimeout, Transport: insecureTransport()}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("upload failed: %v", err)
		return errOctoUnreachable
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
		return errOctoRefused
	}
	return nil
}

// --- OctoPrint settings: webcam addresses and the printer profile name ---

type octoInfo struct {
	snapshotURL string
	streamURL   string
	model       string
	fetchedAt   time.Time
}

var (
	octoInfoCache = map[string]octoInfo{} // by OctoPrint URL
	octoInfoMu    sync.Mutex
)

// octoPrintInfo reads /api/settings and /api/printerprofiles at most once
// every 5 minutes per server. Errors leave the cached answer, or an empty one.
func octoPrintInfo(p configpkg.Printer) octoInfo {
	key := strings.TrimRight(strings.TrimSpace(p.OctoPrintURL), "/")
	octoInfoMu.Lock()
	cached, ok := octoInfoCache[key]
	octoInfoMu.Unlock()
	if ok && time.Since(cached.fetchedAt) < 5*time.Minute {
		return cached
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	// Start from the cached answer: a failed read keeps its fields, so one
	// slow reply does not drop the webcam and model for 5 minutes.
	info := cached
	info.fetchedAt = time.Now()
	if m, err := sendJSONRequest(client, "GET", key+"/api/settings", headers, nil); err == nil {
		webcam := nestedMap(m, "webcam")
		info.snapshotURL = resolveOctoCamURL(key, stringAny(webcam["snapshotUrl"]))
		info.streamURL = resolveOctoCamURL(key, stringAny(webcam["streamUrl"]))
	}
	if m, err := sendJSONRequest(client, "GET", key+"/api/printerprofiles", headers, nil); err == nil {
		info.model = ""
		for _, v := range nestedMap(m, "profiles") {
			prof, _ := v.(map[string]interface{})
			if def, _ := prof["default"].(bool); def {
				info.model = stringAny(prof["name"])
			}
		}
	}
	octoInfoMu.Lock()
	octoInfoCache[key] = info
	octoInfoMu.Unlock()
	return info
}

// resolveOctoCamURL turns OctoPrint's webcam address into one the Bridge can
// open: a relative path joins the server's address, and a loopback host (the
// default snapshot URL is http://127.0.0.1:8080/?action=snapshot, meant for
// OctoPrint itself) becomes the server's own host.
func resolveOctoCamURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u, err := b.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if h := strings.ToLower(u.Hostname()); h == "127.0.0.1" || h == "localhost" || h == "::1" {
		host := b.Hostname()
		if port := u.Port(); port != "" {
			host += ":" + port
		}
		u.Host = host
	}
	return u.String()
}

func octoPrintSnapshotURL(p configpkg.Printer) string { return octoPrintInfo(p).snapshotURL }
func octoPrintStreamURL(p configpkg.Printer) string   { return octoPrintInfo(p).streamURL }

func octoPrintModel(p configpkg.Printer) string {
	if m := octoPrintInfo(p).model; m != "" {
		return m
	}
	return "OctoPrint"
}
