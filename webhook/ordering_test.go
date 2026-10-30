package webhook

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func relayItem(url, serial string, seq uint64, percent int) retryItem {
	return retryItem{
		apiKey: "k", webhookURL: url, serial: serial, name: serial,
		payload:   RelayPayload{Print: RelayPrint{McPercent: percent}},
		attempts:  1,
		createdAt: time.Now(),
		st:        relayStateFor(url, serial),
		seq:       seq,
	}
}

// A retry of an older payload must be dropped once a newer one was delivered,
// or FoxTrack shows the printer printing again after it finished.
func TestRelayRetry_DroppedWhenNewerDelivered(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	st := relayStateFor(srv.URL, "drop-old")
	older := relayItem(srv.URL, "drop-old", st.seq.Add(1), 99)
	newer := relayItem(srv.URL, "drop-old", st.seq.Add(1), 100)

	if err, skipped := deliverRelay(newer); err != nil || skipped {
		t.Fatalf("newer payload: err=%v skipped=%v", err, skipped)
	}
	runRelayRetry(older)
	if got := hits.Load(); got != 1 {
		t.Fatalf("server saw %d posts, want 1: the older retry must not be sent", got)
	}
}

// Posts racing each other must reach FoxTrack in sequence order.
func TestSendRelay_NeverDeliversOlderAfterNewer(t *testing.T) {
	var mu sync.Mutex
	last := 0
	var reversed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p RelayPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		if p.Print.McPercent < last {
			reversed = true
		}
		last = p.Print.McPercent
		mu.Unlock()
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := relayStateFor(srv.URL, "race")
			seq := st.seq.Add(1)
			time.Sleep(time.Duration(rand.Intn(20)) * time.Millisecond) // arrive out of order
			_ = sendRelaySeq(relayItem(srv.URL, "race", seq, int(seq)))
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if reversed {
		t.Fatal("an older sequence was delivered after a newer one")
	}
	if last != 30 {
		t.Fatalf("newest payload (30) must be the last one delivered, got %d", last)
	}
}

// History must keep retrying past the old 2s+8s+30s window.
func TestSendHistory_RetriesPastOldLimit(t *testing.T) {
	oldBase, oldMax := historyBackoffBase, historyBackoffMax
	historyBackoffBase, historyBackoffMax = time.Millisecond, 4*time.Millisecond
	defer func() { historyBackoffBase, historyBackoffMax = oldBase, oldMax }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 8 { // old code gave up after 4 posts
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	SendHistory("k", srv.URL, HistoryPayload{PrinterName: "p", Result: "finished"})
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 9 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := hits.Load(); got < 9 {
		t.Fatalf("history retried only %d times, want it to keep going until FoxTrack answers", got)
	}
}

func TestHistoryDelay_CappedAtMax(t *testing.T) {
	if got := historyDelay(1); got != historyBackoffBase {
		t.Errorf("first delay = %v, want %v", got, historyBackoffBase)
	}
	if got := historyDelay(50); got != historyBackoffMax {
		t.Errorf("late delay = %v, want cap %v", got, historyBackoffMax)
	}
}
