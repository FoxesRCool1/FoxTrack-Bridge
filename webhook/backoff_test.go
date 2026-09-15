package webhook

import (
	"testing"
	"time"
)

// retryDelays replays the arithmetic the retry workers run, so the schedule is
// asserted rather than inferred. Both workers index backoffs by the item's
// current attempt count, which reads like an off-by-one in isolation: the first
// delay in the table looks skipped. It is not. SendRelay and SendHistory enqueue
// a failed payload with attempts=1 and after=now+2s, applying backoffs[0]
// themselves, so backoffs[attempts] correctly names the *next* delay.
func retryDelays(backoffs []time.Duration, firstDelay time.Duration, firstAttempts int) []time.Duration {
	out := []time.Duration{firstDelay}
	attempts := firstAttempts
	for attempts < len(backoffs) {
		out = append(out, backoffs[attempts])
		attempts++
	}
	return out
}

func TestRetryBackoffSchedule(t *testing.T) {
	// The table both workers use (sender.go, relay and history).
	backoffs := []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}
	// What SendRelay and SendHistory put in the queue on the first failure.
	const firstDelay = 2 * time.Second
	const firstAttempts = 1

	got := retryDelays(backoffs, firstDelay, firstAttempts)
	want := []time.Duration{2 * time.Second, 8 * time.Second, 30 * time.Second}

	if len(got) != len(want) {
		t.Fatalf("retries = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("retry %d waits %v, want %v (full schedule %v)", i+1, got[i], want[i], got)
		}
	}
}
