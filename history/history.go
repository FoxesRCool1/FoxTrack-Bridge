package history

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// TempSample holds a single temperature reading taken during a print.
type TempSample struct {
	T          int64   `json:"t"` // seconds elapsed since print start
	NozzleTemp float64 `json:"nozzle_temp"`
	BedTemp    float64 `json:"bed_temp"`
}

// Record captures a single completed print job.
type Record struct {
	PrinterName string       `json:"printer_name"`
	FileName    string       `json:"file_name"`
	NozzleTemp  float64      `json:"nozzle_temp"`
	BedTemp     float64      `json:"bed_temp"`
	StartTime   int64        `json:"start_time"` // Unix seconds
	EndTime     int64        `json:"end_time"`
	Duration    int64        `json:"duration"` // seconds
	Result      string       `json:"result"`   // "finished" | "cancelled" | "error"
	TempSamples []TempSample `json:"temp_samples,omitempty"`
	// Optional and additive: records written before v2.6 have none of these.
	PercentAtEnd int           `json:"percent_at_end,omitempty"`
	FilamentMM   float64       `json:"filament_mm,omitempty"` // Klipper and OctoPrint report length
	Filaments    []FilamentUse `json:"filaments,omitempty"`   // Bambu AMS and external spool
	EnergyWh     *float64      `json:"energy_wh,omitempty"`   // from the printer's smart plug
}

// FilamentUse is one spool's share of a print. Grams is estimated from the
// spool's remain percentage and weight, so it is 0 when either is unknown.
type FilamentUse struct {
	Slot     int     `json:"slot"`
	Material string  `json:"material,omitempty"`
	Color    string  `json:"color,omitempty"`
	TagUID   string  `json:"tag_uid,omitempty"`
	Grams    float64 `json:"grams,omitempty"`
}

// TotalGrams adds up the estimated grams over every spool.
func (r Record) TotalGrams() float64 {
	var g float64
	for _, f := range r.Filaments {
		g += f.Grams
	}
	return g
}

var (
	listenersMu sync.Mutex
	listeners   []func(Record)
)

// OnAppend registers fn to run, on its own goroutine, after each record is
// written. Notifications, Spoolman and maintenance hang off this.
func OnAppend(fn func(Record)) {
	listenersMu.Lock()
	listeners = append(listeners, fn)
	listenersMu.Unlock()
}

func notifyAppend(r Record) {
	listenersMu.Lock()
	fns := slices.Clone(listeners)
	listenersMu.Unlock()
	for _, fn := range fns {
		go fn(r)
	}
}

var mu sync.Mutex

func historyPath() string {
	return filepath.Join(config.ConfigDir(), "history.json")
}

// Load reads all history records from disk. Returns an empty slice if the file does not exist.
func Load() ([]Record, error) {
	data, err := os.ReadFile(historyPath())
	if os.IsNotExist(err) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func save(records []Record) error {
	p := historyPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(p, data, 0600)
}

// backupCorrupt moves a history file that failed to load aside as
// history.json.corrupt-<timestamp> and returns the backup location.
func backupCorrupt() (string, error) {
	p := historyPath()
	backup := p + ".corrupt-" + time.Now().Format("20060102-150405")
	if err := os.Rename(p, backup); err != nil {
		return "", err
	}
	return backup, nil
}

// Append adds a record to the history file in a thread-safe manner.
// Load returns no error for a missing file, so an error here means an existing
// file failed to load. Back it up rather than overwrite the user's history.
func Append(r Record) error {
	mu.Lock()
	defer mu.Unlock()
	records, err := Load()
	if err != nil {
		backup, backupErr := backupCorrupt()
		if backupErr != nil {
			return fmt.Errorf("history failed to load (%v) and could not be backed up: %w", err, backupErr)
		}
		log.Printf("[history] history failed to load (%v); backed up to %s, starting a new file", err, backup)
		records = []Record{}
	}
	records = append(records, r)
	if err := save(records); err != nil {
		return err
	}
	notifyAppend(r)
	return nil
}

// ForPrinter returns all records for the named printer.
func ForPrinter(name string) ([]Record, error) {
	all, err := Load()
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range all {
		if r.PrinterName == name {
			out = append(out, r)
		}
	}
	if out == nil {
		out = []Record{}
	}
	return out, nil
}
