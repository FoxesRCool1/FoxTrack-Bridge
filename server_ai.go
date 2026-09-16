package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"foxtrack-bridge/ai"
	"foxtrack-bridge/capture"
	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

// The assistant. Everything here is read-only by design: the executor below can
// look at printers, telemetry, history and logs, and it can take one camera
// still, but there is no path from a model reply to a config write or a printer
// command. That is not enforced by a prompt — it is enforced by there being no
// such tool.

// serverPort is the port the dashboard is listening on, recorded so
// get_bridge_info can report it. Written once in StartServer before any handler
// can run, and only read afterwards.
var serverPort int

// aiSettings returns a copy of the stored assistant settings, or the zero value
// when none are saved.
func aiSettings() config.AI {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil || configStore.AI == nil {
		return config.AI{}
	}
	return *configStore.AI
}

// handleAISettings reads or writes the assistant's provider settings. They live
// in config.json alongside everything else, but they get their own endpoint so
// a Settings save on the dashboard never carries them, and so the provider key
// can follow the same "empty means keep" rule as every other stored secret.
func handleAISettings(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		view := redactAI(configStore.AI)
		configMutex.RUnlock()
		if view == nil {
			view = &redactedAI{}
		}
		json.NewEncoder(w).Encode(view)
	case "POST":
		var req struct {
			Enabled     bool    `json:"enabled"`
			Preset      string  `json:"preset"`
			BaseURL     string  `json:"base_url"`
			Model       string  `json:"model"`
			APIKey      *string `json:"api_key"`
			AllowCamera bool    `json:"allow_camera"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Preset = strings.TrimSpace(req.Preset)
		if req.Preset != "" && !ai.ValidPreset(req.Preset) {
			http.Error(w, "unknown AI provider "+req.Preset, http.StatusBadRequest)
			return
		}

		configMutex.Lock()
		if configStore.AI == nil {
			configStore.AI = &config.AI{}
		}
		stored := configStore.AI
		stored.Enabled = req.Enabled
		stored.Preset = req.Preset
		stored.BaseURL = strings.TrimSpace(req.BaseURL)
		stored.Model = strings.TrimSpace(req.Model)
		stored.AllowCamera = req.AllowCamera
		// A nil api_key means "keep what is stored"; an empty string means
		// "clear it". The dashboard only ever sees api_key_set, so it sends nil
		// unless the user actually typed a new key or cleared the box.
		if req.APIKey != nil {
			stored.APIKey = strings.TrimSpace(*req.APIKey)
		}
		view := redactAI(stored)
		cfg := configStore.Clone()
		configMutex.Unlock()

		if err := config.SaveConfig(cfg); err != nil {
			log.Printf("[assistant] Warning: failed to save config: %v", err)
		}
		json.NewEncoder(w).Encode(view)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAIModels asks a provider what models it offers, so the settings screen
// can show a picker rather than a free-text box. The settings in the body are
// the ones being edited, which may not be saved yet; an omitted key falls back
// to the stored one so the user does not have to retype it to refresh the list.
func handleAIModels(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Preset  string `json:"preset"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s := ai.Settings{
		Preset:  strings.TrimSpace(req.Preset),
		BaseURL: strings.TrimSpace(req.BaseURL),
		APIKey:  strings.TrimSpace(req.APIKey),
	}
	if s.APIKey == "" {
		s.APIKey = aiSettings().APIKey
	}

	models, err := ai.NewClient(s).ListModels(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// handleAIChat answers one message. The dashboard sends the whole visible
// conversation each time, so the server keeps no session state: a browser
// refresh starts a new conversation and nothing is written to disk.
func handleAIChat(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	settings := aiSettings()
	if !settings.Configured() {
		w.WriteHeader(http.StatusPreconditionFailed)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "The assistant is not set up yet. Open Settings, choose an AI provider, enter a model, and turn the assistant on.",
		})
		return
	}

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	history := make([]ai.Message, 0, len(req.Messages))
	latest := ""
	for _, m := range req.Messages {
		text := strings.TrimSpace(m.Content)
		if text == "" {
			continue
		}
		switch m.Role {
		case "user":
			history = append(history, ai.UserMessage(text))
			latest = text
		case "assistant":
			history = append(history, ai.Message{"role": "assistant", "content": text})
		}
	}
	if latest == "" {
		http.Error(w, "no message to answer", http.StatusBadRequest)
		return
	}

	client := ai.NewClient(ai.Settings{
		Preset:      settings.Preset,
		BaseURL:     settings.BaseURL,
		Model:       settings.Model,
		APIKey:      settings.APIKey,
		AllowCamera: settings.AllowCamera,
	})

	// A tool round trip plus generation can take a while on a loaded provider
	// or a local model on CPU, and MaxToolRounds of them stack up.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	reply, err := ai.RunChatLoop(ctx, ai.ChatOptions{
		Client:         client,
		History:        ai.TrimHistory(history),
		LatestUserText: latest,
		Execute:        executeAITool,
		AllowCamera:    settings.AllowCamera,
	})
	if err != nil {
		log.Printf("[assistant] %v", err)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"reply": reply})
}

// --- tools -----------------------------------------------------------------

// notFound is the answer for a printer name the model made up or mistyped. It
// is a result, not an error: the model can read it, call list_printers, and
// recover inside the same reply.
func notFound(name string) ai.ToolResult {
	return ai.ToolResult{Value: map[string]any{
		"error":   "unknown_printer",
		"message": fmt.Sprintf("There is no printer named %q in this Bridge. Call list_printers to see the exact names.", name),
	}}
}

// findPrinter looks up one printer by exact name.
func findPrinter(name string) (config.Printer, bool) {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, p := range configStore.Printers {
		if p.Name == name {
			return p, true
		}
	}
	return config.Printer{}, false
}

// printerKind describes how a printer is reached, in the words the help library
// uses, so the model's answer and the help text agree.
func printerKind(p config.Printer) string {
	if !isBambuPrinterConfig(p) {
		return "klipper"
	}
	if p.IsCloud() {
		return "bambu-cloud"
	}
	return "bambu-lan"
}

// liveStates merges the MQTT and Moonraker telemetry maps, the same way
// handleStatus does.
func liveStates() map[string]*mqttpkg.TelemetryData {
	merged := mqttpkg.GetPrintersState()
	for k, v := range lanCtrl.GetStates() {
		merged[k] = v
	}
	return merged
}

// executeAITool runs one tool call. Every branch returns a value the model can
// read; an error is reserved for a genuine failure.
func executeAITool(ctx context.Context, name, argumentsJSON string) (ai.ToolResult, error) {
	args := map[string]any{}
	if strings.TrimSpace(argumentsJSON) != "" {
		// A model that writes malformed arguments should be told so, not have
		// the whole reply fail.
		if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
			return ai.ToolResult{Value: map[string]string{
				"error":   "bad_arguments",
				"message": "Those arguments were not valid JSON. Call the tool again with arguments matching its schema.",
			}}, nil
		}
	}
	argStr := func(key string) string {
		s, _ := args[key].(string)
		return strings.TrimSpace(s)
	}
	argInt := func(key string, def, min, max int) int {
		f, ok := args[key].(float64)
		if !ok {
			return def
		}
		n := int(f)
		if n < min {
			return min
		}
		if n > max {
			return max
		}
		return n
	}

	switch name {
	case "search_help":
		return ai.ToolResult{Value: map[string]any{"results": ai.SearchHelp(argStr("query"))}}, nil

	case "get_help_topic":
		topic, ok := ai.FindTopic(argStr("topic_id"))
		if !ok {
			return ai.ToolResult{Value: map[string]string{
				"error":   "unknown_topic",
				"message": "There is no help topic with that id. Use search_help instead.",
			}}, nil
		}
		return ai.ToolResult{Value: map[string]any{
			"topic_id": topic.ID,
			"title":    topic.Title,
			"body":     topic.Body,
		}}, nil

	case "list_printers":
		return ai.ToolResult{Value: toolListPrinters()}, nil

	case "get_printer_status":
		return toolPrinterStatus(argStr("printer_name")), nil

	case "test_printer_connection":
		return toolTestConnection(ctx, argStr("printer_name")), nil

	case "get_bridge_info":
		return ai.ToolResult{Value: toolBridgeInfo()}, nil

	case "get_print_history":
		return toolPrintHistory(argStr("printer_name"), argInt("limit", 10, 1, ai.ToolRowLimit)), nil

	case "get_recent_logs":
		return ai.ToolResult{Value: toolRecentLogs(argStr("printer_name"), argInt("lines", 40, 1, 80))}, nil

	case "view_printer_camera":
		return toolCamera(ctx, argStr("printer_name"))

	default:
		return ai.ToolResult{Value: map[string]string{
			"error":   "unknown_tool",
			"message": fmt.Sprintf("There is no tool called %q. Use one of the tools you were given.", name),
		}}, nil
	}
}

func toolListPrinters() map[string]any {
	states := liveStates()

	configMutex.RLock()
	printers := make([]config.Printer, len(configStore.Printers))
	copy(printers, configStore.Printers)
	configMutex.RUnlock()

	rows := make([]map[string]any, 0, len(printers))
	for _, p := range printers {
		row := map[string]any{
			"name":             p.Name,
			"kind":             printerKind(p),
			"reporting":        states[p.Name] != nil,
			"camera_available": p.WebcamURL != "" || (isBambuPrinterConfig(p) && p.IP != "" && p.LANCode != ""),
			"camera_hidden":    p.CameraHidden,
		}
		if p.IP != "" {
			row["ip"] = p.IP
		}
		if p.MoonrakerURL != "" {
			row["moonraker_url"] = p.MoonrakerURL
		}
		if st := states[p.Name]; st != nil {
			row["status"] = st.Status
			if st.PrinterModel != "" {
				row["model"] = st.PrinterModel
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["name"]) < fmt.Sprint(rows[j]["name"])
	})

	return map[string]any{
		"printers": rows,
		"count":    len(rows),
		"note":     "\"reporting\" false means the Bridge currently has no telemetry for that printer. It does not by itself say why.",
	}
}

func toolPrinterStatus(name string) ai.ToolResult {
	p, ok := findPrinter(name)
	if !ok {
		return notFound(name)
	}
	st := liveStates()[name]
	if st == nil {
		return ai.ToolResult{Value: map[string]any{
			"printer_name": name,
			"kind":         printerKind(p),
			"reporting":    false,
			"message":      "The Bridge has no live telemetry for this printer. It may be off, unreachable, or the connection may have been refused. Use test_printer_connection and get_recent_logs to find out which.",
		}}
	}

	out := map[string]any{
		"printer_name":    name,
		"kind":            printerKind(p),
		"reporting":       true,
		"status":          st.Status,
		"progress_pct":    st.Progress,
		"nozzle_temp_c":   st.NozzleTemp,
		"nozzle_target_c": st.NozzleTarget,
		"bed_temp_c":      st.BedTemp,
		"bed_target_c":    st.BedTarget,
		"light_on":        st.LightOn,
	}
	if st.FileName != "" {
		out["file_name"] = st.FileName
	}
	if st.TimeRemaining > 0 {
		out["time_remaining_minutes"] = st.TimeRemaining
	}
	if st.PrinterModel != "" {
		out["model"] = st.PrinterModel
	}
	if st.CoolingFanPct > 0 {
		out["cooling_fan_pct"] = st.CoolingFanPct
	}
	if st.SpeedLevel > 0 {
		out["speed_level"] = st.SpeedLevel
		out["speed_level_meaning"] = "1 Silent, 2 Standard, 3 Sport, 4 Ludicrous"
	}
	if len(st.AMS) > 0 {
		out["ams"] = st.AMS
	}
	if st.Error != "" {
		out["printer_error"] = st.Error
		out["printer_error_note"] = "This code comes from the printer's own firmware, not from the Bridge. Report it exactly and tell the user to look it up in the manufacturer's documentation. Do not guess what it means."
	}
	return ai.ToolResult{Value: out}
}

// toolTestConnection probes reachability the same way handleTest does: a TCP
// dial to the Bambu MQTT port, or a call to Moonraker's /printer/info. It sends
// nothing to the printer beyond opening the connection.
func toolTestConnection(ctx context.Context, name string) ai.ToolResult {
	p, ok := findPrinter(name)
	if !ok {
		return notFound(name)
	}

	out := map[string]any{"printer_name": name, "kind": printerKind(p)}

	if !isBambuPrinterConfig(p) {
		url := strings.TrimSpace(p.MoonrakerURL)
		if url == "" {
			out["reachable"] = false
			out["detail"] = "No Moonraker URL is configured for this printer."
			return ai.ToolResult{Value: out}
		}
		out["tested"] = strings.TrimRight(url, "/") + "/printer/info"
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(url, "/")+"/printer/info", nil)
		if err != nil {
			out["reachable"] = false
			out["detail"] = err.Error()
			return ai.ToolResult{Value: out}
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			out["reachable"] = false
			out["detail"] = err.Error()
			return ai.ToolResult{Value: out}
		}
		resp.Body.Close()
		out["reachable"] = resp.StatusCode < 500
		out["http_status"] = resp.StatusCode
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			out["detail"] = "Moonraker answered but refused the request. It probably has authentication turned on and needs an API key."
		}
		return ai.ToolResult{Value: out}
	}

	ip := strings.TrimSpace(p.IP)
	if ip == "" {
		out["reachable"] = false
		out["detail"] = "No IP address is configured for this printer."
		if p.IsCloud() {
			out["detail"] = "No IP address is known for this cloud printer yet. The Bridge learns it from the Bambu account once the printer reports in."
		}
		return ai.ToolResult{Value: out}
	}
	address := net.JoinHostPort(ip, "8883")
	out["tested"] = address
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		out["reachable"] = false
		out["detail"] = err.Error()
		out["note"] = "The Bridge could not open a TCP connection to the printer's MQTT port. That means the printer is not reachable on the network from this machine: wrong or stale IP address, a different network or VLAN, or a firewall. It is not a credentials problem."
		return ai.ToolResult{Value: out}
	}
	conn.Close()
	out["reachable"] = true
	out["note"] = "The printer's MQTT port accepted a TCP connection, so the machine is reachable. If the Bridge still has no telemetry, the likely causes are a wrong serial number or LAN access code, or LAN Only Mode or Developer Mode being switched off. Check get_recent_logs."
	return ai.ToolResult{Value: out}
}

func toolBridgeInfo() map[string]any {
	configMutex.RLock()
	linked := configStore.APIKey != "" || configStore.FoxTrack2APIKey != ""
	autoUpdate := configStore.AutoUpdate
	printerCount := len(configStore.Printers)
	cloud := configStore.BambuCloud
	configMutex.RUnlock()

	out := map[string]any{
		"version":          version.AppVersion,
		"operating_system": runtime.GOOS,
		"architecture":     runtime.GOARCH,
		"listen_port":      serverPort,
		"config_directory": config.ConfigDir(),
		"auto_update":      autoUpdate,
		"printer_count":    printerCount,
		"foxtrack_linked":  linked,
	}
	if problem := webhook.RelayHealth(); problem != nil {
		out["foxtrack_relay_problem"] = problem.Message
		out["foxtrack_relay_problem_kind"] = problem.Kind
		if problem.Printer != "" {
			out["foxtrack_relay_problem_printer"] = problem.Printer
		}
		out["foxtrack_relay_note"] = "This does not fix itself on a retry. The key was revoked or mistyped, or the FoxTrack workspace's plan no longer covers the Bridge."
	}
	if cloud.Linked() {
		out["bambu_cloud_linked"] = true
		out["bambu_cloud_region"] = cloud.Region
		if cloud.ExpiresAt > 0 {
			out["bambu_cloud_token_expires"] = time.Unix(cloud.ExpiresAt, 0).UTC().Format(time.RFC3339)
			out["bambu_cloud_token_expired"] = cloud.Expired(time.Now().Unix())
		}
	} else {
		out["bambu_cloud_linked"] = false
	}
	return out
}

func toolPrintHistory(printerName string, limit int) ai.ToolResult {
	var records []history.Record
	var err error
	if printerName != "" {
		if _, ok := findPrinter(printerName); !ok {
			return notFound(printerName)
		}
		records, err = history.ForPrinter(printerName)
	} else {
		records, err = history.Load()
	}
	if err != nil {
		return ai.ToolResult{Value: map[string]string{
			"error":   "failed",
			"message": "The print history could not be read: " + err.Error(),
		}}
	}

	// Newest first.
	sort.Slice(records, func(i, j int) bool { return records[i].EndTime > records[j].EndTime })
	total := len(records)
	if len(records) > limit {
		records = records[:limit]
	}

	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		row := map[string]any{
			"printer_name":     rec.PrinterName,
			"file_name":        rec.FileName,
			"result":           rec.Result,
			"duration_minutes": rec.Duration / 60,
		}
		if rec.StartTime > 0 {
			row["started"] = time.Unix(rec.StartTime, 0).UTC().Format(time.RFC3339)
		}
		if rec.EndTime > 0 {
			row["ended"] = time.Unix(rec.EndTime, 0).UTC().Format(time.RFC3339)
		}
		if rec.NozzleTemp > 0 {
			row["nozzle_temp_c"] = rec.NozzleTemp
		}
		if rec.BedTemp > 0 {
			row["bed_temp_c"] = rec.BedTemp
		}
		rows = append(rows, row)
	}

	out := map[string]any{"prints": rows, "returned": len(rows), "total_recorded": total}
	if total > len(rows) {
		out["truncated"] = true
	}
	if total == 0 {
		out["note"] = "No prints have been recorded. History is only written while the Bridge is running, so a print that finished while it was stopped leaves no record."
	}
	return ai.ToolResult{Value: out}
}

// logSecrets collects every stored secret so none of them can reach the model
// through a log line. The Bridge logs connection errors verbatim, and an error
// string can carry a URL with an API key in it.
func logSecrets() []string {
	configMutex.RLock()
	defer configMutex.RUnlock()

	var secrets []string
	add := func(s string) {
		if len(strings.TrimSpace(s)) >= 6 {
			secrets = append(secrets, strings.TrimSpace(s))
		}
	}
	if configStore != nil {
		add(configStore.APIKey)
		add(configStore.FoxTrack2APIKey)
		for _, p := range configStore.Printers {
			add(p.LANCode)
			add(p.APIKey)
		}
		if bc := configStore.BambuCloud; bc != nil {
			add(bc.AccessToken)
			add(bc.Email)
		}
		if a := configStore.AI; a != nil {
			add(a.APIKey)
		}
	}
	return secrets
}

func toolRecentLogs(printerName string, lines int) map[string]any {
	logBufMu.Lock()
	snapshot := make([]string, len(logBuf))
	copy(snapshot, logBuf)
	logBufMu.Unlock()

	secrets := logSecrets()
	matched := make([]string, 0, len(snapshot))
	for _, line := range snapshot {
		if printerName != "" && !strings.Contains(line, printerName) {
			continue
		}
		for _, secret := range secrets {
			line = strings.ReplaceAll(line, secret, "[redacted]")
		}
		matched = append(matched, line)
	}
	if len(matched) > lines {
		matched = matched[len(matched)-lines:]
	}

	out := map[string]any{"lines": matched, "returned": len(matched)}
	if len(matched) == 0 {
		out["note"] = "There are no matching log lines. The Bridge keeps only the most recent 500, and it starts empty on every restart."
	}
	return out
}

// toolCamera fetches one still frame and hands it back as a data URL for the
// chat loop to attach. Reaching this function at all means the user switched
// the camera on, because the tool is not offered otherwise — but it is checked
// again here, since a setting can change between the schema being built and the
// call arriving.
func toolCamera(ctx context.Context, name string) (ai.ToolResult, error) {
	if !aiSettings().AllowCamera {
		return ai.ToolResult{Value: map[string]string{
			"error":   "not_permitted",
			"message": "Looking at printer cameras is switched off. A user can turn it on in Settings, under Assistant. Answer from the telemetry instead, and do not call this tool again.",
		}}, nil
	}

	p, ok := findPrinter(name)
	if !ok {
		return notFound(name), nil
	}

	var frame []byte
	var err error
	if isBambuPrinterConfig(p) {
		if p.IP == "" || p.LANCode == "" {
			return ai.ToolResult{Value: map[string]string{
				"error":   "no_camera",
				"message": "This Bambu printer has no IP address or LAN access code stored, so the Bridge cannot reach its camera.",
			}}, nil
		}
		frame, err = capture.BambuFrame(p.IP, p.LANCode, p.Name)
	} else {
		if strings.TrimSpace(p.WebcamURL) == "" {
			return ai.ToolResult{Value: map[string]string{
				"error":   "no_camera",
				"message": "No webcam URL is configured for this Klipper printer, so the Bridge cannot reach its camera.",
			}}, nil
		}
		frame, err = capture.KlipperFrame(p.WebcamURL)
	}
	if err != nil {
		return ai.ToolResult{Value: map[string]string{
			"error":   "camera_failed",
			"message": "The camera frame could not be fetched: " + err.Error(),
		}}, nil
	}

	// A frame that large is a fault, not a photograph, and it would cost the
	// user real money to send.
	const maxFrameBytes = 4 << 20
	if len(frame) == 0 || len(frame) > maxFrameBytes {
		return ai.ToolResult{Value: map[string]string{
			"error":   "camera_failed",
			"message": "The camera returned an unusable frame.",
		}}, nil
	}

	_ = ctx
	log.Printf("[assistant] sent a camera frame from %s to the AI provider (%d bytes)", name, len(frame))
	return ai.ToolResult{
		Value: map[string]any{
			"printer_name": name,
			"status":       "image_attached",
			"note":         "The frame follows as an image. Describe only what is actually visible in it.",
		},
		ImageDataURL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(frame),
	}, nil
}
