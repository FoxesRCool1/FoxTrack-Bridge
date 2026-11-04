package main

import (
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/history"
	"foxtrack-bridge/maintenance"
)

// Maintenance reminders. Tasks live in config.Maintenance and change only
// through these routes; the hours come from the print history.

func init() {
	http.HandleFunc("/api/maintenance", handleMaintenance)      // GET: tasks per printer; POST: add, edit or add the suggested set
	http.HandleFunc("/api/maintenance/", handleMaintenanceTask) // POST /{id}/done, DELETE /{id}
	onStartup(func() {
		maintAnnouncer = maintenance.NewAnnouncer(filepath.Join(config.ConfigDir(), "maintenance_state.json"))
		history.OnAppend(checkMaintenance)
	})
}

// maintAnnouncer remembers which due tasks were announced. Nil until startup.
var maintAnnouncer *maintenance.Announcer

var maintIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const maxMaintenanceTasks = 500

// dueMaintenance returns one event for each task that has just become due.
func dueMaintenance() []events.Event {
	if maintAnnouncer == nil {
		return nil
	}
	printers, tasks := maintenanceSnapshot()
	if len(tasks) == 0 {
		return nil
	}
	recs, err := history.Load()
	if err != nil {
		log.Printf("[maintenance] print history not readable: %v", err)
		return nil
	}
	var out []events.Event
	for _, v := range maintAnnouncer.NewlyDue(maintenance.Evaluate(tasks, printers, recs)) {
		out = append(out, events.Event{Kind: events.MaintenanceDue, PrinterID: v.PrinterID, Printer: v.PrinterName, Message: v.Message(), Progress: -1})
	}
	return out
}

// checkMaintenance runs after every ended print.
func checkMaintenance(history.Record) {
	for _, e := range dueMaintenance() {
		events.Publish(e)
	}
}

func maintenanceSnapshot() ([]config.Printer, []config.MaintenanceTask) {
	configMutex.RLock()
	defer configMutex.RUnlock()
	printers := append([]config.Printer(nil), configStore.Printers...)
	var tasks []config.MaintenanceTask
	if configStore.Maintenance != nil {
		tasks = append(tasks, configStore.Maintenance.Tasks...)
	}
	return printers, tasks
}

type maintenancePrinter struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Kind  string             `json:"kind"`
	Tasks []maintenance.View `json:"tasks"`
	// Presets are the suggested tasks this printer does not have yet.
	Presets []maintenance.Preset `json:"presets"`
}

func handleMaintenance(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		printers, tasks := maintenanceSnapshot()
		recs, err := history.Load()
		if err != nil {
			http.Error(w, "The print history could not be read.", http.StatusInternalServerError)
			return
		}
		views := maintenance.Evaluate(tasks, printers, recs)
		out := make([]maintenancePrinter, 0, len(printers))
		for _, p := range printers {
			mp := maintenancePrinter{ID: p.ID, Name: p.Name, Kind: p.Kind(), Tasks: []maintenance.View{}, Presets: []maintenance.Preset{}}
			have := map[string]bool{}
			for _, v := range views {
				if v.PrinterID == p.ID {
					mp.Tasks = append(mp.Tasks, v)
					have[strings.ToLower(v.Name)] = true
				}
			}
			for _, ps := range maintenance.Presets(p.Kind()) {
				if !have[strings.ToLower(ps.Name)] {
					mp.Presets = append(mp.Presets, ps)
				}
			}
			out = append(out, mp)
		}
		json.NewEncoder(w).Encode(map[string]any{"printers": out})
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		postMaintenance(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// postMaintenance adds, edits or adds the suggested set. A body whose id is
// not stored yet is added under that id, which is how the dashboard undoes a
// delete with the task it got back.
func postMaintenance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID            string  `json:"id"`
		PrinterID     string  `json:"printer_id"`
		Name          string  `json:"name"`
		IntervalHours float64 `json:"interval_hours"`
		LastDoneAt    *int64  `json:"last_done_at"`
		Note          string  `json:"note"`
		Presets       bool    `json:"presets"` // add every suggested task the printer lacks
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "The request could not be read.", http.StatusBadRequest)
		return
	}
	bad := func(msg string) { http.Error(w, msg, http.StatusBadRequest) }
	printer, ok := printerByID(req.PrinterID)
	if !ok {
		bad("Choose a printer from the list.")
		return
	}
	now := time.Now().Unix()
	var add []config.MaintenanceTask
	if req.Presets {
		for _, ps := range maintenance.Presets(printer.Kind()) {
			add = append(add, config.MaintenanceTask{ID: newTaskID(), PrinterID: printer.ID, Name: ps.Name, IntervalHours: ps.IntervalHours, LastDoneAt: now})
		}
	} else {
		name := strings.TrimSpace(req.Name)
		note := strings.TrimSpace(req.Note)
		switch {
		case name == "" || utf8.RuneCountInString(name) > 80:
			bad("Give the task a name of up to 80 characters.")
			return
		case math.IsNaN(req.IntervalHours) || req.IntervalHours <= 0 || req.IntervalHours > 100000:
			bad("Enter the printing hours between jobs, more than 0.")
			return
		case utf8.RuneCountInString(note) > 300:
			bad("A note can be at most 300 characters.")
			return
		case req.ID != "" && !maintIDPattern.MatchString(req.ID):
			bad("That task id is not valid.")
			return
		}
		id := req.ID
		if id == "" {
			id = newTaskID()
		}
		add = []config.MaintenanceTask{{ID: id, PrinterID: printer.ID, Name: name, IntervalHours: req.IntervalHours, LastDoneAt: now, Note: note}}
		if req.LastDoneAt != nil && *req.LastDoneAt >= 0 {
			add[0].LastDoneAt = *req.LastDoneAt
		}
	}

	var saved []config.MaintenanceTask
	var refused string
	err := mutateConfig(func(c *config.Config) error {
		if c.Maintenance == nil {
			c.Maintenance = &config.Maintenance{}
		}
		tasks := c.Maintenance.Tasks
		for _, t := range add {
			if req.Presets && hasTaskNamed(tasks, printer.ID, t.Name) {
				continue
			}
			if i := taskIndex(tasks, t.ID); i >= 0 {
				if req.LastDoneAt == nil {
					t.LastDoneAt = tasks[i].LastDoneAt // an edit keeps the clock
				}
				tasks[i] = t
			} else {
				if len(tasks) >= maxMaintenanceTasks {
					refused = "There are too many maintenance tasks. Delete some first."
					return errors.New(refused)
				}
				tasks = append(tasks, t)
			}
			saved = append(saved, t)
		}
		c.Maintenance.Tasks = tasks
		return nil
	})
	if refused != "" {
		bad(refused)
		return
	}
	if err != nil {
		writeSaveFailed(w, err)
		return
	}
	printers, _ := maintenanceSnapshot()
	views := maintenance.Evaluate(saved, printers, mustHistory())
	if views == nil {
		views = []maintenance.View{}
	}
	json.NewEncoder(w).Encode(map[string]any{"tasks": views})
}

func newTaskID() string { return config.NewPrinterID()[:16] }

func hasTaskNamed(tasks []config.MaintenanceTask, printerID, name string) bool {
	for _, t := range tasks {
		if t.PrinterID == printerID && strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

func taskIndex(tasks []config.MaintenanceTask, id string) int {
	for i, t := range tasks {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// mustHistory is the history, or none when it cannot be read: a view of a
// task just saved is not worth failing the request for.
func mustHistory() []history.Record {
	recs, err := history.Load()
	if err != nil {
		return nil
	}
	return recs
}

// handleMaintenanceTask serves /api/maintenance/{id}[/done].
func handleMaintenanceTask(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/maintenance/"), "/"), "/")
	id := parts[0]
	if id == "" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "done") {
		http.NotFound(w, r)
		return
	}
	gone := "That task no longer exists."
	switch {
	case len(parts) == 2 && r.Method == "POST":
		var done config.MaintenanceTask
		found := false
		err := mutateConfig(func(c *config.Config) error {
			if c.Maintenance == nil {
				return nil
			}
			if i := taskIndex(c.Maintenance.Tasks, id); i >= 0 {
				c.Maintenance.Tasks[i].LastDoneAt = time.Now().Unix()
				done, found = c.Maintenance.Tasks[i], true
			}
			return nil
		})
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		if !found {
			http.Error(w, gone, http.StatusNotFound)
			return
		}
		printers, _ := maintenanceSnapshot()
		views := maintenance.Evaluate([]config.MaintenanceTask{done}, printers, nil) // nothing has printed since now
		if len(views) == 0 {
			http.Error(w, gone, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(views[0])
	case len(parts) == 1 && r.Method == "DELETE":
		var removed config.MaintenanceTask
		found := false
		err := mutateConfig(func(c *config.Config) error {
			if c.Maintenance == nil {
				return nil
			}
			if i := taskIndex(c.Maintenance.Tasks, id); i >= 0 {
				removed, found = c.Maintenance.Tasks[i], true
				c.Maintenance.Tasks = append(c.Maintenance.Tasks[:i:i], c.Maintenance.Tasks[i+1:]...)
			}
			return nil
		})
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		if !found {
			http.Error(w, gone, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(removed)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
