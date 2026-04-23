package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/lan"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/update"
	"foxtrack-bridge/version"
)

var (
	configStore *config.Config
	configMutex sync.RWMutex
	lanCtrl     = lan.NewController()
)

// logBuf stores recent log lines for display in the UI log panel.
var (
	logBuf   []string
	logBufMu sync.Mutex
	logSubs  []chan string
	logSubMu sync.Mutex
)

// logWriter captures log output, appends it to logBuf, and fans it out to SSE subscribers.
type logWriter struct{ underlying io.Writer }

func (lw *logWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if line != "" {
		logBufMu.Lock()
		logBuf = append(logBuf, line)
		if len(logBuf) > 500 {
			logBuf = logBuf[len(logBuf)-500:]
		}
		logBufMu.Unlock()

		logSubMu.Lock()
		for _, ch := range logSubs {
			select {
			case ch <- line:
			default:
			}
		}
		logSubMu.Unlock()
	}
	return lw.underlying.Write(p)
}

// sseEscape makes a string safe to embed in a single SSE data field.
func sseEscape(s string) string {
	return strings.NewReplacer("\n", " ", "\r", "").Replace(s)
}

func StartServer() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Printf("No config found (%v) — starting fresh", err)
		cfg = &config.Config{Printers: []config.Printer{}}
	}

	// Redirect log output through logWriter so the UI can stream it.
	log.SetOutput(&logWriter{underlying: os.Stderr})
	log.SetFlags(log.LstdFlags)

	configMutex.Lock()
	configStore = cfg
	configMutex.Unlock()

	syncPrinterConnections(cfg)

	// On startup, tell Supabase which serials are currently configured.
	// Any bridge_printers rows for this token that aren't in the list get deleted.
	go syncPrintersToSupabase(cfg)

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/logo.png", handleLogo)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/printers", handlePrinters)
	http.HandleFunc("/api/printers/", handlePrinterByName) // DELETE /api/printers/{name}
	http.HandleFunc("/api/sync", handleSync)               // POST — FoxTrack sends current printer list
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/version", handleVersion)
	http.HandleFunc("/api/update/check", handleUpdateCheck)
	http.HandleFunc("/api/update/install", handleUpdateInstall)
	http.HandleFunc("/api/update/restart", handleUpdateRestart)
	http.HandleFunc("/api/test", handleTest)
	http.HandleFunc("/api/control/", handleControl) // /api/control/{name}/{command}
	http.HandleFunc("/api/camera/", handleCamera)   // /api/camera/{name}
	http.HandleFunc("/api/logs", handleLogs)        // GET — SSE log stream

	fmt.Println("FoxTrack Bridge running at http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Printf("Server error: %v", err)
	}
}

// handleLogs streams log lines to the browser as SSE (Server-Sent Events).
func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Send existing buffered lines on connect.
	logBufMu.Lock()
	snapshot := make([]string, len(logBuf))
	copy(snapshot, logBuf)
	logBufMu.Unlock()
	for _, line := range snapshot {
		fmt.Fprintf(w, "data: %s\n\n", sseEscape(line))
	}
	flusher.Flush()

	// Subscribe to new lines.
	ch := make(chan string, 64)
	logSubMu.Lock()
	logSubs = append(logSubs, ch)
	logSubMu.Unlock()
	defer func() {
		logSubMu.Lock()
		for i, c := range logSubs {
			if c == ch {
				logSubs = append(logSubs[:i], logSubs[i+1:]...)
				break
			}
		}
		logSubMu.Unlock()
	}()

	for {
		select {
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", sseEscape(line))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func mqttPrinter(p config.Printer, cfg *config.Config) mqttpkg.Printer {
	return mqttpkg.Printer{
		Name:       p.Name,
		IP:         p.IP,
		Serial:     p.Serial,
		LANCode:    p.LANCode,
		WebhookURL: cfg.WebhookURL,
		APIKey:     cfg.APIKey,
	}
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

func handleLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Write(logoPNG)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	cors(w)
	merged := mqttpkg.GetPrintersState()
	for k, v := range lanCtrl.GetStates() {
		merged[k] = v
	}
	json.NewEncoder(w).Encode(merged)
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"version": version.AppVersion})
}

func handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	result, err := update.CheckLatest(ctx)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(result)
}

func handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	if err := update.StartInstall(ctx); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "update staged, restart when ready"})
}

func handleUpdateRestart(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := update.RestartToApply(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "restarting to apply update"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		time.Sleep(700 * time.Millisecond)
		os.Exit(0)
	}()
}

// handleControl handles printer control commands.
// URL: /api/control/{printerName}/{command}
// Commands: pause, resume, stop, light_on, light_off
func handleControl(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse /api/control/{name}/{command}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/control/"), "/")
	if len(parts) != 2 {
		http.Error(w, "usage: /api/control/{printer_name}/{command}", http.StatusBadRequest)
		return
	}
	printerName, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(w, "invalid printer name", http.StatusBadRequest)
		return
	}
	command := parts[1]

	var args map[string]interface{}
	if r.Body != nil {
		defer r.Body.Close()
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&args); err != nil && err != io.EOF {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON body"})
			return
		}
	}

	if printerIsBambu(printerName) {
		if err := mqttpkg.SendCommandWithArgs(printerName, command, args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	} else {
		if err := lanCtrl.SendCommand(printerName, command, args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "command": command, "printer": printerName})
}

// handleCamera proxies the BambuLab MJPEG camera stream.
// URL: /api/camera/{printerName}
// BambuLab streams MJPEG on port 6000 with basic auth (bblp:lancode).
func handleCamera(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}

	printerName := strings.TrimPrefix(r.URL.Path, "/api/camera/")
	printerName = strings.TrimSuffix(printerName, "/")
	if decodedName, err := url.PathUnescape(printerName); err == nil {
		printerName = decodedName
	}

	// Find the printer config
	configMutex.RLock()
	var found *config.Printer
	for i := range configStore.Printers {
		if configStore.Printers[i].Name == printerName {
			found = &configStore.Printers[i]
			break
		}
	}
	configMutex.RUnlock()

	if found == nil {
		http.Error(w, "printer not found", http.StatusNotFound)
		return
	}

	if !isBambuPrinterConfig(*found) {
		if err := lanCtrl.ProxyCamera(w, r, found.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}

	bambuCameraStream(w, found.IP, found.LANCode, printerName)
}

// bambuCameraStream proxies a BambuLab printer camera using the proprietary binary
// protocol on port 6000: TLS connect → JSON auth → continuous 16-byte-header JPEG frames.
func bambuCameraStream(w http.ResponseWriter, ip, lanCode, printerName string) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp",
		net.JoinHostPort(ip, "6000"),
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		log.Printf("[camera/%s] connect: %v", printerName, err)
		http.Error(w, "camera unavailable", http.StatusBadGateway)
		return
	}
	defer conn.Close()

	// Build auth JSON safely
	authBytes, _ := json.Marshal(map[string]interface{}{
		"doctype": "auth",
		"content": map[string]string{"user": "bblp", "passwd": lanCode},
	})

	// Send auth within 5 s
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(authBytes, '\n')); err != nil {
		log.Printf("[camera/%s] auth write: %v", printerName, err)
		http.Error(w, "camera unavailable", http.StatusBadGateway)
		return
	}

	// Discard auth response (newline-terminated JSON) — 2 s timeout
	reader := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader.ReadString('\n') // ignore error — some firmware omits the response
	conn.SetDeadline(time.Time{})

	// Start MJPEG response
	const boundary = "bambu"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	hdr := make([]byte, 16)
	for {
		// Each frame: 16-byte header (bytes 4-7 = little-endian payload size) + JPEG bytes
		if _, err := io.ReadFull(reader, hdr); err != nil {
			log.Printf("[camera/%s] header read: %v", printerName, err)
			return
		}
		frameSize := binary.LittleEndian.Uint32(hdr[4:8])
		if frameSize == 0 || frameSize > 5<<20 {
			log.Printf("[camera/%s] invalid frame size %d", printerName, frameSize)
			return
		}
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(reader, frame); err != nil {
			log.Printf("[camera/%s] frame read: %v", printerName, err)
			return
		}
		// Write multipart MJPEG boundary
		fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, frameSize)
		if _, err := w.Write(frame); err != nil {
			return // client disconnected
		}
		fmt.Fprint(w, "\r\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func handleTest(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		IP           string `json:"ip"`
		MoonrakerURL string `json:"moonraker_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	address := ""
	if strings.TrimSpace(req.MoonrakerURL) != "" {
		u, err := url.Parse(req.MoonrakerURL)
		if err != nil || u.Host == "" {
			http.Error(w, "invalid moonraker_url", http.StatusBadRequest)
			return
		}
		address = u.Host
		if !strings.Contains(address, ":") {
			address = net.JoinHostPort(address, "7125")
		}
	} else {
		address = net.JoinHostPort(req.IP, "8883")
	}

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	reachable := err == nil
	if conn != nil {
		conn.Close()
	}

	json.NewEncoder(w).Encode(map[string]bool{"reachable": reachable})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	cors(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		defer configMutex.RUnlock()
		json.NewEncoder(w).Encode(configStore)
	case "POST":
		var newCfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		configStore = &newCfg
		configMutex.Unlock()
		syncPrinterConnections(&newCfg)
		if err := config.SaveConfig(&newCfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePrinters(w http.ResponseWriter, r *http.Request) {
	cors(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		defer configMutex.RUnlock()
		json.NewEncoder(w).Encode(configStore.Printers)
	case "POST":
		var p config.Printer
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		configStore.Printers = append(configStore.Printers, p)
		cfg := configStore
		configMutex.Unlock()

		if err := config.SaveConfig(cfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		if isBambuPrinterConfig(p) {
			mqttpkg.ConnectPrinter(mqttPrinter(p, cfg))
		} else {
			lanCtrl.AddOrUpdatePrinter(p, cfg.WebhookURL, cfg.APIKey)
			log.Printf("[%s] connected via Moonraker", p.Name)
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePrinterByName handles DELETE /api/printers/{name}
func handlePrinterByName(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "DELETE" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/printers/")
	if name == "" {
		http.Error(w, "missing printer name", http.StatusBadRequest)
		return
	}
	configMutex.Lock()
	printers := configStore.Printers[:0]
	for _, p := range configStore.Printers {
		if p.Name != name {
			printers = append(printers, p)
		}
	}
	configStore.Printers = printers
	cfg := configStore
	configMutex.Unlock()
	_ = config.SaveConfig(cfg)
	lanCtrl.RemovePrinter(name)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleSync receives the current printer list from FoxTrack and removes
// any local printers that are no longer in the list.
func handleSync(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Serials []string `json:"serials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	keep := make(map[string]bool)
	for _, s := range req.Serials {
		keep[s] = true
	}
	configMutex.Lock()
	printers := configStore.Printers[:0]
	for _, p := range configStore.Printers {
		if keep[p.Serial] {
			printers = append(printers, p)
		}
	}
	configStore.Printers = printers
	cfg := configStore
	configMutex.Unlock()
	_ = config.SaveConfig(cfg)
	lanCtrl.SyncPrinters(cfg.Printers, cfg.WebhookURL, cfg.APIKey)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// syncPrintersToSupabase POSTs the current serial list to the relay so it can
// delete any stale bridge_printers rows for this API token.
func syncPrintersToSupabase(cfg *config.Config) {
	if cfg.WebhookURL == "" || cfg.APIKey == "" {
		return
	}
	serials := make([]string, 0, len(cfg.Printers))
	for _, p := range cfg.Printers {
		serials = append(serials, p.Serial)
	}

	// Derive the sync URL from the webhook URL
	// webhook: .../bambu-local-relay  →  sync: .../bambu-local-sync
	syncURL := strings.Replace(cfg.WebhookURL, "bambu-local-relay", "bambu-local-sync", 1)

	body, _ := json.Marshal(map[string]interface{}{"serials": serials})
	req, err := http.NewRequest("POST", syncURL, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("[sync] failed to build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[sync] failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("[sync] startup sync complete — %d printers registered", len(serials))
}

func syncPrinterConnections(cfg *config.Config) {
	for _, p := range cfg.Printers {
		if isBambuPrinterConfig(p) {
			mqttpkg.ConnectPrinter(mqttPrinter(p, cfg))
		}
	}
	lanCtrl.SyncPrinters(cfg.Printers, cfg.WebhookURL, cfg.APIKey)
}

func printerIsBambu(name string) bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, p := range configStore.Printers {
		if p.Name == name {
			return isBambuPrinterConfig(p)
		}
	}
	return false
}

func isBambuPrinterConfig(p config.Printer) bool {
	if strings.TrimSpace(p.Serial) == "" {
		return false
	}
	if strings.TrimSpace(p.LANCode) == "" {
		return false
	}
	if strings.TrimSpace(p.MoonrakerURL) != "" {
		return false
	}
	return true
}
