// Package maintenance tracks printer maintenance tasks against print hours
// and announces each task once when it comes due.
package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

// Preset is a suggested maintenance task with its interval in hours.
type Preset struct {
	Name          string  `json:"name"`
	IntervalHours float64 `json:"interval_hours"`
}

// Presets returns the suggested tasks for a printer kind. A new slice is
// returned each call.
func Presets(kind string) []Preset {
	if kind == config.KindBambu {
		return []Preset{
			{Name: "Clean and lubricate the rods", IntervalHours: 100},
			{Name: "Lubricate the lead screws", IntervalHours: 200},
			{Name: "Clean the nozzle and hotend", IntervalHours: 200},
			{Name: "Check belt tension", IntervalHours: 300},
			{Name: "Clean the build plate", IntervalHours: 20},
			{Name: "Replace the carbon or HEPA filter", IntervalHours: 500},
		}
	}
	return []Preset{
		{Name: "Lubricate the rails", IntervalHours: 100},
		{Name: "Check the belts", IntervalHours: 200},
		{Name: "Check the nozzle", IntervalHours: 300},
		{Name: "Tighten the frame and eccentric nuts", IntervalHours: 300},
	}
}

// Task status.
const (
	StatusOK   = "ok"
	StatusSoon = "soon"
	StatusDue  = "due"
)

// StatusFor reports the status and percent of the interval reached.
func StatusFor(hours, interval float64) (string, int) {
	if interval <= 0 {
		return StatusOK, 0
	}
	percent := int(math.Floor(hours/interval*100 + 1e-9))
	switch {
	case hours >= interval:
		return StatusDue, percent
	case hours >= interval*0.9:
		return StatusSoon, percent
	default:
		return StatusOK, percent
	}
}

// Hours sums the print hours in recs for the named printer, counting only
// records that ended after since.
func Hours(recs []history.Record, names []string, since int64) float64 {
	var sec int64
	for _, r := range recs {
		if r.EndTime <= since {
			continue
		}
		for _, n := range names {
			if r.PrinterName == n {
				sec += r.Duration
				break
			}
		}
	}
	return float64(sec) / 3600
}

// View is one maintenance task with its current hours and status.
type View struct {
	config.MaintenanceTask
	PrinterName string  `json:"printer_name"`
	Hours       float64 `json:"hours"`
	Percent     int     `json:"percent"`
	Status      string  `json:"status"`
}

// Evaluate builds a view for each task whose printer still exists, in task
// order.
func Evaluate(tasks []config.MaintenanceTask, printers []config.Printer, recs []history.Record) []View {
	views := []View{}
	for _, task := range tasks {
		var printer *config.Printer
		for i := range printers {
			if printers[i].ID == task.PrinterID {
				printer = &printers[i]
				break
			}
		}
		if printer == nil {
			continue
		}
		names := append([]string{printer.Name}, printer.PreviousNames...)
		hours := Hours(recs, names, task.LastDoneAt)
		status, percent := StatusFor(hours, task.IntervalHours)
		views = append(views, View{
			MaintenanceTask: task,
			PrinterName:     printer.Name,
			Hours:           math.Round(hours*10) / 10,
			Percent:         percent,
			Status:          status,
		})
	}
	return views
}

// fmtHours formats hours to one decimal, dropping a trailing ".0".
func fmtHours(h float64) string {
	return strconv.FormatFloat(math.Round(h*10)/10, 'f', -1, 64)
}

// Message is a human-readable summary of the view.
func (v View) Message() string {
	return fmt.Sprintf("%s: %s h since it was last done (every %s h)", v.Name, fmtHours(v.Hours), fmtHours(v.IntervalHours))
}

// Announcer remembers which tasks have already been announced, so each due
// task is announced once.
type Announcer struct {
	mu        sync.Mutex
	path      string
	announced map[string]int64
}

// NewAnnouncer loads the state file at path, starting empty if it is missing
// or unreadable.
func NewAnnouncer(path string) *Announcer {
	a := &Announcer{path: path, announced: map[string]int64{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return a
	}
	var st struct {
		Announced map[string]int64 `json:"announced"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return a
	}
	if st.Announced != nil {
		a.announced = st.Announced
	}
	return a
}

// NewlyDue returns the views that are due and not yet announced, recording
// them. Tasks that are no longer due are forgotten.
func (a *Announcer) NewlyDue(views []View) []View {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []View{}
	changed := false
	for _, v := range views {
		if v.Status != StatusDue {
			if _, ok := a.announced[v.ID]; ok {
				delete(a.announced, v.ID)
				changed = true
			}
			continue
		}
		if prev, ok := a.announced[v.ID]; ok && prev == v.LastDoneAt {
			continue
		}
		a.announced[v.ID] = v.LastDoneAt
		out = append(out, v)
		changed = true
	}
	if changed {
		a.save()
	}
	return out
}

// save writes the state file, logging an error if the write fails.
func (a *Announcer) save() {
	data, err := json.Marshal(map[string]map[string]int64{"announced": a.announced})
	if err != nil {
		log.Printf("[maintenance] announcer save: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0755); err != nil {
		log.Printf("[maintenance] announcer save: %v", err)
		return
	}
	if err := config.WriteFileAtomic(a.path, data, 0600); err != nil {
		log.Printf("[maintenance] announcer save: %v", err)
	}
}
