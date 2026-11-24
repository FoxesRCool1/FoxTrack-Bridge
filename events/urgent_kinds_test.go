package events

import "testing"

// Urgent kinds bypass quiet hours and rate limits, so the set is pinned: it is
// exactly these five, and every other kind in All stays non-urgent.
func TestUrgentKinds(t *testing.T) {
	urgent := map[Kind]bool{
		PrintFailed:           true,
		PrintFailureSuspected: true,
		PrinterError:          true,
		PrinterAlarmCleared:   true,
		Test:                  true,
	}
	kinds := append([]Kind{}, All...)
	kinds = append(kinds, PrinterError, PrinterAlarmCleared)
	for _, k := range kinds {
		if got := k.Urgent(); got != urgent[k] {
			t.Errorf("%s.Urgent() = %v, want %v", k, got, urgent[k])
		}
	}
	for k := range urgent {
		if !k.Urgent() {
			t.Errorf("%s must be urgent", k)
		}
	}
}
