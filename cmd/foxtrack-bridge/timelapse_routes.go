package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/timelapse"
)

// Timelapse. A picture of each printing printer's camera every few seconds,
// built into a video when the print ends. The package timelapse does the
// storing and building; this file says when, from the settings, the events
// and the live telemetry, and serves the dashboard.

const (
	timelapseSnapTimeout = 20 * time.Second
	// A print that ends without an end event (a history record that was never
	// written) is closed after the printer has been idle this long.
	timelapseIdleEnd = 90 * time.Second
)

var (
	tlMu      sync.Mutex
	tlRec     *timelapse.Recorder
	tlIdle    = map[string]time.Time{} // printer ID -> since when it is recording but not printing; tick goroutine only
	tlFailing = map[string]int{}       // printer ID -> consecutive failed pictures; guarded by tlMu
)

func init() {
	http.HandleFunc("/api/timelapse", handleTimelapse)
	http.HandleFunc("/api/timelapse/file/", handleTimelapseFile)
	onStartup(startTimelapse)
}

func timelapseDir() string { return filepath.Join(config.ConfigDir(), "timelapse") }

// timelapseRecorder returns the recorder for the current config directory.
func timelapseRecorder() *timelapse.Recorder {
	tlMu.Lock()
	defer tlMu.Unlock()
	if dir := timelapseDir(); tlRec == nil || tlRec.Dir() != dir {
		tlRec = timelapse.New(dir, ffmpegPath)
	}
	return tlRec
}

// timelapseSettings returns a copy of the stored settings.
func timelapseSettings() config.Timelapse {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil || configStore.Timelapse == nil {
		return config.Timelapse{}
	}
	t := *configStore.Timelapse
	t.Printers = slices.Clone(t.Printers)
	return t
}

// timelapseWants reports whether the settings include the printer.
func timelapseWants(t config.Timelapse, p config.Printer) bool {
	if p.CameraHidden {
		return false
	}
	return len(t.Printers) == 0 || slices.Contains(t.Printers, p.ID)
}

func startTimelapse() {
	events.Subscribe(timelapseEvent)
	go func() {
		rec := timelapseRecorder()
		rec.Recover()
		rec.Cleanup(timelapse.KeepDays(timelapseSettings().KeepDays))
		tick := time.NewTicker(time.Second)
		daily := time.NewTicker(24 * time.Hour)
		for {
			select {
			case <-tick.C:
				timelapseTick(time.Now())
			case <-daily.C:
				rec.Recover()
				rec.Cleanup(timelapse.KeepDays(timelapseSettings().KeepDays))
			}
		}
	}()
}

// timelapseEvent starts a recording when a print starts and ends it with the
// print. Ends are handled even when the feature was switched off meanwhile.
func timelapseEvent(e events.Event) {
	rec := timelapseRecorder()
	switch e.Kind {
	case events.PrintStarted:
		t := timelapseSettings()
		if p, ok := printerByID(e.PrinterID); ok && t.Enabled && timelapseWants(t, p) {
			rec.Start(timelapse.Printer{ID: p.ID, Name: p.Name}, e.File, false)
		}
	case events.PrintFinished:
		rec.End(e.PrinterID, "finished")
	case events.PrintFailed:
		rec.End(e.PrinterID, "failed")
	case events.PrintCancelled:
		rec.End(e.PrinterID, "cancelled")
	}
}

// timelapseTick runs every second: it picks up prints already under way (after
// a restart, or when the feature is switched on mid-print), takes the pictures
// that are due, and closes a recording whose printer has gone idle without an
// end event.
func timelapseTick(now time.Time) {
	rec := timelapseRecorder()
	cfg := timelapseSettings()
	active := rec.ActiveIDs()
	if !cfg.Enabled && len(active) == 0 {
		return
	}
	configMutex.RLock()
	printers := slices.Clone(configStore.Printers)
	configMutex.RUnlock()
	states := liveStates()
	interval := time.Duration(timelapse.Interval(cfg.IntervalSec)) * time.Second

	known := map[string]bool{}
	for _, p := range printers {
		known[p.ID] = true
		s := states[p.Name]
		if s == nil {
			continue
		}
		recording := rec.Active(p.ID)
		if recording {
			if mqttpkg.Settled(s.Status) {
				if since, ok := tlIdle[p.ID]; !ok {
					tlIdle[p.ID] = now
				} else if now.Sub(since) >= timelapseIdleEnd {
					delete(tlIdle, p.ID)
					result := "cancelled"
					switch s.Status {
					case "finished":
						result = "finished"
					case "error":
						result = "failed"
					}
					rec.End(p.ID, result)
				}
			} else {
				delete(tlIdle, p.ID)
			}
		}
		if !cfg.Enabled || !timelapseWants(cfg, p) || s.Status != "printing" {
			continue
		}
		if !recording {
			rec.Start(timelapse.Printer{ID: p.ID, Name: p.Name}, s.FileName, true)
		}
		go timelapseCapture(rec, p, interval)
	}
	for _, id := range active {
		if !known[id] { // the printer was removed mid-print
			rec.End(id, "cancelled")
		}
	}
	for id := range tlIdle {
		if !known[id] {
			delete(tlIdle, id)
		}
	}
	tlMu.Lock()
	for id := range tlFailing {
		if !known[id] {
			delete(tlFailing, id)
		}
	}
	tlMu.Unlock()
}

func timelapseCapture(rec *timelapse.Recorder, p config.Printer, interval time.Duration) {
	err := rec.Capture(p.ID, interval, func() ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timelapseSnapTimeout)
		defer cancel()
		return printerSnapshot(ctx, p)
	})
	tlMu.Lock()
	defer tlMu.Unlock()
	if err == nil {
		delete(tlFailing, p.ID)
		return
	}
	// One line for the first failure and then one in twenty: a printer with no
	// usable camera would otherwise fill the log.
	if tlFailing[p.ID]++; tlFailing[p.ID]%20 == 1 {
		log.Printf("[timelapse] %s: no picture: %v", p.Name, err)
	}
}

var errNoSuchPrinters = errors.New("None of those printers exist. Reload the page and try again.")

// timelapseView is what GET and POST /api/timelapse answer with.
type timelapseView struct {
	Enabled     bool                  `json:"enabled"`
	IntervalSec int                   `json:"interval_sec"`
	KeepDays    int                   `json:"keep_days"` // 0 keeps them all
	Printers    []string              `json:"printers"`
	FFmpeg      bool                  `json:"ffmpeg"`
	Building    int                   `json:"building"`
	Recording   []timelapse.Recording `json:"recording"`
	Items       []timelapse.Meta      `json:"items"`
}

func buildTimelapseView() timelapseView {
	cfg := timelapseSettings()
	rec := timelapseRecorder()
	v := timelapseView{
		Enabled:     cfg.Enabled,
		IntervalSec: timelapse.Interval(cfg.IntervalSec),
		KeepDays:    timelapse.KeepDays(cfg.KeepDays),
		Printers:    cfg.Printers,
		FFmpeg:      ffmpegPath() != "",
		Building:    rec.Building(),
		Recording:   rec.Recording(),
		Items:       rec.List(),
	}
	if v.Printers == nil {
		v.Printers = []string{}
	}
	return v
}

// handleTimelapse reads or writes the timelapse settings, and lists the
// videos.
func handleTimelapse(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		json.NewEncoder(w).Encode(buildTimelapseView())
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		// A field left out keeps its stored value, so a script can change one
		// setting without knowing the others.
		var req struct {
			Enabled     *bool     `json:"enabled"`
			IntervalSec *int      `json:"interval_sec"`
			KeepDays    *int      `json:"keep_days"`
			Printers    *[]string `json:"printers"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if v := req.IntervalSec; v != nil && *v != 0 && (*v < timelapse.MinInterval || *v > timelapse.MaxInterval) {
			writeErr(w, http.StatusBadRequest, "The picture interval must be between 5 and 3600 seconds.")
			return
		}
		if v := req.KeepDays; v != nil && (*v < 0 || *v > timelapse.MaxKeepDays) {
			writeErr(w, http.StatusBadRequest, "Keep days must be between 0 (keep them all) and 3650.")
			return
		}
		err := mutateConfig(func(c *config.Config) error {
			t := config.Timelapse{}
			if c.Timelapse != nil {
				t = *c.Timelapse
			}
			if req.Enabled != nil {
				t.Enabled = *req.Enabled
			}
			if req.IntervalSec != nil {
				t.IntervalSec = *req.IntervalSec
			}
			if req.KeepDays != nil {
				t.KeepDays = timelapse.StoredKeepDays(*req.KeepDays)
			}
			if req.Printers != nil {
				t.Printers = nil
				for _, p := range c.Printers { // only printers that exist, in the order they are shown
					if slices.Contains(*req.Printers, p.ID) {
						t.Printers = append(t.Printers, p.ID)
					}
				}
				if len(*req.Printers) > 0 && t.Printers == nil { // an empty list would mean all printers
					return errNoSuchPrinters
				}
			}
			c.Timelapse = &t
			return nil
		})
		if errors.Is(err, errNoSuchPrinters) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		json.NewEncoder(w).Encode(buildTimelapseView())
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleTimelapseFile serves (GET, with ranges so the player can seek) or
// deletes (DELETE) one video. ?poster=1 serves its poster picture.
func handleTimelapseFile(w http.ResponseWriter, r *http.Request) {
	id := pathTail(r, "/api/timelapse/file/")
	if !timelapse.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "That is not a timelapse ID.")
		return
	}
	rec := timelapseRecorder()
	switch r.Method {
	case "GET", "HEAD":
		path, m, err := rec.Open(id, r.URL.Query().Get("poster") == "1")
		if err != nil {
			timelapseError(w, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			timelapseError(w, timelapse.ErrNotFound)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			timelapseError(w, err)
			return
		}
		ctype := map[string]string{"mp4": "video/mp4", "gif": "image/gif"}[m.Format]
		if strings.HasSuffix(path, ".jpg") {
			ctype = "image/jpeg"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
	case "DELETE":
		if err := rec.Delete(id); err != nil {
			timelapseError(w, err)
			return
		}
		jsonHeaders(w)
		w.Write([]byte(`{"status":"ok"}`))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func timelapseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, timelapse.ErrBadID):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, timelapse.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "The timelapse could not be read: "+err.Error())
	}
}
