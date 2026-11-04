package spoolman

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/history"
)

type call struct {
	ID   string
	Body map[string]float64
}

// fake is a Spoolman server that records the /use calls. failures says how
// many of the first /use calls answer with status.
type fake struct {
	*httptest.Server
	mu       sync.Mutex
	calls    []call
	n        atomic.Int32
	failures int32
	status   int
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{status: 500}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.22.1"}`))
	})
	mux.HandleFunc("/api/v1/spool", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("allow_archived") != "false" {
			http.Error(w, "archived spools must be left out", 400)
			return
		}
		w.Write([]byte(`[
		 {"id":1,"remaining_weight":812.5,"filament":{"name":"Galaxy Black","material":"PLA","color_hex":"1A1A1A","vendor":{"name":"Prusament"}},"extra":{"tag":"\"04AABBCC\""}},
		 {"id":2,"remaining_weight":null,"filament":{"name":"Plain","material":"PETG"}},
		 {"id":3,"filament":{"name":"No tag"},"extra":{"tag":"\"0000000000000000\""}}]`))
	})
	mux.HandleFunc("PUT /api/v1/spool/{id}/use", func(w http.ResponseWriter, r *http.Request) {
		if f.n.Add(1) <= f.failures {
			http.Error(w, "spool is busy", f.status)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]float64
		json.Unmarshal(b, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.PathValue("id"), body})
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fake) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(f.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fastSyncer(delays int) *Syncer {
	s := NewSyncer()
	s.Delays = make([]time.Duration, delays)
	for i := range s.Delays {
		s.Delays[i] = time.Millisecond
	}
	return s
}

func TestNewClient(t *testing.T) {
	for _, in := range []string{"", "ftp://x", "http://u:p@host:1", "host:7912", "http://", "http://u@host"} {
		if c, err := NewClient(in); err == nil {
			t.Errorf("NewClient(%q) = %+v, want an error", in, c)
		}
	}
	for in, want := range map[string]string{
		"http://192.168.1.20:7912/":          "http://192.168.1.20:7912",
		" https://spool.lan/sub/path/?a=b#c": "https://spool.lan/sub/path",
	} {
		c, err := NewClient(in)
		if err != nil || c.base != want {
			t.Errorf("NewClient(%q) = %v, %v; want base %q", in, c, err, want)
		}
	}
	if _, err := NewClient("http://u:p@host"); !strings.Contains(err.Error(), "username and password") {
		t.Errorf("userinfo error = %v", err)
	}
}

func TestInfoAndSpools(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	ctx := context.Background()
	if v, err := c.Info(ctx); err != nil || v != "0.22.1" {
		t.Fatalf("Info = %q, %v", v, err)
	}
	spools, err := c.Spools(ctx)
	if err != nil || len(spools) != 3 {
		t.Fatalf("Spools = %+v, %v", spools, err)
	}
	if s := spools[0]; s.ID != 1 || s.Name != "Galaxy Black" || s.Vendor != "Prusament" || s.Material != "PLA" || s.Color != "1A1A1A" || s.RemainingG != 812.5 || s.Tag != "04AABBCC" {
		t.Errorf("spool 1 = %+v", s)
	}
	if s := spools[1]; s.Vendor != "" || s.Color != "" || s.RemainingG != 0 || s.Tag != "" {
		t.Errorf("spool 2 = %+v", s)
	}
}

func TestTagIndex(t *testing.T) {
	f := newFake(t)
	spools, _ := f.client(t).Spools(context.Background())
	by := TagIndex(spools)
	if id, ok := by("04aabbcc"); !ok || id != 1 {
		t.Errorf("lower-case tag = %d, %v", id, ok)
	}
	for _, tag := range []string{"0000000000000000", "", "FFFF"} {
		if id, ok := by(tag); ok {
			t.Errorf("tag %q matched spool %d", tag, id)
		}
	}
}

func TestPlan(t *testing.T) {
	slots := map[string]int{"p1/0": 7, "p1/254": 9}
	by := TagIndex([]Spool{{ID: 1, Tag: "04AABBCC"}})

	got := Plan(history.Record{Filaments: []history.FilamentUse{{Slot: 0, Grams: 12.5}, {Slot: 1}, {Slot: 254, Grams: 3}}}, "p1", slots, by)
	if len(got) != 2 || got[0] != (Use{Slot: 0, Spool: 7, Grams: 12.5}) || got[1] != (Use{Slot: 254, Spool: 9, Grams: 3}) {
		t.Errorf("mapped slots: %+v", got)
	}

	got = Plan(history.Record{Filaments: []history.FilamentUse{{Slot: 2, Grams: 4, TagUID: "04AABBCC"}, {Slot: 3, Grams: 5}, {Slot: 4, Grams: 6, TagUID: "FFFF"}}}, "p1", slots, by)
	if len(got) != 1 || got[0] != (Use{Slot: 2, Spool: 1, Grams: 4}) {
		t.Errorf("tag match: %+v", got)
	}
	if got := Plan(history.Record{Filaments: []history.FilamentUse{{Slot: 2, Grams: 4, TagUID: "04AABBCC"}}}, "p1", slots, nil); got != nil {
		t.Errorf("no tag lookup should map nothing: %+v", got)
	}

	got = Plan(history.Record{FilamentMM: 1000}, "p1", map[string]int{"p1/0": 5}, nil)
	if len(got) != 1 || got[0] != (Use{Slot: 0, Spool: 5, MM: 1000}) {
		t.Errorf("length: %+v", got)
	}
	if got := Plan(history.Record{FilamentMM: 1000}, "p1", map[string]int{"p1/1": 5}, nil); got != nil {
		t.Errorf("unmapped slot 0: %+v", got)
	}
	got = Plan(history.Record{FilamentMM: 1000, Filaments: []history.FilamentUse{{Slot: 0, Grams: 2}}}, "p1", map[string]int{"p1/0": 5}, nil)
	if len(got) != 1 || got[0].Grams != 2 || got[0].MM != 0 {
		t.Errorf("grams win over length: %+v", got)
	}
}

func TestSync(t *testing.T) {
	f := newFake(t)
	s := fastSyncer(2)
	s.Sync(context.Background(), f.client(t), "Fox", []Use{{Slot: 0, Spool: 7, Grams: 12.5}, {Slot: 0, Spool: 5, MM: 1000}})
	calls := f.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		switch c.ID {
		case "7":
			if len(c.Body) != 1 || c.Body["use_weight"] != 12.5 {
				t.Errorf("weight body = %v", c.Body)
			}
		case "5":
			if len(c.Body) != 1 || c.Body["use_length"] != 1000 {
				t.Errorf("length body = %v", c.Body)
			}
		default:
			t.Errorf("unexpected spool %s", c.ID)
		}
	}
	log := s.Log()
	if len(log) != 2 {
		t.Fatalf("log = %+v", log)
	}
	for _, e := range log {
		if e.Status != "ok" || e.Attempts != 1 || e.Printer != "Fox" || e.Time == 0 || (e.Unit == "g") != (e.Amount == 12.5) {
			t.Errorf("entry = %+v", e)
		}
	}
}

func TestSyncRetries(t *testing.T) {
	f := newFake(t)
	f.failures = 1
	s := fastSyncer(2)
	s.Sync(context.Background(), f.client(t), "Fox", []Use{{Spool: 7, Grams: 1}})
	if e := s.Log()[0]; e.Status != "ok" || e.Attempts != 2 || e.Error != "" {
		t.Errorf("one failure then success: %+v", e)
	}

	f = newFake(t)
	f.failures = 100
	s = fastSyncer(2)
	s.Sync(context.Background(), f.client(t), "Fox", []Use{{Spool: 7, Grams: 1}})
	if e := s.Log()[0]; e.Status != "error" || e.Attempts != 3 || !strings.Contains(e.Error, "500") {
		t.Errorf("always failing: %+v", e)
	}

	f = newFake(t)
	f.failures, f.status = 100, 404
	s = fastSyncer(2)
	s.Sync(context.Background(), f.client(t), "Fox", []Use{{Spool: 7, Grams: 1}})
	if e := s.Log()[0]; e.Status != "error" || e.Attempts != 1 || !strings.Contains(e.Error, "404") {
		t.Errorf("404 must not be retried: %+v", e)
	}
}

func TestSyncRetryOnlyWhenTheUseNeverArrived(t *testing.T) {
	// The connection is dropped after the request arrived: Spoolman may have
	// booked the use, so it must not be sent again.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	c, _ := NewClient(srv.URL)
	s := fastSyncer(2)
	s.Sync(context.Background(), c, "Fox", []Use{{Spool: 7, Grams: 1}})
	if e := s.Log()[0]; e.Status != "error" || e.Attempts != 1 || hits.Load() != 1 {
		t.Errorf("dropped after sending: %+v, %d hits, want one try", e, hits.Load())
	}

	// Nothing is listening: the use never left, so retrying is safe.
	srv.Close()
	s = fastSyncer(2)
	s.Sync(context.Background(), c, "Fox", []Use{{Spool: 7, Grams: 1}})
	if e := s.Log()[0]; e.Status != "error" || e.Attempts != 3 {
		t.Errorf("connection refused: %+v, want 3 tries", e)
	}
}

func TestSyncStopsWhenCancelled(t *testing.T) {
	f := newFake(t)
	f.failures = 100
	s := NewSyncer() // real delays: the cancel has to cut the wait short
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Sync(ctx, f.client(t), "Fox", []Use{{Spool: 7, Grams: 1}}); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Sync did not stop after the context was cancelled")
	}
	if e := s.Log()[0]; e.Status != "error" {
		t.Errorf("entry = %+v", e)
	}
}

func TestLogKeepsTheLast20NewestFirst(t *testing.T) {
	f := newFake(t)
	s := fastSyncer(0)
	c := f.client(t)
	for i := 1; i <= 25; i++ {
		s.Sync(context.Background(), c, "Fox", []Use{{Spool: i, Grams: 1}})
	}
	log := s.Log()
	if len(log) != 20 || log[0].Spool != 25 || log[19].Spool != 6 {
		t.Fatalf("log has %d entries, first spool %d, last %d", len(log), log[0].Spool, log[len(log)-1].Spool)
	}
}

func TestSyncNilClient(t *testing.T) {
	s := NewSyncer()
	s.Sync(context.Background(), nil, "Fox", []Use{{Spool: 1, Grams: 1}})
	if len(s.Log()) != 0 {
		t.Error("a nil client should log nothing")
	}
}
