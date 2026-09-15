package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	"foxtrack-bridge/lan"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/update"
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
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

func StartServer(port int) {
	cfg, err := config.LoadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("No config found — starting fresh")
		} else {
			// The file exists but failed to load. Never start with an empty
			// config in that case — a later save would overwrite the user's
			// printers. Move the bad file aside first.
			backup, backupErr := config.BackupCorrupt()
			if backupErr != nil {
				log.Fatalf("ERROR: config failed to load (%v) and could not be backed up (%v) — refusing to start with an empty config; fix or move the file manually", err, backupErr)
			}
			log.Printf("ERROR: config failed to load (%v) — the original file was backed up to %s; starting with an empty config", err, backup)
		}
		cfg = &config.Config{Printers: []config.Printer{}}
	}

	// Redirect log output through logWriter so the UI can stream it.
	log.SetOutput(&logWriter{underlying: os.Stderr})
	log.SetFlags(log.LstdFlags)

	configMutex.Lock()
	configStore = cfg
	configMutex.Unlock()

	mqttpkg.OnCloudAuthFailed = expireCloudToken
	syncPrinterConnections(nil, cfg)
	go autoUpdateLoop()
	go pollBridgeCommands()

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/logo.png", handleLogo)
	http.HandleFunc("/logo-light.png", pngHandler(logoLightPNG))
	http.HandleFunc("/logo-dark.png", pngHandler(logoDarkPNG))
	http.HandleFunc("/tailwind.css", cssHandler(tailwindCSS))
	http.HandleFunc("/icons.css", cssHandler(iconsCSS))
	http.HandleFunc("/fonts.css", cssHandler(fontsCSS))
	http.HandleFunc("/fonts/", fontHandler)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/printers", handlePrinters)
	http.HandleFunc("/api/printers/", handlePrinterByName) // DELETE /api/printers/{name}
	http.HandleFunc("/api/status", handleStatus)
	http.HandleFunc("/api/relay-health", handleRelayHealth)
	http.HandleFunc("/api/version", handleVersion)
	http.HandleFunc("/api/update/check", handleUpdateCheck)
	http.HandleFunc("/api/update/install", handleUpdateInstall)
	http.HandleFunc("/api/update/restart", handleUpdateRestart)
	http.HandleFunc("/api/test", handleTest)
	http.HandleFunc("/api/control/", handleControl)          // /api/control/{name}/{command}
	http.HandleFunc("/api/camera/", handleCamera)            // /api/camera/{name}
	http.HandleFunc("/api/logs", handleLogs)                 // GET — SSE log stream
	http.HandleFunc("/api/history", handleHistory)           // GET all history records
	http.HandleFunc("/api/history/", handleHistoryByPrinter) // GET /api/history/{name}
	http.HandleFunc("/api/cloud/", handleCloud)              // Bambu Cloud account: status, login, verify, token, devices, unlink, retry

	printStartupBanner(port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		log.Printf("Server error: %v", err)
	}
}

var privateRanges = []net.IPNet{
	{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)},
	{IP: net.ParseIP("172.16.0.0"), Mask: net.CIDRMask(12, 32)},
	{IP: net.ParseIP("192.168.0.0"), Mask: net.CIDRMask(16, 32)},
}

func isPrivateIP(ip net.IP) bool {
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

func printStartupBanner(port int) {
	log.Println("FoxTrack Bridge is running. Open in your browser:")
	log.Printf("  http://localhost:%d", port)
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() || ip.To4() == nil || !isPrivateIP(ip) {
					continue
				}
				log.Printf("  http://%s:%d", ip.String(), port)
			}
		}
	}
}

// handleLogs streams log lines to the browser as SSE (Server-Sent Events).
func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
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
		Name:            p.Name,
		IP:              p.IP,
		Serial:          p.Serial,
		LANCode:         p.LANCode,
		APIKey:          cfg.APIKey,
		FoxTrack2APIKey: cfg.FoxTrack2APIKey,
	}
}

// jsonHeaders sets the response content type for JSON API endpoints. The
// dashboard is served same-origin, so no CORS headers are sent.
func jsonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

// foxtrackWebOrigins are the browser origins allowed to call this bridge's
// control and camera endpoints cross-origin.
//
// The FoxTrack web app tries the local bridge first for pause/resume/stop/light
// and falls back to the cloud command queue when that call fails. With no CORS
// headers the browser rejected every response, so the fast path always looked
// like it had failed — *after* the bridge had already executed the command —
// and the queued copy then ran it a second time a few seconds later.
//
// Deliberately an allow-list rather than "*": /api/control has no auth of its
// own, so a wildcard would let any page the user happens to visit read their
// printer state and drive their printers.
var foxtrackWebOrigins = map[string]bool{
	"https://foxtrack.studio":           true,
	"https://www.foxtrack.studio":       true,
	"https://foxtrack-beta.lovable.app": true,
}

// isFoxTrackWebOrigin also accepts a loopback dev server, so the web app can be
// developed against a real bridge. That grants nothing new: any process already
// on this machine can reach the bridge's unauthenticated API directly.
func isFoxTrackWebOrigin(origin string) bool {
	if foxtrackWebOrigins[origin] {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

// allowWebOrigin echoes an allowed Origin back as CORS headers. Chrome's
// Private Network Access check additionally requires the private-network header
// before a public HTTPS page may reach a loopback or LAN address, so preflights
// for these endpoints fail without it.
func allowWebOrigin(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" || !isFoxTrackWebOrigin(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

func handleLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Write(logoPNG)
}

// pngHandler returns an http.HandlerFunc that serves the given bytes as a PNG.
// Used for the embedded theme-specific logos.
func pngHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(body)
	}
}

// cssHandler returns an http.HandlerFunc that serves the given bytes as CSS.
// Used for the embedded, locally-served stylesheets so the dashboard has zero
// CDN dependencies.
func cssHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(body)
	}
}

// fontHandler serves the embedded Inter woff2 files from web/fonts.
func fontHandler(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path) // path.Base strips any traversal segments
	data, err := fontsFS.ReadFile("web/fonts/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(data)
}

// handleStatus reports live status keyed by printer ID. mqttpkg.GetPrintersState
// and lanCtrl.GetStates are both internally name-keyed (out of scope to change),
// so the merged result is translated through configStore.Printers before encoding.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	merged := mqttpkg.GetPrintersState()
	for k, v := range lanCtrl.GetStates() {
		merged[k] = v
	}

	configMutex.RLock()
	nameToID := make(map[string]string, len(configStore.Printers))
	for _, p := range configStore.Printers {
		if p.ID != "" {
			nameToID[p.Name] = p.ID
		}
	}
	configMutex.RUnlock()

	byID := make(map[string]*mqttpkg.TelemetryData, len(merged))
	var dropped []string
	for name, v := range merged {
		id, ok := nameToID[name]
		if !ok {
			dropped = append(dropped, name)
			continue
		}
		byID[id] = v
	}
	if len(dropped) > 0 {
		log.Printf("[status] dropped live status for %d printer(s) with no matching config entry: %v", len(dropped), dropped)
	}
	json.NewEncoder(w).Encode(byID)
}

// handleRelayHealth reports a FoxTrack rejection the user has to act on: a
// revoked or mistyped bridge token, or a workspace whose plan no longer covers
// the Bridge. Those answers never change on a retry, so they used to be tried
// three times and dropped — the dashboard looked healthy while nothing at all
// reached FoxTrack. Returns {"problem": null} when the relay is fine.
func handleRelayHealth(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"problem": webhook.RelayHealth()})
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
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
	jsonHeaders(w)
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
	jsonHeaders(w)
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
	jsonHeaders(w)
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
	jsonHeaders(w)
	allowWebOrigin(w, r)
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
	allowWebOrigin(w, r)
	if r.Method == "OPTIONS" {
		return
	}

	printerName := strings.TrimPrefix(r.URL.Path, "/api/camera/")
	printerName = strings.TrimSuffix(printerName, "/")
	if decodedName, err := url.PathUnescape(printerName); err == nil {
		printerName = decodedName
	}

	// Find the printer config. Copy it by value while holding the lock — the
	// delete handler compacts configStore.Printers in place, so a pointer into
	// the slice would race once the lock is released.
	configMutex.RLock()
	var found config.Printer
	ok := false
	for i := range configStore.Printers {
		if configStore.Printers[i].Name == printerName {
			found = configStore.Printers[i]
			ok = true
			break
		}
	}
	configMutex.RUnlock()

	if !ok {
		http.Error(w, "printer not found", http.StatusNotFound)
		return
	}

	if !isBambuPrinterConfig(found) {
		if err := lanCtrl.ProxyCamera(w, r, found.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}

	ip := found.IP
	if found.IsCloud() && ip == "" {
		ip = mqttpkg.CloudIP(found.Serial)
	}
	if found.IsCloud() && ip == "" {
		http.Error(w, "camera needs the printer's LAN address: it is learned from the printer's first cloud report, or set it when adding the printer", http.StatusServiceUnavailable)
		return
	}
	if found.IsCloud() && found.LANCode == "" {
		http.Error(w, "camera needs the printer's LAN access code, which the Bambu account did not report", http.StatusServiceUnavailable)
		return
	}
	bambuCameraStream(w, ip, found.LANCode, printerName)
}

// bambuCameraStream proxies a BambuLab printer camera using the proprietary binary
// protocol on port 6000.
//
// Auth: 80-byte binary struct (NOT JSON):
//
//	[0:4]  = 0x40 (LE u32) — magic
//	[4:8]  = 0x3000 (LE u32) — command
//	[8:16] = 8 zero bytes — padding
//	[16:48] = "bblp" NUL-padded to 32 bytes — username
//	[48:80] = access_code NUL-padded to 32 bytes — password
//
// Each frame: 16-byte header where bytes [0:4] are the LE u32 JPEG payload size,
// followed by that many bytes of JPEG data.
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

	// 80-byte binary auth payload — not JSON.
	auth := make([]byte, 80)
	binary.LittleEndian.PutUint32(auth[0:4], 0x40)   // magic
	binary.LittleEndian.PutUint32(auth[4:8], 0x3000) // command
	// bytes [8:16] remain zero — padding
	copy(auth[16:48], []byte("bblp"))  // username field, NUL-padded to 32 bytes
	copy(auth[48:80], []byte(lanCode)) // password field, NUL-padded to 32 bytes

	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(auth); err != nil {
		log.Printf("[camera/%s] auth write: %v", printerName, err)
		http.Error(w, "camera unavailable", http.StatusBadGateway)
		return
	}
	conn.SetDeadline(time.Time{})

	// Start MJPEG response
	const boundary = "bambu"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	reader := bufio.NewReader(conn)
	hdr := make([]byte, 16)
	for {
		// 16-byte frame header: bytes [0:4] = LE u32 JPEG payload size.
		if _, err := io.ReadFull(reader, hdr); err != nil {
			log.Printf("[camera/%s] header read: %v", printerName, err)
			return
		}
		frameSize := binary.LittleEndian.Uint32(hdr[0:4])
		if frameSize == 0 || frameSize > 10<<20 {
			log.Printf("[camera/%s] invalid frame size %d", printerName, frameSize)
			return
		}
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(reader, frame); err != nil {
			log.Printf("[camera/%s] frame read: %v", printerName, err)
			return
		}
		// Skip non-JPEG payloads (some frames are metadata, not images).
		if frameSize < 2 || frame[0] != 0xFF || frame[1] != 0xD8 {
			continue
		}
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
	jsonHeaders(w)
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

	reachable := false
	detail := ""

	if strings.TrimSpace(req.MoonrakerURL) != "" {
		// Klipper: hit Moonraker's /printer/info endpoint.
		infoURL := strings.TrimRight(req.MoonrakerURL, "/") + "/printer/info"
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(infoURL)
		if err != nil {
			detail = err.Error()
		} else {
			resp.Body.Close()
			reachable = resp.StatusCode < 500
		}
	} else {
		// Bambu: TCP reachability on MQTT port.
		address := net.JoinHostPort(strings.TrimSpace(req.IP), "8883")
		conn, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err != nil {
			detail = err.Error()
		} else {
			conn.Close()
			reachable = true
		}
	}

	resp := map[string]interface{}{"reachable": reachable}
	if detail != "" {
		resp["detail"] = detail
	}
	json.NewEncoder(w).Encode(resp)
}

// redactedPrinter mirrors config.Printer for API responses, with the secret
// fields (LAN access code, Moonraker API key) blanked and replaced by
// "is set" flags so the UI can show that a value exists without seeing it.
type redactedPrinter struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IP           string `json:"ip,omitempty"`
	Serial       string `json:"serial,omitempty"`
	LANCode      string `json:"lan_code"`
	LANCodeSet   bool   `json:"lan_code_set"`
	MoonrakerURL string `json:"moonraker_url,omitempty"`
	APIKey       string `json:"api_key"`
	APIKeySet    bool   `json:"api_key_set"`
	WebcamURL    string `json:"webcam_url,omitempty"`
	CameraHidden bool   `json:"camera_hidden"`
	Connection   string `json:"connection,omitempty"`
}

// redactedConfig mirrors config.Config with the FoxTrack cloud tokens blanked
// the same way. Secrets never leave the server.
type redactedConfig struct {
	APIKey             string            `json:"api_key"`
	APIKeySet          bool              `json:"api_key_set"`
	FoxTrack2APIKey    string            `json:"foxtrack2_api_key"`
	FoxTrack2APIKeySet bool              `json:"foxtrack2_api_key_set"`
	Printers           []redactedPrinter `json:"printers"`
	AutoUpdate         bool              `json:"auto_update,omitempty"`
	BambuCloud         *redactedCloud    `json:"bambu_cloud,omitempty"`
}

// redactedCloud mirrors config.BambuCloud without the access token.
type redactedCloud struct {
	Linked         bool   `json:"linked"`
	Email          string `json:"email,omitempty"`
	Region         string `json:"region,omitempty"`
	TokenExpiresAt int64  `json:"token_expires_at,omitempty"`
}

func redactPrinter(p config.Printer) redactedPrinter {
	return redactedPrinter{
		ID:           p.ID,
		Name:         p.Name,
		IP:           p.IP,
		Serial:       p.Serial,
		LANCodeSet:   p.LANCode != "",
		MoonrakerURL: p.MoonrakerURL,
		APIKeySet:    p.APIKey != "",
		WebcamURL:    p.WebcamURL,
		CameraHidden: p.CameraHidden,
		Connection:   p.Connection,
	}
}

// assignMissingIDs fills in a fresh, server-generated ID for any printer that
// doesn't already have one. Printers that already carry an ID (backfilled at
// boot, or echoed back by a client that already knows it) are left untouched —
// an ID is generated once on creation and never changed.
func assignMissingIDs(printers []config.Printer) {
	for i := range printers {
		if printers[i].ID == "" {
			printers[i].ID = config.NewPrinterID()
		}
	}
}

// normalizeName folds a printer name for uniqueness comparisons only. Every
// identity lookup by name elsewhere in this file (delete, camera, control,
// applyStoredSecrets' name-fallback) stays exact-match and untouched —
// case-folding applies solely to the uniqueness guards below.
func normalizeName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// errDuplicatePrinterName marks a save rejected because it would introduce
// two printers sharing the same name (case-insensitively). Matched via
// errors.Is in handleConfig to pick the right HTTP status.
var errDuplicatePrinterName = errors.New("printer names must be unique")

// hasDuplicateName reports whether incoming's name collides
// (case-insensitively) with any printer already in existing.
func hasDuplicateName(existing []config.Printer, incoming config.Printer) bool {
	target := normalizeName(incoming.Name)
	for _, p := range existing {
		if normalizeName(p.Name) == target {
			return true
		}
	}
	return false
}

// rejectNewlyIntroducedDuplicateNames checks a full-replace printer list
// against the previous one and rejects only a name that the payload itself
// makes duplicate. A name that was already duplicated in oldPrinters is
// grandfathered through unchanged (logged once) — no uniqueness check
// existed before this session, so some installs may already have duplicates
// on disk, and a strict reject here would make those installs permanently
// unable to save any change through this path, including the rename that
// would fix the duplicate.
func rejectNewlyIntroducedDuplicateNames(oldPrinters, newPrinters []config.Printer) error {
	oldCounts := make(map[string]int, len(oldPrinters))
	for _, p := range oldPrinters {
		oldCounts[normalizeName(p.Name)]++
	}
	newCounts := make(map[string]int, len(newPrinters))
	for _, p := range newPrinters {
		newCounts[normalizeName(p.Name)]++
	}
	for norm, count := range newCounts {
		if count < 2 {
			continue
		}
		if oldCounts[norm] >= 2 {
			log.Printf("[config] printer name %q is already duplicated in the stored config — kept as-is", norm)
			continue
		}
		// Find one of the actual (non-normalized) colliding names for a
		// readable message.
		display := norm
		for _, p := range newPrinters {
			if normalizeName(p.Name) == norm {
				display = p.Name
				break
			}
		}
		return fmt.Errorf("more than one printer named %q: %w", display, errDuplicatePrinterName)
	}
	return nil
}

// errBlankPrinterName marks a save rejected because it would add a printer with
// no name. A nameless printer is not just cosmetic: control, camera, history and
// the MQTT/Moonraker drivers are all keyed by name, so a blank one both fails
// every lookup and blocks the next printer from using a blank-ish name.
// Matched via errors.Is in handleConfig to pick the right HTTP status.
var errBlankPrinterName = errors.New("every printer must have a name")

// rejectNewlyBlankNames checks a full-replace printer list against the previous
// one and rejects only a nameless entry that the payload itself introduces. A
// blank name already on disk is grandfathered through for the same reason a
// pre-existing duplicate is (see rejectNewlyIntroducedDuplicateNames): no
// name check existed before, and a strict reject would leave such an install
// permanently unable to save any change, including the rename that would fix it.
func rejectNewlyBlankNames(oldPrinters, newPrinters []config.Printer) error {
	countBlank := func(printers []config.Printer) int {
		n := 0
		for _, p := range printers {
			if strings.TrimSpace(p.Name) == "" {
				n++
			}
		}
		return n
	}
	if countBlank(newPrinters) > countBlank(oldPrinters) {
		return errBlankPrinterName
	}
	return nil
}

func redactConfig(cfg *config.Config) redactedConfig {
	out := redactedConfig{
		APIKeySet:          cfg.APIKey != "",
		FoxTrack2APIKeySet: cfg.FoxTrack2APIKey != "",
		Printers:           make([]redactedPrinter, len(cfg.Printers)),
		AutoUpdate:         cfg.AutoUpdate,
	}
	for i, p := range cfg.Printers {
		out.Printers[i] = redactPrinter(p)
	}
	if bc := cfg.BambuCloud; bc.Linked() {
		out.BambuCloud = &redactedCloud{Linked: true, Email: bc.Email, Region: bc.Region, TokenExpiresAt: bc.ExpiresAt}
	}
	return out
}

// applyStoredSecrets fills empty secret fields on an incoming config from the
// currently stored one. The dashboard posts back the redacted config it
// received, so an empty secret means "keep the existing value" — only a
// non-empty value replaces a stored secret. Printers are matched by ID
// first — the stable identity that survives a rename — falling back to name
// only for the degenerate case of an incoming printer with no ID at all (a
// legacy or hand-crafted payload that predates this field). The caller must
// hold configMutex.
func applyStoredSecrets(newCfg, old *config.Config) {
	if old == nil {
		return
	}
	if newCfg.APIKey == "" {
		newCfg.APIKey = old.APIKey
	}
	if newCfg.FoxTrack2APIKey == "" {
		newCfg.FoxTrack2APIKey = old.FoxTrack2APIKey
	}
	// The dashboard never sends the Bambu account (it only ever sees a
	// redacted view), so an absent or token-less block means "keep it".
	if newCfg.BambuCloud == nil || newCfg.BambuCloud.AccessToken == "" {
		newCfg.BambuCloud = old.BambuCloud
	}
	oldByID := make(map[string]config.Printer, len(old.Printers))
	oldByName := make(map[string]config.Printer, len(old.Printers))
	for _, p := range old.Printers {
		if p.ID != "" {
			oldByID[p.ID] = p
		}
		oldByName[p.Name] = p
	}
	for i := range newCfg.Printers {
		p := &newCfg.Printers[i]
		prev, ok := config.Printer{}, false
		if p.ID != "" {
			prev, ok = oldByID[p.ID]
		}
		if !ok {
			prev, ok = oldByName[p.Name]
		}
		if !ok {
			continue
		}
		if p.LANCode == "" {
			p.LANCode = prev.LANCode
		}
		if p.APIKey == "" {
			p.APIKey = prev.APIKey
		}
		// PreviousNames has no redacted-view counterpart, so a client echoing
		// back the config it was given always omits it. Treat "absent" as "keep"
		// rather than letting a round-trip drop the rename history.
		if len(p.PreviousNames) == 0 && len(prev.PreviousNames) > 0 {
			p.PreviousNames = append([]string(nil), prev.PreviousNames...)
		}
	}
}

// errRefuseClearPrinters is returned when a POST /api/config body would replace a
// non-empty printer list with an empty one and does not carry
// "confirm_clear_printers": true. Its message is sent as the 409 response body, so
// it is written to be understandable by an end user surfacing it in any client.
// See CLAUDE.md (config-compatibility invariants).
var errRefuseClearPrinters = errors.New("This save would remove all of your saved printers, so it was blocked to prevent accidental data loss. Your existing printers were kept. To intentionally remove every printer, delete them individually, or resend this request with \"confirm_clear_printers\": true.")

// resolveConfigUpdate applies a POST /api/config request body to the previous
// config, enforcing the printer-preservation invariants:
//   - if the body omits the "printers" key entirely, existing printers are kept
//     untouched (partial update — used by Settings saves);
//   - if the body carries "printers": [] while printers currently exist, the
//     update is refused unless it also carries "confirm_clear_printers": true;
//   - otherwise the printer list is replaced as given.
//
// Blank top-level/per-printer secrets are re-filled from old. old is not mutated.
func resolveConfigUpdate(old *config.Config, body []byte) (*config.Config, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var incoming config.Config
	if err := json.Unmarshal(body, &incoming); err != nil {
		return nil, err
	}
	var meta struct {
		ConfirmClearPrinters bool `json:"confirm_clear_printers"`
	}
	_ = json.Unmarshal(body, &meta) // best-effort; defaults to false

	_, printersPresent := raw["printers"]
	newCfg := incoming
	// Every top-level setting is partial: a body that omits a key leaves the
	// stored value alone. Blank secrets are handled by applyStoredSecrets below;
	// auto_update is a bool, so "absent" is the only way to say "leave it" — and
	// without this, a Settings save made from a dashboard whose /api/config load
	// failed would silently turn auto-update off.
	if _, ok := raw["auto_update"]; !ok && old != nil {
		newCfg.AutoUpdate = old.AutoUpdate
	}
	switch {
	case !printersPresent:
		// Partial update: never touch printers. Copy so old is never aliased.
		if old != nil {
			newCfg.Printers = append([]config.Printer(nil), old.Printers...)
		}
	case len(newCfg.Printers) == 0 && old != nil && len(old.Printers) > 0 && !meta.ConfirmClearPrinters:
		return nil, errRefuseClearPrinters
	default:
		// Full replace: entries missing an ID (genuinely new printers added
		// client-side before this save) get one minted now. Entries that
		// already carry an ID (a rename/edit of an existing printer) keep it
		// untouched — the client must be able to echo back the ID it has.
		assignMissingIDs(newCfg.Printers)
		if old != nil {
			if err := rejectNewlyIntroducedDuplicateNames(old.Printers, newCfg.Printers); err != nil {
				return nil, err
			}
			if err := rejectNewlyBlankNames(old.Printers, newCfg.Printers); err != nil {
				return nil, err
			}
		}
	}
	applyStoredSecrets(&newCfg, old)
	return &newCfg, nil
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		defer configMutex.RUnlock()
		json.NewEncoder(w).Encode(redactConfig(configStore))
	case "POST":
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		oldCfg := configStore
		newCfg, err := resolveConfigUpdate(oldCfg, body)
		if err != nil {
			configMutex.Unlock()
			if errors.Is(err, errRefuseClearPrinters) || errors.Is(err, errDuplicatePrinterName) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		configStore = newCfg
		snapshot := newCfg.Clone()
		configMutex.Unlock()
		syncPrinterConnections(oldCfg, snapshot)
		if err := config.SaveConfig(snapshot); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePrinters(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		printers := make([]redactedPrinter, len(configStore.Printers))
		for i, p := range configStore.Printers {
			printers[i] = redactPrinter(p)
		}
		configMutex.RUnlock()
		json.NewEncoder(w).Encode(printers)
	case "POST":
		var p config.Printer
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if p.IsCloud() {
			// Network lookup (cached, paced) happens before the lock is taken.
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			err := resolveCloudPrinter(ctx, &p)
			cancel()
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
		} else {
			p.Connection = "" // LAN is the zero value; never store an unknown mode
		}

		// Trim on create only — an existing printer's name is its lookup key and
		// is never rewritten behind the user's back.
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": errBlankPrinterName.Error()})
			return
		}

		configMutex.Lock()
		if hasDuplicateName(configStore.Printers, p) {
			configMutex.Unlock()
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("a printer named %q already exists — printer names must be unique", p.Name)})
			return
		}
		// Server-generated only — a client-supplied id is never trusted on
		// create, so "generated once on creation" is an actual guarantee.
		p.ID = config.NewPrinterID()
		configStore.Printers = append(configStore.Printers, p)
		cfg := configStore.Clone()
		configMutex.Unlock()

		if err := config.SaveConfig(cfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		if p.IsCloud() {
			syncCloud(cfg)
		} else if isBambuPrinterConfig(p) {
			mqttpkg.ConnectPrinter(mqttPrinter(p, cfg))
		} else {
			lanCtrl.AddOrUpdatePrinter(p, cfg.APIKey, cfg.FoxTrack2APIKey)
			log.Printf("[%s] connected via Moonraker", p.Name)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "printer": redactPrinter(p)})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePrinterByName handles DELETE /api/printers/{token}. token is checked
// against printer IDs first, falling back to an exact name match if it
// matches no ID — so the current, name-only dashboard keeps working exactly
// as before, while a future ID-aware client can delete by ID with no further
// server change. Deletion internals (mqtt/lan teardown) are still
// name-keyed, so once a match is resolved we always operate on the matched
// printer's actual Name, never the raw token.
func handlePrinterByName(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method == "PATCH" {
		type patchBody struct {
			CameraHidden *bool `json:"camera_hidden"`
		}
		var pb patchBody
		if err := json.NewDecoder(r.Body).Decode(&pb); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if pb.CameraHidden == nil {
			http.Error(w, "camera_hidden is required", http.StatusBadRequest)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/api/printers/")
		if token == "" {
			http.Error(w, "missing printer id or name", http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		matchByID := false
		for _, p := range configStore.Printers {
			if p.ID != "" && p.ID == token {
				matchByID = true
				break
			}
		}
		var matched *config.Printer
		for i := range configStore.Printers {
			matches := configStore.Printers[i].Name == token
			if matchByID {
				matches = configStore.Printers[i].ID == token
			}
			if matches {
				matched = &configStore.Printers[i]
				break
			}
		}
		if matched == nil {
			configMutex.Unlock()
			http.Error(w, "printer not found", http.StatusNotFound)
			return
		}
		matched.CameraHidden = *pb.CameraHidden
		snapshot := *matched
		cfg := configStore.Clone()
		configMutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := config.SaveConfig(cfg); err != nil {
			http.Error(w, "failed to save config", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(redactPrinter(snapshot))
		return
	}
	if r.Method != "DELETE" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/api/printers/")
	if token == "" {
		http.Error(w, "missing printer id or name", http.StatusBadRequest)
		return
	}

	configMutex.Lock()
	matchByID := false
	for _, p := range configStore.Printers {
		if p.ID != "" && p.ID == token {
			matchByID = true
			break
		}
	}
	var removedNames []string
	printers := configStore.Printers[:0]
	for _, p := range configStore.Printers {
		matches := p.Name == token
		if matchByID {
			matches = p.ID == token
		}
		if matches {
			removedNames = append(removedNames, p.Name)
			continue
		}
		printers = append(printers, p)
	}
	if len(removedNames) == 0 {
		// Nothing matched: leave the stored list exactly as it was (the
		// compaction above is a no-op in that case) and say so, rather than
		// reporting a delete that never happened.
		configMutex.Unlock()
		http.Error(w, "printer not found", http.StatusNotFound)
		return
	}
	configStore.Printers = printers
	cfg := configStore.Clone()
	configMutex.Unlock()
	if err := config.SaveConfig(cfg); err != nil {
		log.Printf("Warning: failed to save config: %v", err)
	}
	// Stop whichever connection type each removed printer had. Both calls
	// are no-ops for names they don't manage, so no need to know which type
	// it was.
	for _, name := range removedNames {
		mqttpkg.DisconnectPrinter(name)
		mqttpkg.RemovePrinterState(name)
		lanCtrl.RemovePrinter(name)
	}
	if len(removedNames) > 0 {
		syncCloud(cfg) // unsubscribes a removed cloud printer without reconnecting
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// syncPrinterConnections reconciles running printer connections with cfg.
// oldCfg is the previously active config (nil at startup): Bambu printers whose
// connection-relevant fields (IP, serial, LAN code, webhook keys) changed are
// restarted, removed ones are disconnected and their state cleaned up, and
// unchanged ones are left untouched so their telemetry never blips.
func syncPrinterConnections(oldCfg, cfg *config.Config) {
	oldBambu := map[string]mqttpkg.Printer{}
	if oldCfg != nil {
		for _, p := range oldCfg.Printers {
			if isBambuPrinterConfig(p) && !p.IsCloud() {
				oldBambu[p.Name] = mqttPrinter(p, oldCfg)
			}
		}
	}

	for _, p := range cfg.Printers {
		if !isBambuPrinterConfig(p) || p.IsCloud() {
			continue // cloud printers share one account connection (syncCloud below)
		}
		newP := mqttPrinter(p, cfg)
		if old, ok := oldBambu[p.Name]; ok {
			delete(oldBambu, p.Name)
			if old == newP {
				continue // unchanged — leave the running connection alone
			}
			log.Printf("[%s] connection settings changed — restarting MQTT connection", p.Name)
			mqttpkg.DisconnectPrinter(p.Name)
		}
		mqttpkg.ConnectPrinter(newP)
	}

	// Bambu printers no longer in cfg (deleted, or switched to Klipper):
	// stop their goroutines and drop all per-printer state.
	for name := range oldBambu {
		mqttpkg.DisconnectPrinter(name)
		mqttpkg.RemovePrinterState(name)
	}

	lanCtrl.SyncPrinters(cfg.Printers, cfg.APIKey, cfg.FoxTrack2APIKey)
	syncCloud(cfg)
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

// handleHistory returns all print history records as JSON.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	records, err := history.Load()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"records": records})
}

// handleHistoryByPrinter returns print history for a single printer.
// URL: /api/history/{printerName}
func handleHistoryByPrinter(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/history/")
	name = strings.TrimSuffix(name, "/")
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if name == "" {
		http.Error(w, "missing printer name", http.StatusBadRequest)
		return
	}
	records, err := history.ForPrinter(name)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"records": records})
}

func isBambuPrinterConfig(p config.Printer) bool {
	if strings.TrimSpace(p.Serial) == "" {
		return false
	}
	if strings.TrimSpace(p.MoonrakerURL) != "" {
		return false // a Moonraker address always means Klipper
	}
	if p.IsCloud() {
		return true // the cloud reports the LAN code; its absence only disables the camera
	}
	return strings.TrimSpace(p.LANCode) != ""
}

// autoUpdateLoop runs in the background and applies updates automatically when
// the AutoUpdate setting is enabled. It waits 2 minutes on startup (so the
// user has a chance to disable it if needed), then checks every hour.
func autoUpdateLoop() {
	time.Sleep(2 * time.Minute)
	loggedReadOnly := false
	loggedDevBuild := false
	for {
		if !version.IsValid(version.AppVersion) {
			if !loggedDevBuild {
				log.Printf("[auto-update] development build (%s) — update checks disabled", version.AppVersion)
				loggedDevBuild = true
			}
			time.Sleep(1 * time.Hour)
			continue
		}

		configMutex.RLock()
		enabled := configStore != nil && configStore.AutoUpdate
		configMutex.RUnlock()

		if enabled && !update.CanReplaceExecutable() {
			if !loggedReadOnly {
				log.Printf("[auto-update] unavailable in this environment: the binary location is read-only — update by pulling a new image or replacing the binary")
				loggedReadOnly = true
			}
		} else if enabled {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			result, err := update.CheckLatest(ctx)
			cancel()
			if err != nil {
				log.Printf("[auto-update] check failed: %v", err)
			} else if result.Available && result.CanAutoInstall {
				log.Printf("[auto-update] new version %s available — downloading", result.LatestVersion)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
				err = update.StartInstall(ctx2)
				cancel2()
				if err != nil {
					log.Printf("[auto-update] install failed: %v", err)
				} else {
					log.Printf("[auto-update] update staged — restarting to apply %s", result.LatestVersion)
					if err := update.RestartToApply(); err != nil {
						log.Printf("[auto-update] restart failed: %v", err)
					} else {
						time.Sleep(700 * time.Millisecond)
						os.Exit(0)
					}
				}
			} else if !result.Available {
				log.Printf("[auto-update] already up to date (%s)", result.CurrentVersion)
			} else {
				log.Printf("[auto-update] update available but cannot auto-install on this platform")
			}
		}

		time.Sleep(1 * time.Hour)
	}
}
