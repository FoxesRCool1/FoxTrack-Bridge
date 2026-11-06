package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// Print queue: files already on a printer, lined up to print one after the
// other. The Bridge never starts the next one by itself: someone has to clear
// the bed and press "Start next", because a print started on a full bed
// crashes into the last one. The queue lives in queue.json next to
// config.json, so config.json is never touched by it.

const maxQueuePerPrinter = 50

type queueItem struct {
	ID       string `json:"id"`
	FileName string `json:"file_name"`
	AddedAt  int64  `json:"added_at"`
}

var printQueue struct {
	sync.Mutex
	loaded    bool
	byPrinter map[string][]queueItem // printer ID -> items, head first
	starting  map[string]bool        // printer IDs with a start in flight
}

var (
	errQueueNotReady = errors.New("The printer has to be idle or finished, and connected, to start the next print.")
	errQueueEmpty    = errors.New("The queue for this printer is empty.")
	errQueueBusy     = errors.New("The next print is already being started.")
)

func init() {
	http.HandleFunc("/api/queue", handleQueue)
	http.HandleFunc("/api/queue/", handleQueue)
}

func queuePath() string { return filepath.Join(config.ConfigDir(), "queue.json") }

// queueLoadLocked reads queue.json once. A missing file is an empty queue; a
// file that does not parse is moved aside, never deleted. When the file cannot
// be read or moved aside, the queue shows empty, saves are refused (so the file
// is never overwritten) and the next call tries again.
func queueLoadLocked() {
	if printQueue.loaded {
		return
	}
	printQueue.byPrinter = map[string][]queueItem{}
	if printQueue.starting == nil {
		printQueue.starting = map[string]bool{}
	}
	data, err := os.ReadFile(queuePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[queue] could not read queue.json: %v", err)
		return
	}
	if err == nil {
		if err := json.Unmarshal(data, &printQueue.byPrinter); err != nil {
			printQueue.byPrinter = map[string][]queueItem{}
			bak := queuePath() + ".corrupt-" + time.Now().Format("20060102-150405")
			if rerr := os.Rename(queuePath(), bak); rerr != nil {
				log.Printf("[queue] queue.json did not parse (%v) and could not be moved aside: %v", err, rerr)
				return
			}
			log.Printf("[queue] queue.json did not parse (%v); moved it to %s", err, filepath.Base(bak))
		}
	}
	printQueue.loaded = true
}

var errQueueUnreadable = errors.New("queue.json could not be read, so the queue was not saved. Check the Bridge log.")

func queueSaveLocked() error {
	if !printQueue.loaded {
		return errQueueUnreadable
	}
	data, err := json.MarshalIndent(printQueue.byPrinter, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.ConfigDir(), 0700); err != nil {
		return err
	}
	return config.WriteFileAtomic(queuePath(), data, 0600)
}

// queueSnapshot returns the queues of printers that still exist, keyed by
// printer ID. Queues of removed printers are dropped here and on next save.
func queueSnapshot() map[string][]queueItem {
	configMutex.RLock()
	known := make(map[string]bool, len(configStore.Printers))
	for _, p := range configStore.Printers {
		known[p.ID] = true
	}
	configMutex.RUnlock()
	printQueue.Lock()
	defer printQueue.Unlock()
	queueLoadLocked()
	out := map[string][]queueItem{}
	for id, items := range printQueue.byPrinter {
		if known[id] && len(items) > 0 {
			out[id] = append([]queueItem(nil), items...)
		}
	}
	return out
}

// printerReadyForNext reports whether the printer is connected and not busy.
func printerReadyForNext(name string) bool {
	t := mqttpkg.GetPrintersState()[name]
	if t == nil {
		t = lanCtrl.GetStates()[name]
	}
	if t == nil {
		return false
	}
	return t.Status == "idle" || t.Status == "finished"
}

// queueStartNext starts the head of a printer's queue. The item leaves the
// queue only once the printer accepted the start; a refused start keeps it.
func queueStartNext(printerID string, ready func(string) bool, start func(name, file string) error) (queueItem, error) {
	p, ok := printerByID(printerID)
	if !ok {
		return queueItem{}, errQueueEmpty
	}
	printQueue.Lock()
	queueLoadLocked()
	items := printQueue.byPrinter[printerID]
	if len(items) == 0 {
		printQueue.Unlock()
		return queueItem{}, errQueueEmpty
	}
	if printQueue.starting[printerID] {
		printQueue.Unlock()
		return queueItem{}, errQueueBusy
	}
	head := items[0]
	printQueue.starting[printerID] = true
	printQueue.Unlock()
	defer func() {
		printQueue.Lock()
		delete(printQueue.starting, printerID)
		printQueue.Unlock()
	}()

	if !ready(p.Name) {
		return queueItem{}, errQueueNotReady
	}
	if err := start(p.Name, head.FileName); err != nil {
		return queueItem{}, err
	}

	printQueue.Lock()
	defer printQueue.Unlock()
	cur := printQueue.byPrinter[printerID]
	for i, it := range cur {
		if it.ID == head.ID {
			printQueue.byPrinter[printerID] = append(cur[:i:i], cur[i+1:]...)
			break
		}
	}
	if err := queueSaveLocked(); err != nil {
		log.Printf("[queue] started %q on %s but could not save the queue: %v", head.FileName, p.Name, err)
	}
	return head, nil
}

// handleQueue:
//
//	GET    /api/queue                          all queues, by printer ID
//	POST   /api/queue/{printerID}              {"file_name"} add to the end
//	DELETE /api/queue/{printerID}/{itemID}     remove one
//	POST   /api/queue/{printerID}/move         {"item_id","to"} move to index
//	POST   /api/queue/{printerID}/start        start the head (bed must be clear)
func handleQueue(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/queue"), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(queueSnapshot())
		return
	}
	printerID := parts[0]
	if _, ok := printerByID(printerID); !ok {
		http.Error(w, "No such printer.", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPost && len(parts) == 1:
		var req struct {
			FileName string `json:"file_name"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.FileName)
		if name == "" || utf8.RuneCountInString(name) > 255 {
			http.Error(w, "Enter the name of a file on the printer.", http.StatusBadRequest)
			return
		}
		printQueue.Lock()
		queueLoadLocked()
		if len(printQueue.byPrinter[printerID]) >= maxQueuePerPrinter {
			printQueue.Unlock()
			http.Error(w, "This printer already has 50 files queued.", http.StatusBadRequest)
			return
		}
		printQueue.byPrinter[printerID] = append(printQueue.byPrinter[printerID], queueItem{ID: randomHex(6), FileName: name, AddedAt: time.Now().Unix()})
		err := queueSaveLocked()
		printQueue.Unlock()
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
	case r.Method == http.MethodDelete && len(parts) == 2:
		printQueue.Lock()
		queueLoadLocked()
		items := printQueue.byPrinter[printerID]
		found := false
		for i, it := range items {
			if it.ID == parts[1] {
				printQueue.byPrinter[printerID] = append(items[:i:i], items[i+1:]...)
				found = true
				break
			}
		}
		var err error
		if found {
			err = queueSaveLocked()
		}
		printQueue.Unlock()
		if !found {
			http.Error(w, "That file is no longer in the queue.", http.StatusNotFound)
			return
		}
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "move":
		var req struct {
			ItemID string `json:"item_id"`
			To     int    `json:"to"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		printQueue.Lock()
		queueLoadLocked()
		items := printQueue.byPrinter[printerID]
		from := -1
		for i, it := range items {
			if it.ID == req.ItemID {
				from = i
			}
		}
		if from < 0 {
			printQueue.Unlock()
			http.Error(w, "That file is no longer in the queue.", http.StatusNotFound)
			return
		}
		to := max(0, min(req.To, len(items)-1))
		it := items[from]
		rest := append(items[:from:from], items[from+1:]...)
		moved := append(append(append([]queueItem(nil), rest[:to]...), it), rest[to:]...)
		printQueue.byPrinter[printerID] = moved
		err := queueSaveLocked()
		printQueue.Unlock()
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "start":
		started, err := queueStartNext(printerID, printerReadyForNext, func(name, file string) error {
			return runControl(name, "start", map[string]interface{}{"file_name": file})
		})
		if err != nil {
			code := http.StatusConflict
			if errors.Is(err, errQueueEmpty) {
				code = http.StatusNotFound
			}
			http.Error(w, err.Error(), code)
			return
		}
		log.Printf("[queue] started %q", started.FileName)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(queueSnapshot())
}
