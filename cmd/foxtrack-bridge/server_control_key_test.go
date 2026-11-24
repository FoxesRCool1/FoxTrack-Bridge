package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetControlKeys empties the package-level key map now and when the test
// ends, so the test passes again under -count=2.
func resetControlKeys(t *testing.T) {
	t.Helper()
	wipe := func() {
		controlKeysMu.Lock()
		defer controlKeysMu.Unlock()
		for k := range controlKeys {
			delete(controlKeys, k)
		}
	}
	wipe()
	t.Cleanup(wipe)
}

func TestControlKeys(t *testing.T) {
	resetControlKeys(t)
	now := time.Now()
	k := controlKey("Fox", "pause", "k-1")
	if controlKey("Fox", "pause", "") != "" || !reserveControlKey("", now) || !reserveControlKey("", now) {
		t.Fatal("no key is never a repeat")
	}
	if !reserveControlKey(k, now) {
		t.Fatal("a key that never ran is not a repeat")
	}
	if reserveControlKey(k, now.Add(time.Second)) {
		t.Fatal("the same click again must be dropped")
	}
	if !reserveControlKey(controlKey("Fox", "stop", "k-1"), now) || !reserveControlKey(controlKey("Owl", "pause", "k-1"), now) {
		t.Fatal("the key is per printer and command")
	}
	if !reserveControlKey(k, now.Add(11*time.Minute)) {
		t.Fatal("a key is forgotten after 10 minutes")
	}
}

// A failed command gives its key back, so the retry runs.
func TestControlKeys_ReleaseAfterFailureAllowsRetry(t *testing.T) {
	resetControlKeys(t)
	now := time.Now()
	k := controlKey("Fox", "pause", "k-2")
	if !reserveControlKey(k, now) {
		t.Fatal("first request must run")
	}
	releaseControlKey(k) // runControl failed
	if !reserveControlKey(k, now.Add(time.Second)) {
		t.Fatal("a failed command may be retried with the same key")
	}
	releaseControlKey("") // an empty key is a no-op
}

// Two requests with the same key that are in flight together run the command
// once: the key is claimed before the command runs, not after.
func TestControlKeys_ConcurrentSameKeyRunsOnce(t *testing.T) {
	resetControlKeys(t)
	k := controlKey("Fox", "pause", "k-3")
	var ran atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if reserveControlKey(k, time.Now()) {
				ran.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := ran.Load(); got != 1 {
		t.Fatalf("command ran %d times, want 1", got)
	}
}
