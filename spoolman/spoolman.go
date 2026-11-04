// Package spoolman is a small client for the Spoolman REST API
// (https://github.com/Donkie/Spoolman) and the logic that reports a finished
// print's filament to it. The server is the user's own, on their network.
package spoolman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/history"
)

// Client talks to one Spoolman server.
type Client struct {
	base string
	http *http.Client
}

// NewClient checks rawURL and returns a client for it. Only http and https
// are allowed, and the address may not carry a username or password.
func NewClient(rawURL string) (*Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("Enter the address of your Spoolman server, e.g. http://192.168.1.20:7912.")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("Spoolman's address must start with http:// or https://.")
	}
	if u.Host == "" {
		return nil, errors.New("Enter the address of your Spoolman server, e.g. http://192.168.1.20:7912.")
	}
	if u.User != nil {
		return nil, errors.New("Leave the username and password out of the address.")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return &Client{
		base: u.String(),
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// HTTPError is a non-2xx answer from the server.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Spoolman answered %d", e.Status)
	}
	return fmt.Sprintf("Spoolman answered %d: %s", e.Status, e.Body)
}

// do sends one request and returns the body, read up to limit bytes.
func (c *Client) do(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := strings.TrimSpace(string(data))
		if len(text) > 120 {
			text = text[:120]
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: text}
	}
	return data, nil
}

// Info reads the server's version, which is enough to prove it is Spoolman.
func (c *Client) Info(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v1/info", nil, 1<<20)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", errors.New("That address did not answer like Spoolman.")
	}
	return v.Version, nil
}

// Spool is one spool, trimmed to what the dashboard shows.
type Spool struct {
	ID         int     `json:"id"`
	Name       string  `json:"name"` // the filament's name
	Vendor     string  `json:"vendor"`
	Material   string  `json:"material"`
	Color      string  `json:"color"` // hex without '#', may be empty
	RemainingG float64 `json:"remaining_g"`
	Tag        string  `json:"tag,omitempty"` // the spool's "tag" extra field, unquoted
}

// Spools lists the spools that are not archived.
func (c *Client) Spools(ctx context.Context) ([]Spool, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v1/spool?allow_archived=false", nil, 16<<20)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID              int      `json:"id"`
		RemainingWeight *float64 `json:"remaining_weight"`
		Filament        struct {
			Name     string `json:"name"`
			Material string `json:"material"`
			ColorHex string `json:"color_hex"`
			Vendor   *struct {
				Name string `json:"name"`
			} `json:"vendor"`
		} `json:"filament"`
		Extra map[string]string `json:"extra"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("That address did not answer like Spoolman.")
	}
	out := make([]Spool, 0, len(raw))
	for _, r := range raw {
		s := Spool{ID: r.ID, Name: r.Filament.Name, Material: r.Filament.Material, Color: strings.TrimPrefix(r.Filament.ColorHex, "#")}
		if r.Filament.Vendor != nil {
			s.Vendor = r.Filament.Vendor.Name
		}
		if r.RemainingWeight != nil {
			s.RemainingG = *r.RemainingWeight
		}
		// Spoolman keeps extra values as JSON-encoded strings: "\"04AABB\"".
		if tag, ok := r.Extra["tag"]; ok {
			var unq string
			if json.Unmarshal([]byte(tag), &unq) != nil {
				unq = tag
			}
			s.Tag = strings.TrimSpace(unq)
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *Client) use(ctx context.Context, id int, field string, amount float64) error {
	body, _ := json.Marshal(map[string]float64{field: amount})
	_, err := c.do(ctx, http.MethodPut, "/api/v1/spool/"+strconv.Itoa(id)+"/use", body, 1<<20)
	return err
}

// UseWeight subtracts grams from a spool.
func (c *Client) UseWeight(ctx context.Context, id int, grams float64) error {
	return c.use(ctx, id, "use_weight", grams)
}

// UseLength subtracts millimetres of filament from a spool.
func (c *Client) UseLength(ctx context.Context, id int, mm float64) error {
	return c.use(ctx, id, "use_length", mm)
}

// Use is one amount to subtract from one spool. Exactly one of Grams and MM is
// above 0.
type Use struct {
	Slot  int
	Spool int
	Grams float64
	MM    float64
}

// TagIndex looks a Bambu tag_uid up among the spools' tags, ignoring case.
// Empty tags and tags made only of zeros (a spool with no RFID) never match.
func TagIndex(spools []Spool) func(tag string) (int, bool) {
	return func(tag string) (int, bool) {
		tag = strings.TrimSpace(tag)
		if tag == "" || strings.Trim(tag, "0") == "" {
			return 0, false
		}
		for _, s := range spools {
			if s.Tag != "" && strings.EqualFold(s.Tag, tag) {
				return s.ID, true
			}
		}
		return 0, false
	}
}

// Plan works out what a finished print used and from which spool. slots maps
// "<printer id>/<slot>" to a Spoolman spool ID; a slot with no mapping falls
// back to byTag (may be nil). Klipper and OctoPrint prints report a length and
// use slot 0.
func Plan(r history.Record, printerID string, slots map[string]int, byTag func(string) (int, bool)) []Use {
	key := func(slot int) string { return printerID + "/" + strconv.Itoa(slot) }
	var out []Use
	grams := false
	for _, f := range r.Filaments {
		if f.Grams <= 0 {
			continue
		}
		grams = true
		spool := slots[key(f.Slot)]
		if spool <= 0 && f.TagUID != "" && byTag != nil {
			spool, _ = byTag(f.TagUID)
		}
		if spool > 0 {
			out = append(out, Use{Slot: f.Slot, Spool: spool, Grams: f.Grams})
		}
	}
	if !grams && r.FilamentMM > 0 {
		if spool := slots[key(0)]; spool > 0 {
			out = append(out, Use{Slot: 0, Spool: spool, MM: r.FilamentMM})
		}
	}
	return out
}

// Entry is one line of the sync log.
type Entry struct {
	ID       int     `json:"id"`
	Time     int64   `json:"time"` // unix seconds
	Printer  string  `json:"printer"`
	Spool    int     `json:"spool"`
	Amount   float64 `json:"amount"`
	Unit     string  `json:"unit"`   // "g" or "mm"
	Status   string  `json:"status"` // "ok", "retrying" or "error"
	Error    string  `json:"error,omitempty"`
	Attempts int     `json:"attempts"`
}

const logSize = 20

// Syncer sends uses to Spoolman and keeps the last few results for the UI.
type Syncer struct {
	// Delays are the waits before each retry, so a use gets one try plus
	// len(Delays) retries.
	Delays []time.Duration

	mu      sync.Mutex
	entries []Entry // oldest first
	next    int
}

// NewSyncer retries after 15 s, 1 min and 3 min.
func NewSyncer() *Syncer {
	return &Syncer{Delays: []time.Duration{15 * time.Second, time.Minute, 3 * time.Minute}}
}

// Log returns the last sync results, newest first.
func (s *Syncer) Log() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	for i, e := range s.entries {
		out[len(s.entries)-1-i] = e
	}
	return out
}

func (s *Syncer) add(e Entry) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	e.ID = s.next
	s.entries = append(s.entries, e)
	if len(s.entries) > logSize {
		s.entries = s.entries[len(s.entries)-logSize:]
	}
	return e.ID
}

func (s *Syncer) update(id int, fn func(*Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			fn(&s.entries[i])
			return
		}
	}
}

// Sync sends every use and returns when each has succeeded or given up. Only
// a failed connection or a 5xx answer is retried.
func (s *Syncer) Sync(ctx context.Context, c *Client, printer string, uses []Use) {
	if c == nil {
		return
	}
	var wg sync.WaitGroup
	for _, u := range uses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.syncOne(ctx, c, printer, u)
		}()
	}
	wg.Wait()
}

func (s *Syncer) syncOne(ctx context.Context, c *Client, printer string, u Use) {
	e := Entry{Time: time.Now().Unix(), Printer: printer, Spool: u.Spool, Amount: u.Grams, Unit: "g", Status: "retrying"}
	if u.Grams <= 0 {
		e.Amount, e.Unit = u.MM, "mm"
	}
	id := s.add(e)
	for try := 0; ; try++ {
		var err error
		if u.Grams > 0 {
			err = c.UseWeight(ctx, u.Spool, u.Grams)
		} else {
			err = c.UseLength(ctx, u.Spool, u.MM)
		}
		if err == nil {
			s.update(id, func(e *Entry) { e.Status, e.Error, e.Attempts = "ok", "", try+1 })
			return
		}
		var he *HTTPError
		var oe *net.OpError
		// A use is not idempotent: once the request was sent (a timeout, a
		// reset) Spoolman may have booked it, and a retry would take the
		// filament off twice. Retry only when it certainly never arrived (the
		// connection could not be made) or the server answered 5xx.
		retry := (errors.As(err, &oe) && oe.Op == "dial") || (errors.As(err, &he) && he.Status >= 500)
		giveUp := !retry || try >= len(s.Delays)
		status := "retrying"
		if giveUp {
			status = "error"
		}
		s.update(id, func(e *Entry) { e.Status, e.Error, e.Attempts = status, err.Error(), try+1 })
		if giveUp {
			return
		}
		select {
		case <-ctx.Done():
			s.update(id, func(e *Entry) { e.Status, e.Error = "error", "Stopped before it could be sent." })
			return
		case <-time.After(s.Delays[try]):
		}
	}
}
