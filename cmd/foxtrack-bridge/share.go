package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/web"
)

// Read-only share links (Settings, Security).
//
// A share link shows one printer's progress, temperatures and, when the owner
// allows it, a camera still to anyone who has the link and can reach this
// Bridge. It never shows controls, other printers, addresses, access codes or
// file lists. Only the SHA-256 of a link's token is stored, so a link is shown
// once, when it is made; revoking it stops it at once.

const (
	sharePrefix   = "fts_"
	maxShareLinks = 20
	// shareStillTTL is how long one camera still is reused, so several people
	// watching a link do not each open a camera connection to the printer.
	shareStillTTL = 10 * time.Second
)

func init() {
	http.HandleFunc("/share/", handleSharePage)
	http.HandleFunc("/api/shares", handleShares)
	http.HandleFunc("/api/shares/", handleShares)
}

type shareView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PrinterID   string `json:"printer_id"`
	PrinterName string `json:"printer_name"`
	Camera      bool   `json:"camera"`
	Hint        string `json:"hint,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

func shareViews() []shareView {
	configMutex.RLock()
	defer configMutex.RUnlock()
	out := []shareView{}
	if configStore == nil || configStore.Security == nil {
		return out
	}
	names := make(map[string]string, len(configStore.Printers))
	for _, p := range configStore.Printers {
		names[p.ID] = p.Name
	}
	for _, s := range configStore.Security.Shares {
		out = append(out, shareView{ID: s.ID, Name: s.Name, PrinterID: s.PrinterID, PrinterName: names[s.PrinterID], Camera: s.Camera, Hint: s.Hint, CreatedAt: s.CreatedAt})
	}
	return out
}

// handleShares lists, creates and revokes share links. It sits behind the
// dashboard sign-in like every other /api route.
func handleShares(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/shares"), "/")
	switch {
	case r.Method == http.MethodGet && id == "":
		json.NewEncoder(w).Encode(shareViews())
	case r.Method == http.MethodPost && id == "":
		var req struct {
			Name      string `json:"name"`
			PrinterID string `json:"printer_id"`
			Camera    bool   `json:"camera"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		name := strings.TrimSpace(req.Name)
		if utf8.RuneCountInString(name) > 60 {
			http.Error(w, "Keep the name under 60 characters.", http.StatusBadRequest)
			return
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tok := sharePrefix + base64.RawURLEncoding.EncodeToString(raw)
		link := config.ShareLink{ID: randomHex(8), Name: name, PrinterID: req.PrinterID, Camera: req.Camera, Hash: sha256Hex(tok), Hint: tok[len(tok)-4:], CreatedAt: time.Now().Unix()}
		err := mutateConfig(func(c *config.Config) error {
			found := false
			for _, p := range c.Printers {
				if p.ID == req.PrinterID && p.ID != "" {
					found = true
					if link.Name == "" {
						link.Name = p.Name
					}
					break
				}
			}
			if !found {
				return errors.New("Pick a printer to share.")
			}
			if c.Security == nil {
				c.Security = &config.Security{}
			}
			if len(c.Security.Shares) >= maxShareLinks {
				return errors.New("There are already 20 share links. Revoke one first.")
			}
			c.Security.Shares = append(c.Security.Shares, link)
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("[share] link %q created", link.Name)
		json.NewEncoder(w).Encode(map[string]any{"token": tok, "path": "/share/" + tok, "shares": shareViews()})
	case r.Method == http.MethodDelete && id != "":
		found := false
		err := mutateConfig(func(c *config.Config) error {
			if c.Security == nil {
				return nil
			}
			kept := c.Security.Shares[:0:0]
			for _, s := range c.Security.Shares {
				if s.ID == id {
					found = true
					continue
				}
				kept = append(kept, s)
			}
			c.Security.Shares = kept
			return nil
		})
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		if !found {
			http.Error(w, "No such share link.", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(shareViews())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// shareLookup finds the link and its printer for a token. Every stored hash
// is compared in constant time, so the answer time does not depend on how
// much of a guessed token matched.
func shareLookup(tok string) (config.ShareLink, config.Printer, bool) {
	if !strings.HasPrefix(tok, sharePrefix) || len(tok) > 100 {
		return config.ShareLink{}, config.Printer{}, false
	}
	want := []byte(sha256Hex(tok))
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil || configStore.Security == nil {
		return config.ShareLink{}, config.Printer{}, false
	}
	var link config.ShareLink
	ok := false
	for _, s := range configStore.Security.Shares {
		if subtle.ConstantTimeCompare([]byte(s.Hash), want) == 1 {
			link, ok = s, true
		}
	}
	if !ok {
		return config.ShareLink{}, config.Printer{}, false
	}
	for _, p := range configStore.Printers {
		if p.ID == link.PrinterID {
			return link, p, true
		}
	}
	return config.ShareLink{}, config.Printer{}, false
}

// shareStatus is everything a share page may show. Nothing here identifies
// the network or the printer beyond the name the owner gave it.
type shareStatus struct {
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	FileName      string  `json:"file_name,omitempty"`
	Progress      int     `json:"progress"`
	TimeRemaining int     `json:"time_remaining"`
	NozzleTemp    float64 `json:"nozzle_temp"`
	NozzleTarget  float64 `json:"nozzle_target"`
	BedTemp       float64 `json:"bed_temp"`
	BedTarget     float64 `json:"bed_target"`
	Camera        bool    `json:"camera"`
	Updated       int64   `json:"updated"`
}

func shareStatusFor(link config.ShareLink, p config.Printer) shareStatus {
	out := shareStatus{Name: link.Name, Status: "disconnected", Camera: link.Camera && !p.CameraHidden}
	t := mqttpkg.GetPrintersState()[p.Name]
	if t == nil {
		t = lanCtrl.GetStates()[p.Name]
	}
	if t == nil {
		return out
	}
	out.Status = t.Status
	out.FileName = t.FileName
	out.Progress = int(t.Progress)
	out.TimeRemaining = int(t.TimeRemaining)
	out.NozzleTemp, out.NozzleTarget = t.NozzleTemp, t.NozzleTarget
	out.BedTemp, out.BedTarget = t.BedTemp, t.BedTarget
	out.Updated = t.Timestamp
	return out
}

var shareStills struct {
	sync.Mutex
	byPrinter map[string]shareStill
}

type shareStill struct {
	at  time.Time
	jpg []byte
}

// ponytail: one lock for all printers, so two shared printers' stills are
// fetched one after the other; per-printer locks if that ever matters.
func shareCameraStill(ctx context.Context, p config.Printer) ([]byte, error) {
	shareStills.Lock()
	defer shareStills.Unlock()
	if s, ok := shareStills.byPrinter[p.ID]; ok && time.Since(s.at) < shareStillTTL {
		return s.jpg, nil
	}
	jpg, err := printerSnapshot(ctx, p)
	if err != nil {
		return nil, err
	}
	if shareStills.byPrinter == nil {
		shareStills.byPrinter = map[string]shareStill{}
	}
	shareStills.byPrinter[p.ID] = shareStill{at: time.Now(), jpg: jpg}
	return jpg, nil
}

// handleSharePage serves /share/{token}, /share/{token}/status and
// /share/{token}/camera.jpg. These paths are public (no sign-in): the token
// is the credential.
func handleSharePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	rest := strings.TrimPrefix(r.URL.Path, "/share/")
	tok, sub, _ := strings.Cut(rest, "/")
	link, p, ok := shareLookup(tok)
	if !ok {
		http.Error(w, "This share link is not valid. It may have been revoked.", http.StatusNotFound)
		return
	}
	switch sub {
	case "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		w.Write(web.Share)
	case "status":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(shareStatusFor(link, p))
	case "camera.jpg":
		if !link.Camera || p.CameraHidden {
			http.Error(w, "The camera is not shared.", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		jpg, err := shareCameraStill(ctx, p)
		if err != nil {
			http.Error(w, "The camera is not available right now.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpg)
	default:
		http.NotFound(w, r)
	}
}
