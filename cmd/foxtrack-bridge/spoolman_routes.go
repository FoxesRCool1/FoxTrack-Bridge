package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	"foxtrack-bridge/spoolman"
)

// Spoolman: after each ended print the filament it used is reported to the
// user's own Spoolman server on the LAN. Nothing else leaves the machine.

func init() {
	http.HandleFunc("/api/spoolman", handleSpoolman)              // GET/POST: settings, slot mapping, sync log
	http.HandleFunc("/api/spoolman/test", handleSpoolmanTest)     // POST: reach a server once
	http.HandleFunc("/api/spoolman/spools", handleSpoolmanSpools) // GET: the saved server's spools, for the picker
	onStartup(func() { history.OnAppend(syncSpoolman) })
}

var spoolSyncer = spoolman.NewSyncer()

const maxSpoolID = 1_000_000_000

type spoolmanView struct {
	Enabled bool             `json:"enabled"`
	URL     string           `json:"url"`
	Slots   map[string]int   `json:"slots"`
	Log     []spoolman.Entry `json:"log"`
}

func spoolmanViewNow() spoolmanView {
	v := spoolmanView{Slots: map[string]int{}, Log: spoolSyncer.Log()}
	if v.Log == nil {
		v.Log = []spoolman.Entry{}
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if s := configStore.Spoolman; s != nil {
		v.Enabled, v.URL = s.Enabled, s.URL
		for k, id := range s.Slots {
			v.Slots[k] = id
		}
	}
	return v
}

func handleSpoolman(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		json.NewEncoder(w).Encode(spoolmanViewNow())
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		// A field left out keeps what is stored. Slots are merged into the
		// stored map, so a stale dashboard cannot wipe the mapping: a spool
		// of 0 (or null) unmaps that slot.
		var req struct {
			Enabled *bool           `json:"enabled"`
			URL     *string         `json:"url"`
			Slots   map[string]*int `json:"slots"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "The request could not be read.")
			return
		}
		bad := func(msg string) { writeErr(w, http.StatusBadRequest, msg) }
		var url string
		if req.URL != nil {
			url = strings.TrimSpace(*req.URL)
			if url != "" {
				if _, err := spoolman.NewClient(url); err != nil {
					bad(err.Error())
					return
				}
			}
		}
		for key, id := range req.Slots {
			if !validSlotKey(key) || (id != nil && (*id < 0 || *id > maxSpoolID)) {
				bad("Slot " + strconv.Quote(key) + " is not valid.")
				return
			}
		}
		var refused string
		err := mutateConfig(func(c *config.Config) error {
			// Work on a copy and store it only once it is valid, so a refusal
			// leaves the live config as it was.
			next := config.Spoolman{Slots: map[string]int{}}
			if c.Spoolman != nil {
				next.Enabled, next.URL = c.Spoolman.Enabled, c.Spoolman.URL
				for k, id := range c.Spoolman.Slots {
					next.Slots[k] = id
				}
			}
			if req.URL != nil {
				if url != next.URL {
					next.Slots = map[string]int{} // spool IDs belong to the old server
				}
				next.URL = url
			}
			if req.Enabled != nil {
				next.Enabled = *req.Enabled
			}
			if next.Enabled && next.URL == "" {
				refused = "Enter your Spoolman address before turning it on."
				return errors.New(refused)
			}
			for key, id := range req.Slots {
				if id == nil || *id == 0 {
					delete(next.Slots, key)
					continue
				}
				printerID, _, _ := strings.Cut(key, "/")
				if !printerExists(c, printerID) {
					refused = "Slot " + strconv.Quote(key) + " is for a printer that does not exist."
					return errors.New(refused)
				}
				next.Slots[key] = *id
			}
			if len(next.Slots) == 0 {
				next.Slots = nil
			}
			c.Spoolman = &next
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
		json.NewEncoder(w).Encode(spoolmanViewNow())
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// validSlotKey accepts "<printer id>/<slot>" with a slot from 0 to 254.
func validSlotKey(key string) bool {
	id, slot, ok := strings.Cut(key, "/")
	if !ok || !maintIDPattern.MatchString(id) {
		return false
	}
	n, err := strconv.Atoi(slot)
	return err == nil && n >= 0 && n <= 254 && strconv.Itoa(n) == slot
}

func printerExists(c *config.Config, id string) bool {
	for _, p := range c.Printers {
		if p.ID == id {
			return true
		}
	}
	return false
}

// handleSpoolmanTest reaches the server once. The body may carry the address
// being typed; without one the saved address is used.
func handleSpoolmanTest(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "The request could not be read.")
		return
	}
	reply := func(ok bool, msg string) {
		json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": msg})
	}
	url := strings.TrimSpace(req.URL)
	if url == "" {
		url = spoolmanViewNow().URL
	}
	c, err := spoolman.NewClient(url)
	if err != nil {
		reply(false, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	version, err := c.Info(ctx)
	if err != nil {
		reply(false, "Can't reach Spoolman there. "+err.Error())
		return
	}
	msg := "Spoolman found."
	if version != "" {
		msg = "Spoolman " + version + " found."
	}
	reply(true, msg)
}

func handleSpoolmanSpools(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	v := spoolmanViewNow()
	c, err := spoolman.NewClient(v.URL)
	if err != nil {
		writeErr(w, http.StatusConflict, "Save your Spoolman address first.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	spools, err := c.Spools(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Can't read the spools from Spoolman. "+err.Error())
		return
	}
	if spools == nil {
		spools = []spoolman.Spool{}
	}
	json.NewEncoder(w).Encode(spools)
}

// syncSpoolman reports a finished print's filament. It runs on the history
// listener's own goroutine, so waiting out retries blocks nothing else.
func syncSpoolman(r history.Record) {
	if r.FilamentMM <= 0 && r.TotalGrams() <= 0 {
		return
	}
	v := spoolmanViewNow()
	if !v.Enabled || v.URL == "" {
		return
	}
	p, ok := printerByName(r.PrinterName)
	if !ok {
		return
	}
	c, err := spoolman.NewClient(v.URL)
	if err != nil {
		return
	}
	// The spool list is only fetched when a slot has no mapping and the
	// printer reported a tag for it.
	var byTag func(string) (int, bool)
	lookup := func(tag string) (int, bool) {
		if byTag == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			spools, err := c.Spools(ctx)
			if err != nil {
				log.Printf("[spoolman] could not read the spool list for tag matching: %v", err)
			}
			byTag = spoolman.TagIndex(spools)
		}
		return byTag(tag)
	}
	uses := spoolman.Plan(r, p.ID, v.Slots, lookup)
	if len(uses) == 0 {
		return
	}
	spoolSyncer.Sync(context.Background(), c, p.Name, uses)
}
