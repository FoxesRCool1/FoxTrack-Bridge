package history

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// TempSample holds a single temperature reading taken during a print.
type TempSample struct {
	T          int64   `json:"t"`           // seconds elapsed since print start
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
	return save(records)
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
