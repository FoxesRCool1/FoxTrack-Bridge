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
	PrinterError   Kind = "printer_error"   // a new alarm (HMS, print error, Klipper shutdown)
	PrinterOffline Kind = "printer_offline" // stopped reporting
	PrinterOnline  Kind = "printer_online"  // back after PrinterOffline
	MaintenanceDue Kind = "maintenance_due"
	Test           Kind = "test" // the dashboard's "Send test" button
)

// All lists every kind in the order the dashboard shows them.
var All = []Kind{PrintStarted, PrintPaused, PrintResumed, PrintFinished, PrintFailed, PrintCancelled, PrinterError, PrinterOffline, PrinterOnline, MaintenanceDue}

// Default is what a new notification channel sends.
var Default = []Kind{PrintFinished, PrintFailed, PrintPaused, PrinterError, MaintenanceDue}

// Urgent kinds get through quiet hours.
func (k Kind) Urgent() bool {
	return k == PrintFailed || k == PrinterError || k == Test
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
