// Package events carries printer happenings (a print started, ended, paused,
// an error appeared) from the Bridge's watcher to the features that act on
// them: notifications, MQTT export, maintenance. It knows nothing about
// either side.
package events

import (
	"log"
	"sync"
	"time"

	"foxtrack-bridge/history"
)

type Kind string

const (
	PrintStarted   Kind = "print_started"
	PrintPaused    Kind = "print_paused"
	PrintResumed   Kind = "print_resumed"
	PrintFinished  Kind = "print_finished"
	PrintFailed    Kind = "print_failed"
	PrintCancelled Kind = "print_cancelled"
	PrinterError   Kind = "printer_error" // new alarms (HMS, print error, Klipper shutdown), one event per poll
	// PrinterAlarmCleared follows a PrinterError: the alarms it told about are
	// gone. A channel gets it when it gets PrinterError, so it has no switch
	// of its own and is not in All.
	PrinterAlarmCleared Kind = "printer_alarm_cleared"
	PrinterOffline      Kind = "printer_offline" // stopped reporting
	PrinterOnline       Kind = "printer_online"  // back after PrinterOffline
	MaintenanceDue      Kind = "maintenance_due"
	// PrintAlmostDone is opt-in per channel: it is not in Default, and
	// fires once per print when the remaining time drops to the configured
	// minutes (default 10).
	PrintAlmostDone Kind = "print_almost_done"
	// PrintFailureSuspected comes from the assistant's camera watch (off
	// unless the user turns it on in Settings, Assistant): the picture looked
	// like a failed print twice in a row. It never pauses the print.
	PrintFailureSuspected Kind = "print_failure_suspected"
	Test                  Kind = "test" // the dashboard's "Send test" button
)

// All lists every kind in the order the dashboard shows them.
var All = []Kind{PrintStarted, PrintAlmostDone, PrintPaused, PrintResumed, PrintFinished, PrintFailed, PrintCancelled, PrintFailureSuspected, PrinterError, PrinterOffline, PrinterOnline, MaintenanceDue}

// Default is what a new notification channel sends.
// PrintFailureSuspected is in it because it only ever fires once the user has
// turned the camera watch on.
var Default = []Kind{PrintFinished, PrintFailed, PrintPaused, PrintFailureSuspected, PrinterError, MaintenanceDue}

// Urgent kinds get through quiet hours. A cleared alarm goes too (quietly,
// see Silent): whoever was woken by the alarm wants to know it is over.
func (k Kind) Urgent() bool {
	return k == PrintFailed || k == PrinterError || k == PrinterAlarmCleared || k == Test
}

// Loud kinds go out at high priority where a service has one.
func (k Kind) Loud() bool {
	return k == PrintFailed || k == PrinterError || k == PrintFailureSuspected
}

// Silent kinds are good news nobody needs to be buzzed for: they arrive
// without a sound where a service allows it.
func (k Kind) Silent() bool {
	return k == PrintStarted || k == PrintResumed || k == PrinterOnline || k == PrinterAlarmCleared
}

// Alarm is one printer alarm in a PrinterError or PrinterAlarmCleared event.
type Alarm struct {
	Code     string
	Message  string
	Severity string        // fatal, serious, common
	Lasted   time.Duration // PrinterAlarmCleared only: how long it was there
}

type Event struct {
	Kind      Kind
	Time      time.Time
	PrinterID string
	Printer   string // display name
	Model     string // e.g. "P1S", "Klipper"; may be empty
	File      string
	Progress  int // percent, -1 when unknown
	Remaining int // minutes left, 0 when unknown
	// Message is plain words: the alarm text, or what a maintenance task is.
	Message  string
	Code     string // alarm code, e.g. "0700_2000_0002_0001"
	Severity string // alarm severity: fatal, serious, common, info
	// Alarms are every alarm of a PrinterError or PrinterAlarmCleared event,
	// worst first. Code, Message and Severity repeat the first one.
	Alarms []Alarm
	// Record is the finished print, for the three end kinds.
	Record *history.Record
}

var (
	mu   sync.Mutex
	subs []func(Event)
)

// Subscribe registers fn for every event. fn runs on its own goroutine.
func Subscribe(fn func(Event)) {
	mu.Lock()
	subs = append(subs, fn)
	mu.Unlock()
}

// Publish hands e to every subscriber without waiting for them.
func Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	mu.Lock()
	fns := make([]func(Event), len(subs))
	copy(fns, subs)
	mu.Unlock()
	for _, fn := range fns {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[events] a %s handler panicked: %v", e.Kind, r)
				}
			}()
			fn(e)
		}()
	}
}
