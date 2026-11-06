package lan

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/capture"
	configpkg "foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/pace"
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

type printSession struct {
	StartTime   int64
	FileName    string
	NozzleTemp  float64
	BedTemp     float64
	TempSamples []history.TempSample
	lastSampleT int64
	// lostSince is the first failed poll of the current outage, 0 while
	// polls succeed. lastProgress and lastFilamentMM are the last values a
	// successful poll saw, so an outage that outlives pollGrace still
	// records what the print had reached.
	lostSince      int64
	lastProgress   int
	lastFilamentMM float64
}

// pollGrace is how long a print survives failed polls before the Bridge gives
// up and records it as cancelled. A short outage (Moonraker restarting, Wi-Fi
// blinking) must not end the print: Moonraker's filament_used counts from the
// job start, so a print split into two sessions would report the same
// filament to FoxTrack twice. Bambu printers get the same grace in mqtt.
const pollGrace = 10 * time.Minute

// sessionEnd is what the poll loop records and sends when a print ends.
type sessionEnd struct {
	rec            history.Record
	percentAtEnd   int
	filamentUsedMM float64
}

// updateSession applies one poll to the printer's print session and returns
// the print that this poll ended, if any. When a print ends, the file Bridge
// uploaded for it is deleted from the printer (deleteEndedUpload).
func (c *Controller) updateSession(name string, t *mqttpkg.TelemetryData, pollErr error, now int64) *sessionEnd {
	end := c.applyPoll(name, t, pollErr, now)
	if end != nil {
		c.deleteEndedUpload(name, end.rec.FileName)
	}
	return end
}

// applyPoll is the session bookkeeping behind updateSession. pollErr is set when
// the poll failed; t is then a "disconnected" placeholder and is not trusted.
func (c *Controller) applyPoll(name string, t *mqttpkg.TelemetryData, pollErr error, now int64) *sessionEnd {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	sess := c.sessions[name]

	if pollErr != nil {
		if sess == nil {
			return nil
		}
		if sess.lostSince == 0 {
			sess.lostSince = now
		}
		if now-sess.lostSince < int64(pollGrace/time.Second) {
			return nil
		}
		delete(c.sessions, name)
		return endOf(name, sess, "cancelled", sess.lastProgress, sess.lastFilamentMM, now)
	}

	// Print A ended and print B started while the printer was unreachable:
	// close A with what was last seen, then start B below.
	var ended *sessionEnd
	if sess != nil && sess.lostSince != 0 && t.Status == "printing" &&
		sess.FileName != "" && t.FileName != "" && t.FileName != sess.FileName {
		ended = endOf(name, sess, "cancelled", sess.lastProgress, sess.lastFilamentMM, now)
		sess = nil
	}

	if sess == nil {
		if t.Status == "printing" {
			c.sessions[name] = &printSession{
				StartTime:      now,
				FileName:       t.FileName,
				NozzleTemp:     t.NozzleTemp,
				BedTemp:        t.BedTemp,
				TempSamples:    []history.TempSample{{T: 0, NozzleTemp: t.NozzleTemp, BedTemp: t.BedTemp}},
				lastSampleT:    now,
				lastProgress:   t.Progress,
				lastFilamentMM: t.FilamentUsedMM,
			}
		}
		return ended
	}

	if mqttpkg.InPrint(t.Status) {
		// Ongoing print. A pause is part of it (mqttpkg.InPrint), so it
		// neither ends the session nor starts a new one on resume.
		sess.lostSince = 0
		sess.lastProgress = t.Progress
		sess.lastFilamentMM = max(t.FilamentUsedMM, sess.lastFilamentMM)
		if sess.FileName == "" && t.FileName != "" {
			sess.FileName = t.FileName
		}
		if t.Status == "printing" && now-sess.lastSampleT >= 60 {
			sess.TempSamples = append(sess.TempSamples, history.TempSample{
				T:          now - sess.StartTime,
				NozzleTemp: t.NozzleTemp,
				BedTemp:    t.BedTemp,
			})
			sess.lastSampleT = now
		}
		return nil
	}

	// A real non-print status: the print ended.
	delete(c.sessions, name)
	result := t.Status
	if result != "finished" && result != "error" {
		result = "cancelled"
	}
	percent := sess.lastProgress
	if result == "finished" {
		percent = 100
	}
	// filament_used counts up through the job; keep the largest value seen.
	return endOf(name, sess, result, percent, max(t.FilamentUsedMM, sess.lastFilamentMM), now)
}

func endOf(name string, sess *printSession, result string, percent int, filamentMM float64, now int64) *sessionEnd {
	return &sessionEnd{
		rec: history.Record{
			PrinterName:  name,
			FileName:     sess.FileName,
			NozzleTemp:   sess.NozzleTemp,
			BedTemp:      sess.BedTemp,
			StartTime:    sess.StartTime,
			EndTime:      now,
			Duration:     now - sess.StartTime,
			Result:       result,
			TempSamples:  sess.TempSamples,
			PercentAtEnd: percent,
			FilamentMM:   filamentMM,
		},
		percentAtEnd:   percent,
		filamentUsedMM: filamentMM,
	}
}

// recordEnd writes an ended print to the local history and tells FoxTrack.
func (c *Controller) recordEnd(name, fox2APIKey string, end *sessionEnd) {
	rec := end.rec
	// PrintEnergy hands the reading over once: take it for both.
	energy := mqttpkg.PrintEnergy(name)
	rec.EnergyWh = energy
	if err := history.Append(rec); err != nil {
		log.Printf("[%s] history write error: %v", name, err)
	}
	if fox2APIKey == "" {
		return
	}
	webhook.SendHistory(fox2APIKey, webhook.HistoryURLV2, webhook.HistoryPayload{
		Type:           "print_complete",
		PrinterName:    name,
		Serial:         name,
		FileName:       rec.FileName,
		NozzleTemp:     rec.NozzleTemp,
		BedTemp:        rec.BedTemp,
		StartTime:      time.Unix(rec.StartTime, 0).UTC().Format(time.RFC3339),
		EndTime:        time.Unix(rec.EndTime, 0).UTC().Format(time.RFC3339),
		Duration:       rec.Duration,
		Result:         rec.Result,
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		PercentAtEnd:   end.percentAtEnd,
		FilamentUsedMM: end.filamentUsedMM,
		EnergyWh:       energy,
	})
}

type Controller struct {
	mu             sync.RWMutex
	states         map[string]*mqttpkg.TelemetryData
	printers       map[string]configpkg.Printer
	cancelers      map[string]chan struct{}
	sessionMu      sync.Mutex
	sessions       map[string]*printSession
	snapLastT      map[string]int64
	snapLastTMu    sync.Mutex
	snapInFlight   map[string]bool
	snapInFlightMu sync.Mutex
	relayLastT     map[string]int64
	relayHeld      map[string]bool
	relayLastTMu   sync.Mutex
	relayQMu       sync.Mutex
	relayBusy      map[string]bool
	relayPending   map[string]*relayJob
	uploaded       map[string]string // printer -> file UploadAndPrint started; guarded by mu
}

func NewController() *Controller {
	return &Controller{
		states:       map[string]*mqttpkg.TelemetryData{},
		printers:     map[string]configpkg.Printer{},
		cancelers:    map[string]chan struct{}{},
		sessions:     map[string]*printSession{},
		snapLastT:    map[string]int64{},
		snapInFlight: map[string]bool{},
		relayLastT:   map[string]int64{},
		relayHeld:    map[string]bool{},
		relayBusy:    map[string]bool{},
		relayPending: map[string]*relayJob{},
		uploaded:     map[string]string{},
	}
}

// relayJob is one telemetry post to FoxTrack, for either API key.
type relayJob struct {
	foxAPIKey, fox2APIKey string
	payload               webhook.RelayPayload
}

// sendRelay is webhook.SendRelay; tests swap it.
var sendRelay = webhook.SendRelay

// queueRelay posts to FoxTrack off the poll loop, so a slow or failing
// webhook (10 s timeout, twice) cannot stretch the 2 s poll. One post is in
// flight per printer; while it runs only the newest later payload is kept,
// since each payload is a full snapshot and replaces the one before it.
func (c *Controller) queueRelay(name string, job relayJob) {
	c.relayQMu.Lock()
	if c.relayBusy[name] {
		c.relayPending[name] = &job
		c.relayQMu.Unlock()
		return
	}
	c.relayBusy[name] = true
	c.relayQMu.Unlock()

	go func() {
		for {
			if job.foxAPIKey != "" {
				if err := sendRelay(job.foxAPIKey, webhook.URL, name, name, job.payload); err != nil {
					log.Printf("[%s] webhook error: %v", name, err)
				}
			}
			if job.fox2APIKey != "" {
				if err := sendRelay(job.fox2APIKey, webhook.RelayURLV2, name, name, job.payload); err != nil {
					log.Printf("[%s] webhook error (v2): %v", name, err)
				}
			}
			c.relayQMu.Lock()
			next := c.relayPending[name]
			delete(c.relayPending, name)
			if next == nil {
				delete(c.relayBusy, name)
				c.relayQMu.Unlock()
				return
			}
			c.relayQMu.Unlock()
			job = *next
		}
	}()
}

// relayHeartbeatSec floors how often telemetry reaches FoxTrack, whether or not
// anything changed. FoxTrack marks a printer offline after 90s of silence and
// disables its controls, so the floor must sit comfortably under that.
const relayHeartbeatSec = pace.HeartbeatSec

// shouldRelay reports whether this poll should push telemetry to FoxTrack, and
// records the send when it says yes.
//
// A change-only gate is not enough: an idle Klipper printer settles at ambient
// and then reports the same numbers for minutes at a time, so it used to fall
// silent and show as offline on the website while the bridge was perfectly
// healthy, taking its pause/stop buttons with it. mqtt.go already applies this
// same one-a-minute heartbeat to Bambu printers.
//
// Status, file, error and light changes go out at once; progress and
// temperatures follow at the pace FoxTrack asked for (pace.Decide).
func (c *Controller) shouldRelay(name string, prev, curr *mqttpkg.TelemetryData, now int64) bool {
	c.relayLastTMu.Lock()
	defer c.relayLastTMu.Unlock()

	send, held := pace.Decide(
		mqttpkg.UrgentChange(prev, curr),
		mqttpkg.ShouldSendWebhook(prev, curr),
		c.relayHeld[name],
		now-c.relayLastT[name],
		now,
	)
	c.relayHeld[name] = held
	if send {
		c.relayLastT[name] = now
	}
	return send
}

func (c *Controller) SyncPrinters(printers []configpkg.Printer, foxAPIKey, fox2APIKey string) {
	keep := make(map[string]bool)
	for _, p := range printers {
		if p.Kind() == configpkg.KindBambu {
			continue
		}
		keep[p.Name] = true
		c.AddOrUpdatePrinter(p, foxAPIKey, fox2APIKey)
	}

	var gone []string
	c.mu.Lock()
	for name, stop := range c.cancelers {
		if !keep[name] {
			close(stop)
			delete(c.cancelers, name)
			delete(c.printers, name)
			delete(c.states, name)
			gone = append(gone, name)
		}
	}
	c.mu.Unlock()
	for _, name := range gone {
		c.forgetBookkeeping(name)
	}
}

func (c *Controller) AddOrUpdatePrinter(p configpkg.Printer, foxAPIKey, fox2APIKey string) {
	if p.Name == "" {
		return
	}
	if p.Kind() == configpkg.KindBambu {
		return
	}

	c.mu.Lock()
	if stop, ok := c.cancelers[p.Name]; ok {
		close(stop)
		delete(c.cancelers, p.Name)
	}
	stop := make(chan struct{})
	c.cancelers[p.Name] = stop
	c.printers[p.Name] = p
	c.mu.Unlock()

	go c.pollLoop(p, foxAPIKey, fox2APIKey, stop)
}

func (c *Controller) RemovePrinter(name string) {
	c.mu.Lock()
	if stop, ok := c.cancelers[name]; ok {
		close(stop)
		delete(c.cancelers, name)
	}
	delete(c.printers, name)
	delete(c.states, name)
	c.mu.Unlock()
	c.forgetBookkeeping(name)
}

// forgetBookkeeping drops a removed printer's print session and relay pacing,
// so a printer later added under the same name starts clean.
func (c *Controller) forgetBookkeeping(name string) {
	c.sessionMu.Lock()
	delete(c.sessions, name)
	c.sessionMu.Unlock()
	c.relayLastTMu.Lock()
	delete(c.relayLastT, name)
	delete(c.relayHeld, name)
	c.relayLastTMu.Unlock()
	c.snapLastTMu.Lock()
	delete(c.snapLastT, name)
	c.snapLastTMu.Unlock()
	c.relayQMu.Lock()
	delete(c.relayPending, name)
	c.relayQMu.Unlock()
}

// RenamePrinter moves a printer's print session and relay pacing from oldName
// to newName, so renaming a printer mid-print keeps the print going instead of
// ending it as cancelled. Call it before RemovePrinter(oldName).
func (c *Controller) RenamePrinter(oldName, newName string) {
	if oldName == newName {
		return
	}
	c.sessionMu.Lock()
	if s, ok := c.sessions[oldName]; ok {
		c.sessions[newName] = s
		delete(c.sessions, oldName)
	}
	c.sessionMu.Unlock()
	c.relayLastTMu.Lock()
	if v, ok := c.relayLastT[oldName]; ok {
		c.relayLastT[newName] = v
		delete(c.relayLastT, oldName)
	}
	if v, ok := c.relayHeld[oldName]; ok {
		c.relayHeld[newName] = v
		delete(c.relayHeld, oldName)
	}
	c.relayLastTMu.Unlock()
	c.snapLastTMu.Lock()
	if v, ok := c.snapLastT[oldName]; ok {
		c.snapLastT[newName] = v
		delete(c.snapLastT, oldName)
	}
	c.snapLastTMu.Unlock()
}

func (c *Controller) GetStates() map[string]*mqttpkg.TelemetryData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*mqttpkg.TelemetryData, len(c.states))
	for k, v := range c.states {
		copyV := *v
		out[k] = &copyV
	}
	return out
}

func (c *Controller) SendCommand(name, command string, args map[string]interface{}) error {
	c.mu.RLock()
	p, ok := c.printers[name]
	state := c.states[name]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("printer %q not found", name)
	}

	switch p.Kind() {
	case configpkg.KindPrusaLink:
		if mqttpkg.IsConsoleCommand(command) {
			return errPrusaNoConsole
		}
		return sendPrusaLinkCommand(p, command)
	case configpkg.KindOctoPrint:
		return sendOctoPrintCommand(p, command, args)
	case configpkg.KindHomeAssistant:
		return errReadOnly
	}
	return c.sendKlipperCommand(p, state, command, args)
}

// snapshotURL is the still-image address the snapshot sender reads: the
// webcam URL the user typed, else, for OctoPrint, the snapshot address the
// server itself reports.
func snapshotURL(p configpkg.Printer) string {
	if u := strings.TrimSpace(p.WebcamURL); u != "" {
		return u
	}
	if p.Kind() == configpkg.KindOctoPrint {
		return octoPrintSnapshotURL(p)
	}
	return ""
}

// cameraTransport bounds only the wait for the camera to answer. An MJPEG
// stream never ends, so a total client timeout would cut it every few seconds.
var cameraTransport = &http.Transport{
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	TLSHandshakeTimeout:   5 * time.Second,
	ResponseHeaderTimeout: 8 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}

// cameraStillLimit caps how long a still image may take to arrive in full.
var cameraStillLimit = 8 * time.Second

func (c *Controller) ProxyCamera(w http.ResponseWriter, r *http.Request, name string) error {
	c.mu.RLock()
	p, ok := c.printers[name]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("printer not found")
	}

	candidates := cameraCandidates(p)
	// The API key goes to the printer's own host only: OctoPrint may name a
	// webcam on another machine, and a redirect may lead anywhere. Go drops
	// Authorization on a cross-host redirect but keeps a custom X-Api-Key.
	apiBase := p.MoonrakerURL
	if p.Kind() == configpkg.KindOctoPrint {
		apiBase = p.OctoPrintURL
	}
	client := &http.Client{
		Transport: cameraTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if !sameHost(req.URL.String(), apiBase) {
				req.Header.Del("X-Api-Key")
			}
			return nil
		},
	}
	for _, candidate := range candidates {
		req, err := http.NewRequestWithContext(r.Context(), "GET", candidate, nil)
		if err != nil {
			continue
		}
		if (p.Kind() == configpkg.KindKlipper || p.Kind() == configpkg.KindOctoPrint) && sameHost(candidate, apiBase) {
			applyMoonrakerAuth(req, p)
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			continue
		}

		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}
		if !strings.HasPrefix(contentType, "multipart/") {
			// A still image is bounded; only a multipart (MJPEG) stream may run on.
			defer time.AfterFunc(cameraStillLimit, func() { resp.Body.Close() }).Stop()
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return nil
	}

	return fmt.Errorf("camera unavailable")
}

// storeState records a poll's result and returns the previous state. It
// reports false, and stores nothing, when stop is no longer the printer's
// registered canceler: the printer was removed (or replaced) while the poll was
// in flight, and writing would bring a removed printer back.
func (c *Controller) storeState(name string, stop <-chan struct{}, t *mqttpkg.TelemetryData) (*mqttpkg.TelemetryData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelers[name] != stop {
		return nil, false
	}
	prev := c.states[name]
	c.states[name] = t
	return prev, true
}

func (c *Controller) pollLoop(p configpkg.Printer, foxAPIKey, fox2APIKey string, stop <-chan struct{}) {
	timer := time.NewTimer(0) // fire immediately on first iteration
	defer timer.Stop()

	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[%s] poll panic: %v", p.Name, r)
					timer.Reset(15 * time.Second)
				}
			}()

			t, relayPayload, err := fetchTelemetry(p)
			if err != nil {
				t = mqttpkg.TelemetryData{Status: "disconnected", Error: pollErrorText(err)}
			}

			t.PrinterID = p.Name
			t.Timestamp = time.Now().Unix()
			// The plug is read by its own poller; attach its latest state here so
			// the dashboard and FoxTrack see it with the printer.
			t.Plug = mqttpkg.PlugState(p.Name)
			relayPayload.Print.Plug = t.Plug
			relayPayload.Print.BridgeCapabilities = mqttpkg.WithPlugCaps(relayPayload.Print.BridgeCapabilities, t.Plug != nil)

			// Capture previous state before overwriting, then detect print transitions.
			prev, ok := c.storeState(p.Name, stop, &t)
			if !ok {
				return // the printer was removed or replaced while this poll ran
			}

			if end := c.updateSession(p.Name, &t, err, time.Now().Unix()); end != nil {
				go c.recordEnd(p.Name, fox2APIKey, end)
			}
			if err == nil && c.shouldRelay(p.Name, prev, &t, time.Now().Unix()) {
				c.queueRelay(p.Name, relayJob{foxAPIKey: foxAPIKey, fox2APIKey: fox2APIKey, payload: relayPayload})
			}

			if fox2APIKey != "" && (t.Status == "printing" || t.Status == "paused") && snapshotURL(p) != "" {
				now := time.Now().Unix()
				c.snapLastTMu.Lock()
				eligible := now-c.snapLastT[p.Name] >= pace.SnapshotGap(now)
				if eligible {
					c.snapLastT[p.Name] = now
				}
				c.snapLastTMu.Unlock()

				if eligible {
					c.snapInFlightMu.Lock()
					busy := c.snapInFlight[p.Name]
					if !busy {
						c.snapInFlight[p.Name] = true
					}
					c.snapInFlightMu.Unlock()

					if !busy {
						webcamURL := snapshotURL(p)
						name := p.Name
						apiKey := fox2APIKey
						go func() {
							defer func() {
								c.snapInFlightMu.Lock()
								delete(c.snapInFlight, name)
								c.snapInFlightMu.Unlock()
							}()
							frame, err := capture.KlipperFrame(webcamURL)
							if err != nil {
								log.Printf("[%s] snapshot capture: %v", name, err)
								return
							}
							if err := webhook.SendSnapshot(apiKey, webhook.SnapshotURLV2, name, name, frame); err != nil {
								log.Printf("[%s] snapshot send: %v", name, err)
							}
						}()
					}
				}
			}

			// Adaptive poll interval: faster while printing, slower when idle/disconnected.
			var interval time.Duration
			switch t.Status {
			case "printing":
				interval = 2 * time.Second
			case "paused":
				interval = 5 * time.Second
			case "disconnected":
				interval = 5 * time.Second
			default:
				interval = 8 * time.Second
			}
			timer.Reset(interval)
		}()

		select {
		case <-stop:
			return
		default:
		}
	}
}

// pollErrorText words a failed poll for the card's Offline line. A raw Go error
// (the request URL, "dial tcp ...: i/o timeout") tells a user nothing, and the
// dashboard only rewrites some of them; the full error is in the log. An error
// that is already a sentence passes through as it is.
func pollErrorText(err error) string {
	msg := err.Error()
	var code int
	if _, e := fmt.Sscanf(msg, "HTTP %d", &code); e == nil && strings.HasPrefix(msg, "HTTP ") {
		switch code {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errMoonKey.Error()
		case http.StatusNotFound:
			return "The printer answered, but not as expected (HTTP 404). Check the address in the printer settings in Bridge."
		}
		return fmt.Sprintf("The printer answered with an error (HTTP %d). Check that its software is running.", code)
	}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return "The address answered, but not like the printer's software. Check the address in the printer settings in Bridge."
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return msg
	}
	host := "the printer"
	if u, e := url.Parse(ue.URL); e == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "no such host") {
		return fmt.Sprintf("Can't find %s. Check the address in the printer settings in Bridge.", host)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Sprintf("Can't reach %s. Check that the printer is on and on the same network.", host)
	}
	for _, s := range []string{"connection refused", "no route to host", "unreachable", "host is down", "dial tcp", "eof", "reset by peer"} {
		if strings.Contains(low, s) {
			return fmt.Sprintf("Can't reach %s. Check that the printer is on and on the same network.", host)
		}
	}
	return msg
}

func sendJSONRequest(client *http.Client, method, u string, headers map[string]string, body io.Reader) (map[string]interface{}, error) {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out map[string]interface{}
	// An empty body (PrusaLink answers 204 No Content) is a success, not a decode error.
	// Capped: a misbehaving printer could stream without end inside the timeout.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONReply)).Decode(&out); err != nil && err != io.EOF {
		return nil, err
	}
	return out, nil
}

// fetchTelemetry polls one HTTP printer, whichever kind it is.
func fetchTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	switch p.Kind() {
	case configpkg.KindPrusaLink:
		return fetchPrusaLinkTelemetry(p)
	case configpkg.KindOctoPrint:
		return fetchOctoPrintTelemetry(p)
	case configpkg.KindHomeAssistant:
		return fetchHomeAssistantTelemetry(p)
	}
	return fetchKlipperTelemetry(p)
}

// klipperHours caches the Moonraker lifetime print hours per printer.
// It is re-fetched at most once every 5 minutes to avoid per-poll overhead.
var (
	klipperHoursCache   = make(map[string]float64)
	klipperHoursUpdated = make(map[string]time.Time)
	klipperHoursMu      sync.Mutex
)

func klipperPrintHours(p configpkg.Printer) float64 {
	klipperHoursMu.Lock()
	if t, ok := klipperHoursUpdated[p.Name]; ok && time.Since(t) < 5*time.Minute {
		h := klipperHoursCache[p.Name]
		klipperHoursMu.Unlock()
		return h
	}
	klipperHoursMu.Unlock()

	client := &http.Client{Timeout: 3 * time.Second, Transport: insecureTransport()}
	m, err := sendJSONRequest(client, "GET", moonrakerURL(p, "/server/history/totals"), moonrakerAuthHeaders(p), nil)
	klipperHoursMu.Lock()
	defer klipperHoursMu.Unlock()
	klipperHoursUpdated[p.Name] = time.Now() // mark updated even on error to avoid retry every poll
	if err == nil {
		totals := nestedMap(m, "result", "job_totals")
		if secs := floatAny(anyFromMap(totals, "total_print_time")); secs > 0 {
			klipperHoursCache[p.Name] = secs / 3600
		}
	}
	return klipperHoursCache[p.Name]
}

func fetchKlipperTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	u := moonrakerURL(p, "/printer/objects/query?print_stats&heater_bed&extruder&extruder1&extruder2&extruder3&toolhead&fan&display_status&virtual_sdcard&webhooks")
	m, err := sendJSONRequest(client, "GET", u, moonrakerAuthHeaders(p), nil)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}

	status := nestedMap(m, "result", "status")
	printStats := nestedMapAny(status, "print_stats")
	bed := nestedMapAny(status, "heater_bed")
	displayStatus := nestedMapAny(status, "display_status")
	virtualSD := nestedMapAny(status, "virtual_sdcard")

	// Determine active extruder. Klipper reports the current toolhead extruder name
	// (e.g. "extruder", "extruder1", "extruder2") via the toolhead object.
	toolhead := nestedMapAny(status, "toolhead")
	activeExtruderName := stringAny(anyFromMap(toolhead, "extruder"))
	if activeExtruderName == "" {
		activeExtruderName = "extruder"
	}
	extruder := nestedMapAny(status, activeExtruderName)
	if len(extruder) == 0 {
		extruder = nestedMapAny(status, "extruder")
		activeExtruderName = "extruder"
	}

	// Only expose active_extruder when it differs from the default single-extruder name.
	activeExtruderField := ""
	if activeExtruderName != "extruder" {
		activeExtruderField = activeExtruderName
	}

	stateRaw := lowerString(stringAny(anyFromMap(printStats, "state")))
	state := mapKlipperState(stateRaw)
	fileName := stringAny(anyFromMap(printStats, "filename"))
	progressF := floatAny(anyFromMap(displayStatus, "progress"))
	if progressF == 0 {
		progressF = floatAny(anyFromMap(virtualSD, "progress"))
	}
	progress := int(progressF * 100)
	if progress > 100 {
		progress = 100
	}
	if progress < 0 {
		progress = 0
	}

	printDuration := int(floatAny(anyFromMap(printStats, "print_duration")))
	remaining := 0
	if progress > 0 && progress < 100 && printDuration > 0 {
		total := int(float64(printDuration) / (float64(progress) / 100.0))
		if total > printDuration {
			remaining = (total - printDuration) / 60
		}
	}

	fanSpeed := floatAny(anyFromMap(nestedMapAny(status, "fan"), "speed")) // 0.0-1.0
	coolingFanPct := int(fanSpeed * 100)
	filamentUsedMM := floatAny(anyFromMap(printStats, "filament_used"))
	alarms := klipperAlarms(nestedMapAny(status, "webhooks"), printStats)

	relay := webhook.RelayPayload{
		Print: webhook.RelayPrint{
			GcodeState:         mapMoonrakerRelayState(stateRaw),
			SubTaskName:        fileName,
			McPercent:          progress,
			NozzleTemper:       floatAny(anyFromMap(extruder, "temperature")),
			NozzleTargetTemper: floatAny(anyFromMap(extruder, "target")),
			BedTemper:          floatAny(anyFromMap(bed, "temperature")),
			BedTargetTemper:    floatAny(anyFromMap(bed, "target")),
			McRemainingTime:    remaining,
			ActiveExtruder:     activeExtruderField,
			Alarms:             alarms,
			PrinterKind:        "klipper",
			PrinterModel:       "Klipper",
			BridgeVersion:      version.AppVersion,
			BridgeCapabilities: []string{"print_file", "filament_use", "alarms", "console", "files"},
		},
	}

	return mqttpkg.TelemetryData{
		Status:          state,
		FileName:        fileName,
		Progress:        progress,
		NozzleTemp:      floatAny(anyFromMap(extruder, "temperature")),
		NozzleTarget:    floatAny(anyFromMap(extruder, "target")),
		BedTemp:         floatAny(anyFromMap(bed, "temperature")),
		BedTarget:       floatAny(anyFromMap(bed, "target")),
		TimeRemaining:   remaining,
		ActiveExtruder:  activeExtruderField,
		CoolingFanPct:   coolingFanPct,
		FilamentUsedMM:  filamentUsedMM,
		TotalPrintHours: klipperPrintHours(p),
		PrinterModel:    "Klipper",
		Alarms:          alarms,
	}, relay, nil
}

// klipperAlarms reads Klipper's own fault reports: webhooks.state "shutdown"
// or "error" with state_message (a firmware shutdown, fatal), and
// print_stats.state "error" with print_stats.message (the print failed,
// serious). Both texts are stable while the fault lasts, which is what
// FoxTrack keys a text-only alarm on.
func klipperAlarms(webhooks, printStats map[string]interface{}) []webhook.RelayAlarm {
	var out []webhook.RelayAlarm
	switch lowerString(stringAny(anyFromMap(webhooks, "state"))) {
	case "shutdown", "error":
		msg := strings.TrimSpace(stringAny(anyFromMap(webhooks, "state_message")))
		if msg == "" {
			msg = "Klipper has shut down. Restart the firmware."
		}
		out = append(out, webhook.MessageAlarm(webhook.AlarmFatal, msg))
	}
	if lowerString(stringAny(anyFromMap(printStats, "state"))) == "error" {
		msg := strings.TrimSpace(stringAny(anyFromMap(printStats, "message")))
		if msg == "" {
			msg = "The print stopped with an error."
		}
		out = append(out, webhook.MessageAlarm(webhook.AlarmSerious, msg))
	}
	return out
}

func mapKlipperState(state string) string {
	switch state {
	case "printing":
		return "printing"
	case "paused":
		return "paused"
	case "complete", "completed":
		return "finished"
	case "error":
		return "error"
	case "standby", "ready":
		return "idle"
	default:
		if state == "" {
			return "connected"
		}
		return state
	}
}

func mapMoonrakerRelayState(state string) string {
	switch state {
	case "printing":
		return "RUNNING"
	case "paused":
		return "PAUSE"
	case "complete", "completed":
		return "FINISH"
	case "error":
		return "FAILED"
	case "standby", "ready":
		return "IDLE"
	default:
		if state == "" {
			return "IDLE"
		}
		return strings.ToUpper(state)
	}
}

func (c *Controller) sendKlipperCommand(p configpkg.Printer, state *mqttpkg.TelemetryData, command string, args map[string]interface{}) error {
	client := &http.Client{Timeout: 6 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)

	switch command {
	case "pause":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/pause"), headers, nil)
		return err
	case "resume":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/resume"), headers, nil)
		return err
	case "stop":
		_, err := sendJSONRequest(client, "POST", moonrakerURL(p, "/printer/print/cancel"), headers, nil)
		return err
	case "start":
		filename := getArgString(args, "file_name")
		if filename == "" {
			filename = getArgString(args, "file")
		}
		if filename == "" {
			return fmt.Errorf("start requires file_name")
		}
		startURL := moonrakerURL(p, "/printer/print/start?filename="+url.QueryEscape(filename))
		_, err := sendJSONRequest(client, "POST", startURL, headers, nil)
		if err != nil && err.Error() == "HTTP 404" {
			return fmt.Errorf("The printer has no file with that name. Check the name in the printer's file list.")
		}
		return err
	case "fan_speed":
		pct := 0
		if v, ok := args["pct"].(float64); ok {
			pct = int(v)
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		s := pct * 255 / 100
		script := fmt.Sprintf("M106 S%d", s)
		gcodeURL := moonrakerURL(p, "/printer/gcode/script?script="+url.QueryEscape(script))
		_, err := sendJSONRequest(client, "POST", gcodeURL, headers, nil)
		return err
	case "set_nozzle_temp", "set_bed_temp", "home":
		// The console (Bridge 2.5.2): the same G-code as Bambu, through Moonraker.
		script, err := mqttpkg.ConsoleGcode(command, args)
		if err != nil {
			return err
		}
		gcodeURL := moonrakerURL(p, "/printer/gcode/script?script="+url.QueryEscape(script))
		if command == "home" {
			// Moonraker answers a script only once it has run, and G28 takes a minute.
			client = &http.Client{Timeout: 2 * time.Minute, Transport: insecureTransport()}
		}
		_, err = sendJSONRequest(client, "POST", gcodeURL, headers, nil)
		return err
	case "light", "toggle_light", "light_on", "light_off":
		desiredOn := false
		hasDesired := false
		if command == "light_on" {
			desiredOn = true
			hasDesired = true
		}
		if command == "light_off" {
			hasDesired = true
		}
		if !hasDesired {
			if v, ok := args["on"].(bool); ok {
				desiredOn = v
				hasDesired = true
			}
		}
		if !hasDesired {
			desiredOn = !(state != nil && state.LightOn)
		}
		device, err := c.findKlipperLightDevice(client, p)
		if err != nil {
			return err
		}
		action := "off"
		if desiredOn {
			action = "on"
		}
		return setKlipperLight(client, p, device, action)
	default:
		return fmt.Errorf("unsupported command for klipper: %s", command)
	}
}

func (c *Controller) findKlipperLightDevice(client *http.Client, p configpkg.Printer) (string, error) {
	u := moonrakerURL(p, "/machine/device_power/devices")
	m, err := sendJSONRequest(client, "GET", u, moonrakerAuthHeaders(p), nil)
	if err != nil {
		return "", err
	}

	result, ok := m["result"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid device list")
	}
	devices, ok := result["devices"].([]interface{})
	if !ok || len(devices) == 0 {
		return "", fmt.Errorf("no controllable power devices found")
	}

	var fallback string
	for _, d := range devices {
		name := ""
		switch v := d.(type) {
		case string:
			name = v
		case map[string]interface{}:
			name = stringAny(v["device"])
			if name == "" {
				name = stringAny(v["name"])
			}
		}
		if name == "" {
			continue
		}
		if fallback == "" {
			fallback = name
		}
		lc := strings.ToLower(name)
		if strings.Contains(lc, "light") || strings.Contains(lc, "led") || strings.Contains(lc, "lamp") {
			return name, nil
		}
	}
	if fallback == "" {
		return "", fmt.Errorf("no valid power device name found")
	}
	return fallback, nil
}

func setKlipperLight(client *http.Client, p configpkg.Printer, device, action string) error {
	headers := moonrakerAuthHeaders(p)
	queries := []string{
		moonrakerURL(p, "/machine/device_power/device?device="+url.QueryEscape(device)+"&action="+url.QueryEscape(action)),
		moonrakerURL(p, "/machine/device_power/set?device="+url.QueryEscape(device)+"&action="+url.QueryEscape(action)),
	}
	for _, q := range queries {
		_, err := sendJSONRequest(client, "POST", q, headers, nil)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("failed to toggle light device %q", device)
}

func cameraCandidates(p configpkg.Printer) []string {
	if isBambuPrinter(p) {
		return []string{fmt.Sprintf("https://%s:6000/mjpeg/1", p.IP)}
	}

	// If the user specified a webcam URL, try it first before guessing.
	if strings.TrimSpace(p.WebcamURL) != "" {
		return []string{strings.TrimSpace(p.WebcamURL)}
	}
	if p.Kind() == configpkg.KindOctoPrint {
		// OctoPrint says where its webcam is; OctoPi's default paths come next.
		var out []string
		if u := octoPrintStreamURL(p); u != "" {
			out = append(out, u)
		}
		if u := octoPrintSnapshotURL(p); u != "" {
			out = append(out, u)
		}
		base := strings.TrimRight(strings.TrimSpace(p.OctoPrintURL), "/")
		return append(out, base+"/webcam/?action=stream", base+"/webcam/?action=snapshot")
	}
	if p.Kind() != configpkg.KindKlipper {
		return nil // PrusaLink and Home Assistant have no stream to guess
	}

	base := strings.TrimRight(p.MoonrakerURL, "/")
	out := []string{
		base + "/webcam/?action=stream",
		base + "/webcam/?action=snapshot",
		base + "/snapshot",
	}
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Hostname() != "" {
		// Derive the plain host (no port) to try port-80 nginx/crowsnest paths
		// and port-8080 mjpg-streamer paths, which are common in Mainsail/Fluidd setups.
		host := u.Scheme + "://" + u.Hostname()
		out = append(out,
			host+"/webcam/?action=stream",
			host+"/webcam/?action=snapshot",
			host+":8080/?action=stream",
			host+":8080/stream",
		)
	}

	return out
}

// sharedInsecureTransport is created once and reused by every Moonraker and
// webcam request. An http.Transport owns a connection pool, so building a fresh
// one per request meant every poll opened a new keep-alive connection that the
// discarded transport never closed. With a 2-8s poll interval those idle
// sockets accumulated for the life of the process until the bridge ran out of
// file descriptors. One shared transport reuses connections and, unlike a bare
// &http.Transport{}, retires idle ones on a timer. Transports are safe for
// concurrent use by design.
var sharedInsecureTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 4,
	IdleConnTimeout:     90 * time.Second,
}

func insecureTransport() *http.Transport {
	return sharedInsecureTransport
}

func getArgString(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// maxJSONReply caps a printer's JSON reply. Moonraker's history and file
// lists stay well under it.
const maxJSONReply = 16 << 20

func moonrakerURL(p configpkg.Printer, path string) string {
	base := strings.TrimSpace(strings.TrimRight(p.MoonrakerURL, "/"))
	if base == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return base + path
	}
	return base + "/" + path
}

func moonrakerAuthHeaders(p configpkg.Printer) map[string]string {
	headers := map[string]string{}
	if strings.TrimSpace(p.APIKey) != "" {
		headers["X-Api-Key"] = p.APIKey
	}
	return headers
}

// sameHost reports whether two URLs name the same host and port.
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(strings.TrimSpace(a))
	ub, err2 := url.Parse(strings.TrimSpace(b))
	return err1 == nil && err2 == nil && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

func applyMoonrakerAuth(req *http.Request, p configpkg.Printer) {
	for k, v := range moonrakerAuthHeaders(p) {
		req.Header.Set(k, v)
	}
}

func isBambuPrinter(p configpkg.Printer) bool {
	if strings.TrimSpace(p.Serial) == "" {
		return false
	}
	if strings.TrimSpace(p.LANCode) == "" {
		return false
	}
	return p.Kind() == configpkg.KindBambu
}

func nestedMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	cur := m
	for _, k := range keys {
		v, ok := cur[k]
		if !ok {
			return map[string]interface{}{}
		}
		next, ok := v.(map[string]interface{})
		if !ok {
			return map[string]interface{}{}
		}
		cur = next
	}
	return cur
}

func nestedMapAny(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	v, ok := m[key]
	if !ok {
		return map[string]interface{}{}
	}
	next, ok := v.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	return next
}

func anyFromMap(m map[string]interface{}, key string) interface{} {
	if m == nil {
		return nil
	}
	return m[key]
}

func stringAny(v interface{}) string {
	s, ok := v.(string)
	if ok {
		return s
	}
	return ""
}

func lowerString(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func floatAny(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}
