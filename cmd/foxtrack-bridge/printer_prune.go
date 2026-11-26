package main

import (
	"log"
	"slices"

	"foxtrack-bridge/config"
)

// forgetPrinterIDs removes deleted printers from everything in cfg that names
// printers by ID: notification channels, the timelapse list, share links and
// maintenance tasks. It edits cfg in place, so the caller passes a copy it is
// about to store, never the live configStore: other goroutines read the live
// blocks after releasing configMutex.
//
// A printer list that becomes empty would mean "all printers", which is wider
// than what the user chose. So a channel whose last printer is deleted is
// turned off (its settings stay), and timelapse is turned off the same way.
func forgetPrinterIDs(cfg *config.Config, gone map[string]bool) {
	if cfg == nil || len(gone) == 0 {
		return
	}
	keep := func(ids []string) []string {
		return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return gone[id] })
	}
	if n := cfg.Notifications; n != nil {
		for i := range n.Channels {
			ch := &n.Channels[i]
			if len(ch.Printers) == 0 {
				continue
			}
			left := keep(ch.Printers)
			if len(left) == len(ch.Printers) {
				continue
			}
			if len(left) == 0 {
				// Keep the dead IDs: an empty list would send for every printer.
				if ch.Enabled {
					ch.Enabled = false
					log.Printf("[notify] channel %q turned off: its last printer was removed", ch.Name)
				}
				continue
			}
			ch.Printers = left
		}
	}
	if t := cfg.Timelapse; t != nil && len(t.Printers) > 0 {
		left := keep(t.Printers)
		switch {
		case len(left) == len(t.Printers):
		case len(left) == 0:
			if t.Enabled {
				t.Enabled = false
				log.Printf("[timelapse] turned off: its last printer was removed")
			}
		default:
			t.Printers = left
		}
	}
	if s := cfg.Security; s != nil {
		s.Shares = slices.DeleteFunc(s.Shares, func(l config.ShareLink) bool { return gone[l.PrinterID] })
	}
	if m := cfg.Maintenance; m != nil {
		m.Tasks = slices.DeleteFunc(m.Tasks, func(t config.MaintenanceTask) bool { return gone[t.PrinterID] })
	}
}

// queueForget drops the print queues of removed printers.
func queueForget(gone map[string]bool) {
	printQueue.Lock()
	defer printQueue.Unlock()
	queueLoadLocked()
	if !printQueue.loaded {
		return // unreadable queue file: left as it is
	}
	changed := false
	for id := range gone {
		if _, ok := printQueue.byPrinter[id]; ok {
			delete(printQueue.byPrinter, id)
			changed = true
		}
	}
	if changed {
		if err := queueSaveLocked(); err != nil {
			log.Printf("[queue] save after a printer was removed: %v", err)
		}
	}
}
