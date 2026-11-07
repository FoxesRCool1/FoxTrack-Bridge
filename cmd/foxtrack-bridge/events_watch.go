package main

import (
	"context"
	"errors"
	"sort"
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
	alarmQuietAgain = 30 * time.Minute // at most one "cleared" per alarm in this time
	// alarmClearAfter: an alarm must stay gone this long before "cleared" is
	// sent, so a sensor that flickers, or a report that briefly lacks the
	// list (a reconnect), does not read as a fix.
	alarmClearAfter = 2 * time.Minute
	// offlineAgainAfter: a printer that drops again soon after coming back
	// is reported only once it has been gone this long, so a flaky Wi-Fi
	// link does not send an offline/online pair every few minutes.
	offlineAgainAfter = 15 * time.Minute
	offlineFlapWindow = 30 * time.Minute
	maxAlarmsPerEvent = 20
)

type watchState struct {
	status    string
	inPrint   bool
	file      string
	alarms    map[string]bool // every alarm in the last report
	told      map[string]*alarmNote
	lostSince time.Time
	offline   bool      // PrinterOffline was sent
	backAt    time.Time // when the last PrinterOnline was sent
	// almostDoneSent: the print_almost_done for this print went out. Reset
	// when a new print starts and when the print ends, so each print gets
	// at most one.
	almostDoneSent bool
}

// alarmNote is an alarm the user was told about. open: no "cleared" sent yet.
type alarmNote struct {
	alarm     events.Alarm
	firstSeen time.Time
	goneSince time.Time // zero while it is in the report
	clearedAt time.Time // last "cleared" sent
	open      bool
}

var (
	watchMu sync.Mutex
	watched = map[string]*watchState{} // by printer name
)

func startEventWatcher() {
	history.OnAppend(publishPrintEnd)
	go func() {
		for range time.Tick(watchEvery) {
			watchOnce(time.Now())
		}
	}()
}

// almostDoneMinutes reads the almost-done threshold from the notifications
// settings: 0 (or no settings) is the default of 10, and the value is clamped
// to 1..120.
func almostDoneMinutes() int {
	configMutex.RLock()
	var n *config.Notifications
	if configStore != nil {
		n = configStore.Notifications
	}
	configMutex.RUnlock()
	m := 10
	if n != nil {
		m = n.AlmostDoneMinutes
	}
	if m <= 0 {
		m = 10
	}
	if m > 120 {
		m = 120
	}
	return m
}

func watchOnce(now time.Time) {
	configMutex.RLock()
	printers := append([]config.Printer(nil), configStore.Printers...)
	configMutex.RUnlock()
	states := liveStates()

	watchMu.Lock()
	defer watchMu.Unlock()
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
		w = &watchState{status: s.Status, inPrint: mqttpkg.InPrint(s.Status), file: s.FileName, alarms: alarmCodes(s.Alarms), told: map[string]*alarmNote{}}
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

	// Link state. "connected" is the moment before the first report. With no
	// report there is no alarm list either, so an alarm's time away restarts.
	if s.Status == "disconnected" || s.Status == "connected" {
		for _, n := range w.told {
			n.goneSince = time.Time{}
		}
	}
	if s.Status == "disconnected" {
		if w.lostSince.IsZero() {
			w.lostSince = now
		}
		wait := offlineAfter
		if !w.backAt.IsZero() && now.Sub(w.backAt) < offlineFlapWindow {
			wait = offlineAgainAfter
		}
		if !w.offline && now.Sub(w.lostSince) >= wait {
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
		w.backAt = now
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
		w.almostDoneSent = false
		emit(events.PrintStarted, nil)
	case s.Status == "paused" && prev == "printing":
		emit(events.PrintPaused, func(e *events.Event) { e.Message = firstAlarmText(s.Alarms) })
	case s.Status == "printing" && prev == "paused":
		emit(events.PrintResumed, nil)
	case mqttpkg.Settled(s.Status):
		w.inPrint = false // the end itself is sent from the history record
		w.almostDoneSent = false
	}

	// Almost done: once per print, while it is running (not paused), when the
	// estimate says the end is near. The progress guard stops a Klipper
	// estimate from firing it early in a print.
	if w.inPrint && !w.almostDoneSent && s.Status == "printing" &&
		s.TimeRemaining > 0 && s.TimeRemaining <= almostDoneMinutes() && s.Progress >= 50 {
		w.almostDoneSent = true
		emit(events.PrintAlmostDone, nil)
	}

	// Alarms: everything new in this report goes out as one message, and so
	// does everything that has cleared. Info-level ones are status chatter.
	if news := w.newAlarms(s.Alarms, now); len(news) > 0 {
		emit(events.PrinterError, func(e *events.Event) { setAlarms(e, news) })
	}
	if gone := w.clearedAlarms(s.Alarms, now); len(gone) > 0 {
		emit(events.PrinterAlarmCleared, func(e *events.Event) { setAlarms(e, gone) })
	}
	w.alarms = alarmCodes(s.Alarms)
	return out
}

// newAlarms returns the alarms the user has not been told about: new in this
// report, not info, and not an open one coming back after a flicker. An alarm
// that was reported cleared and comes back is news again.
func (w *watchState) newAlarms(as []webhook.RelayAlarm, now time.Time) []events.Alarm {
	var out []events.Alarm
	for _, a := range as {
		key := alarmKey(a)
		if n := w.told[key]; n != nil && n.open {
			n.goneSince = time.Time{}
			continue
		}
		if w.alarms[key] || a.Severity == webhook.AlarmInfo {
			continue // in the last report too (seeded at start), or info
		}
		ea := events.Alarm{Severity: a.Severity}
		if a.Code != nil {
			ea.Code = *a.Code
		}
		if a.Message != nil {
			ea.Message = *a.Message
		}
		n := w.told[key]
		if n == nil {
			n = &alarmNote{}
			w.told[key] = n
		}
		n.alarm, n.firstSeen, n.goneSince, n.open = ea, now, time.Time{}, true
		out = append(out, ea)
	}
	return out
}

// clearedAlarms returns the told-about alarms that have stayed gone for
// alarmClearAfter, at most one "cleared" per alarm per alarmQuietAgain (a
// flapping one then stays open, and quiet, until that time is up).
func (w *watchState) clearedAlarms(as []webhook.RelayAlarm, now time.Time) []events.Alarm {
	cur := alarmCodes(as)
	var out []events.Alarm
	for key, n := range w.told {
		if !n.open {
			if now.Sub(n.clearedAt) >= alarmQuietAgain {
				delete(w.told, key)
			}
			continue
		}
		if cur[key] {
			continue
		}
		if n.goneSince.IsZero() {
			n.goneSince = now
		}
		if now.Sub(n.goneSince) < alarmClearAfter || (!n.clearedAt.IsZero() && now.Sub(n.clearedAt) < alarmQuietAgain) {
			continue
		}
		n.open, n.clearedAt = false, now
		a := n.alarm
		a.Lasted = n.goneSince.Sub(n.firstSeen)
		out = append(out, a)
	}
	return out
}

var severityRank = map[string]int{webhook.AlarmFatal: 0, webhook.AlarmSerious: 1, webhook.AlarmCommon: 2}

// setAlarms puts the alarms on the event, worst first (then by code, so the
// order is stable), at most maxAlarmsPerEvent; the first also fills Code,
// Message and Severity.
func setAlarms(e *events.Event, as []events.Alarm) {
	sort.SliceStable(as, func(i, j int) bool {
		ri, rj := severityRank[as[i].Severity], severityRank[as[j].Severity]
		if ri != rj {
			return ri < rj
		}
		return as[i].Code < as[j].Code
	})
	if len(as) > maxAlarmsPerEvent {
		as = as[:maxAlarmsPerEvent]
	}
	e.Alarms = as
	e.Code, e.Message, e.Severity = as[0].Code, as[0].Message, as[0].Severity
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
