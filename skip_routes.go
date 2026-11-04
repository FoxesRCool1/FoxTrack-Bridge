package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

func init() {
	http.HandleFunc("/api/objects/", handleObjects)
}

// skipList and skipRun are the two seams the tests swap out so no real
// printer is needed. They are package vars so a test can replace them and
// restore the originals in a cleanup.
var (
	skipList = skipListFor
	skipRun  = skipRunFor
)

// skipRequestTimeout bounds a single skip request. Reading the plate file
// from a Bambu printer can take a while, so this is generous.
var skipRequestTimeout = 3 * time.Minute

// skipListFor returns the objects on the plate for a printer.
func skipListFor(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
	if isBambuPrinterConfig(p) {
		configMutex.RLock()
		mp := mqttPrinter(p, configStore)
		configMutex.RUnlock()
		return mqttpkg.ListSkipObjects(ctx, mp)
	}
	if p.Kind() == config.KindKlipper {
		return lanCtrl.ExcludeObjects(ctx, p.Name)
	}
	return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Skipping objects works on Bambu and Klipper printers."}
}

// skipRunFor cancels the named objects on the plate for a printer.
func skipRunFor(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
	if isBambuPrinterConfig(p) {
		configMutex.RLock()
		mp := mqttPrinter(p, configStore)
		configMutex.RUnlock()
		return mqttpkg.SkipObjects(ctx, mp, ids)
	}
	if p.Kind() == config.KindKlipper {
		return lanCtrl.ExcludeObject(ctx, p.Name, ids)
	}
	return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Skipping objects works on Bambu and Klipper printers."}
}

// skipResponse is the JSON shape both routes return.
type skipResponse struct {
	mqttpkg.SkipList
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

// writeSkip encodes a skip response, making sure Objects is never null.
func writeSkip(w http.ResponseWriter, supported bool, reason string, list mqttpkg.SkipList) {
	if list.Objects == nil {
		list.Objects = []mqttpkg.SkipObject{}
	}
	resp := skipResponse{SkipList: list, Supported: supported, Reason: reason}
	json.NewEncoder(w).Encode(resp)
}

// printingNow reports whether the printer is currently printing or paused.
func printingNow(p config.Printer) bool {
	st := liveStates()[p.Name]
	return st != nil && (st.Status == "printing" || st.Status == "paused")
}

// handleObjects serves GET /api/objects/{id} and POST /api/objects/{id}/skip.
func handleObjects(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}

	tail := pathTail(r, "/api/objects/")
	parts := strings.Split(tail, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}

	p, ok := printerByID(id)
	if !ok {
		http.Error(w, "Printer not found.", http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		if len(parts) != 1 || parts[0] == "" {
			http.Error(w, "Not found.", http.StatusNotFound)
			return
		}
		handleSkipList(w, r, p)
	case "POST":
		if len(parts) != 2 || parts[1] != "skip" {
			http.Error(w, "Not found.", http.StatusNotFound)
			return
		}
		handleSkipRun(w, r, p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSkipList answers GET /api/objects/{id}.
func handleSkipList(w http.ResponseWriter, r *http.Request, p config.Printer) {
	if !printingNow(p) {
		writeSkip(w, false, "Objects can be skipped only while a print is running or paused.", mqttpkg.SkipList{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), skipRequestTimeout)
	defer cancel()
	list, err := skipList(ctx, p)
	if err != nil {
		var unsupported *mqttpkg.SkipUnsupported
		if errors.As(err, &unsupported) {
			writeSkip(w, false, unsupported.Reason, mqttpkg.SkipList{})
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "The printer took too long to answer. Try again.", http.StatusGatewayTimeout)
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeSkip(w, true, "", list)
}

// handleSkipRun answers POST /api/objects/{id}/skip.
func handleSkipRun(w http.ResponseWriter, r *http.Request, p config.Printer) {
	if !requireJSON(w, r) {
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64*1024)
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That request was not valid.", http.StatusBadRequest)
		return
	}
	var ids []string
	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "Pick at least one object to skip.", http.StatusBadRequest)
		return
	}
	if len(ids) > 200 {
		http.Error(w, "Too many objects in one request.", http.StatusBadRequest)
		return
	}
	if !printingNow(p) {
		http.Error(w, "Objects can be skipped only while a print is running or paused.", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), skipRequestTimeout)
	defer cancel()
	list, err := skipRun(ctx, p, ids)
	if err != nil {
		var unsupported *mqttpkg.SkipUnsupported
		if errors.As(err, &unsupported) {
			http.Error(w, unsupported.Reason, http.StatusConflict)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "The printer took too long to answer. Try again.", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeSkip(w, true, "", list)
}
