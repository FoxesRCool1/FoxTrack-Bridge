package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"foxtrack-bridge/capture"
	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/history"
	"foxtrack-bridge/lan"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// The watcher turns status changes into events. It reads the same live state
// the dashboard shows, so it needs no hook inside the Bambu or LAN code: a
// start, pause, resume, new alarm or lost link is a difference between two
// polls, and a print's end comes from the history record (its exact result,
// duration and filament).

const (
	watchEvery      = 3 * time.Second
	offlineAfter    = 2 * time.Minute  // a short drop is not worth a message
	alarmQuietAgain = 30 * time.Minute // the same alarm code is not resent before this
)

type watchState struct {
	status    string
	inPrint   bool
	file      string
	alarms    map[string]bool
	lostSince time.Time
	offline   bool // PrinterOffline was sent
}

var (
	watchMu     sync.Mutex
	watched     = map[string]*watchState{} // by printer name
	alarmSentAt = map[string]time.Time{}   // "<name>|<code>"
)

func startEventWatcher() {
	history.OnAppend(publishPrintEnd)
	go func() {
		for range time.Tick(watchEvery) {
			watchOnce(time.Now())
		}
	}()
}

func watchOnce(now time.Time) {
	configMutex.RLock()
	printers := append([]config.Printer(nil), configStore.Printers...)
	configMutex.RUnlock()
	states := liveStates()

	watchMu.Lock()
	defer watchMu.Unlock()
	for k, t := range alarmSentAt {
		if now.Sub(t) >= alarmQuietAgain {
			delete(alarmSentAt, k)
		}
	}
	seen := map[string]bool{}
	for _, p := range printers {
		seen[p.Name] = true
		s := states[p.Name]
		if s == nil {
			continue
		}
		for _, e := range diffState(p, s, now) {
			events.Publish(e)
		}
	}
	for name := range watched {
		if !seen[name] {
			delete(watched, name)
		}
	}
}

// diffState updates the printer's watch state and returns what changed. The
// first poll of a printer only records where it is, so a restart mid-print
// sends nothing. The caller holds watchMu.
func diffState(p config.Printer, s *mqttpkg.TelemetryData, now time.Time) []events.Event {
	w, ok := watched[p.Name]
	base := events.Event{Time: now, PrinterID: p.ID, Printer: p.Name, Model: s.PrinterModel, File: s.FileName, Progress: s.Progress, Remaining: s.TimeRemaining}
	if !ok {
		// Seed only from a real report: a printer still connecting (or
		// offline at start) would otherwise look idle, and its first real
		// "printing" would read as a new print.
		if s.Status == "connected" || s.Status == "disconnected" || s.Status == "" {
			return nil
		}
		w = &watchState{status: s.Status, inPrint: mqttpkg.InPrint(s.Status), file: s.FileName, alarms: alarmCodes(s.Alarms)}
		watched[p.Name] = w
		return nil
	}
	var out []events.Event
	emit := func(k events.Kind, mod func(*events.Event)) {
		e := base
		e.Kind = k
		if mod != nil {
			mod(&e)
		}
		out = append(out, e)
	}

	// Link state. "connected" is the moment before the first report.
	if s.Status == "disconnected" {
		if w.lostSince.IsZero() {
			w.lostSince = now
		}
		if !w.offline && now.Sub(w.lostSince) >= offlineAfter {
			w.offline = true
			emit(events.PrinterOffline, func(e *events.Event) {
				e.File, e.Progress = w.file, -1
				if w.inPrint {
					e.Message = "It stopped reporting during a print."
				}
			})
		}
		return out
	}
	if s.Status == "connected" {
		return out
	}
	w.lostSince = time.Time{}
	if w.offline {
		w.offline = false
		emit(events.PrinterOnline, nil)
	}

	prev := w.status
	w.status = s.Status
	if s.FileName != "" {
		w.file = s.FileName
	}
	switch {
	case s.Status == "printing" && !w.inPrint:
		w.inPrint = true
		emit(events.PrintStarted, nil)
	case s.Status == "paused" && prev == "printing":
		emit(events.PrintPaused, func(e *events.Event) { e.Message = firstAlarmText(s.Alarms) })
	case s.Status == "printing" && prev == "paused":
		emit(events.PrintResumed, nil)
	case mqttpkg.Settled(s.Status):
		w.inPrint = false // the end itself is sent from the history record
	}

	// New alarms. Info-level ones are status chatter, not problems.
	now2 := alarmCodes(s.Alarms)
	for _, a := range s.Alarms {
		key := alarmKey(a)
		if w.alarms[key] || a.Severity == webhook.AlarmInfo {
			continue
		}
		sentKey := p.Name + "|" + key
		if t, ok := alarmSentAt[sentKey]; ok && now.Sub(t) < alarmQuietAgain {
			continue
		}
		alarmSentAt[sentKey] = now
		emit(events.PrinterError, func(e *events.Event) {
			e.Severity = a.Severity
			if a.Code != nil {
				e.Code = *a.Code
			}
			if a.Message != nil {
				e.Message = *a.Message
			}
		})
	}
	w.alarms = now2
	return out
}

func alarmKey(a webhook.RelayAlarm) string {
	if a.Code != nil {
		return a.Source + ":" + *a.Code
	}
	if a.Message != nil {
		return a.Source + ":" + *a.Message
	}
	return a.Source
}

func alarmCodes(as []webhook.RelayAlarm) map[string]bool {
	out := make(map[string]bool, len(as))
	for _, a := range as {
		out[alarmKey(a)] = true
	}
	return out
}

func firstAlarmText(as []webhook.RelayAlarm) string {
	for _, a := range as {
		if a.Message != nil && *a.Message != "" {
			return *a.Message
		}
	}
	return ""
}

// publishPrintEnd sends the end of a print with its history record.
func publishPrintEnd(r history.Record) {
	k := events.PrintCancelled
	switch r.Result {
	case "finished":
		k = events.PrintFinished
	case "error":
		k = events.PrintFailed
	}
	p, _ := printerByName(r.PrinterName)
	model := ""
	if s, ok := liveStates()[r.PrinterName]; ok && s != nil {
		model = s.PrinterModel
	}
	rec := r
	events.Publish(events.Event{
		Kind: k, PrinterID: p.ID, Printer: r.PrinterName, Model: model, File: r.FileName,
		Progress: r.PercentAtEnd, Record: &rec,
	})
}

// printerByName returns a copy of the named printer's config.
func printerByName(name string) (config.Printer, bool) {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, p := range configStore.Printers {
		if p.Name == name {
			return p, true
		}
	}
	return config.Printer{}, false
}

// printerByID returns a copy of the printer's config.
func printerByID(id string) (config.Printer, bool) {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, p := range configStore.Printers {
		if p.ID == id {
			return p, true
		}
	}
	return config.Printer{}, false
}

// errNoCamera is returned by printerSnapshot for a printer without one.
var errNoCamera = errors.New("this printer has no camera the Bridge can read")

// printerSnapshot reads one JPEG from any printer's camera: the Bambu
// port-6000 stream (A1, P1), the RTSP stream (X1, H2, P2S; needs ffmpeg), or a
// Klipper, OctoPrint or custom webcam.
func printerSnapshot(ctx context.Context, p config.Printer) ([]byte, error) {
	if p.CameraHidden {
		return nil, errNoCamera
	}
	if !isBambuPrinterConfig(p) || p.WebcamURL != "" {
		return lan.Snapshot(p)
	}
	ip := p.IP
	if p.IsCloud() && ip == "" {
		ip = mqttpkg.CloudIP(p.Serial)
	}
	if ip == "" || p.LANCode == "" {
		return nil, errNoCamera
	}
	if useRTSP(p) {
		return rtspSnapshot(ctx, p, ip)
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := capture.BambuFrame(ip, p.LANCode, p.Name)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// mutateConfig changes the stored config under the lock and writes it. fn
// may refuse by returning an error, and then nothing is saved.
func mutateConfig(fn func(c *config.Config) error) error {
	configMutex.Lock()
	if err := fn(configStore); err != nil {
		configMutex.Unlock()
		return err
	}
	cfg := configStore.Clone()
	configMutex.Unlock()
	return config.SaveConfig(cfg)
}
