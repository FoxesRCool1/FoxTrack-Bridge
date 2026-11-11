package plug

import (
	"context"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// Sync stops a poller but not a poll already under way; Stop waits for it, so
// the printer status lookup the poll calls cannot run after Stop returns (it
// used to outlive a test and read state the test had already reset).
func TestStop_WaitsForPollersToEnd(t *testing.T) {
	inRead := make(chan struct{}, 1)
	release := make(chan struct{})
	m := NewManager()
	m.read = func(context.Context, config.Plug) (Reading, error) {
		inRead <- struct{}{}
		<-release
		return Reading{}, nil
	}
	m.Sync(plugged(false))
	select {
	case <-inRead:
	case <-time.After(2 * time.Second):
		t.Fatal("the poller never started")
	}

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a poll was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned after the poll ended")
	}
	if m.Has("P1") {
		t.Error("Stop must also forget the plugs")
	}
}
