package plug

import (
	"context"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// A Sync that races Stop must not start a loop (loops.Add during Wait) and
// must not panic.
func TestStop_ThenSyncStartsNothing(t *testing.T) {
	m := NewManager()
	m.read = func(context.Context, config.Plug) (Reading, error) { return Reading{}, nil }
	m.Stop()
	m.Sync(plugged(false))
	done := make(chan struct{})
	go func() { m.loops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a poll loop started after Stop")
	}
}
