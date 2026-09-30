package pace

import (
	"testing"
	"time"
)

func TestLegacyUntilServerSays(t *testing.T) {
	Reset()
	now := int64(1000)
	if !Watched(now) || RelayGap(now) != 0 || SnapshotGap(now) != SnapshotGapLegacySec {
		t.Fatal("before any reply Bridge must keep its old pace")
	}
	// An older server sends no "watched" field: still the old pace.
	Update(false, false, now)
	if RelayGap(now) != 0 || SnapshotGap(now) != SnapshotGapLegacySec {
		t.Fatal("a reply without the field must keep the old pace")
	}
}

func TestWatchedAndIdle(t *testing.T) {
	Reset()
	now := int64(1000)
	Update(true, true, now)
	if !Watched(now) || RelayGap(now) != relayGapWatchedSec || SnapshotGap(now) != snapshotGapWatchedSec {
		t.Fatal("a watched reply must switch to the quick pace")
	}
	// The hold keeps quick mode through a missed poll or two...
	if !Watched(now + watchHoldSec - 1) {
		t.Fatal("quick mode must last the hold time")
	}
	// ...and runs out on its own if polls stop.
	if Watched(now+watchHoldSec) || RelayGap(now+watchHoldSec) != relayGapIdleSec {
		t.Fatal("quick mode must end when the hold runs out")
	}
	Update(true, true, now)
	Update(true, false, now+5)
	if Watched(now+5) || SnapshotGap(now+5) != snapshotGapIdleSec {
		t.Fatal("a not-watched reply must switch to the slow pace at once")
	}
	// A server that stops sending the field puts Bridge back on the old pace.
	Update(false, false, now+10)
	if RelayGap(now+10) != 0 {
		t.Fatal("losing the field must restore the old pace")
	}
}

func TestRefused(t *testing.T) {
	Reset()
	Refused()
	if Watched(1000) || RelayGap(1000) != relayGapIdleSec {
		t.Fatal("a refused token must drop to the slow pace")
	}
}

func TestDecideLegacyMatchesOldRules(t *testing.T) {
	Reset()
	now := int64(5000)
	// Old rule: any change is sent at once; an unchanged printer every 60 s.
	if send, _ := Decide(false, true, false, 0, now); !send {
		t.Error("legacy: a change must be sent at once")
	}
	if send, _ := Decide(false, false, false, HeartbeatSec-1, now); send {
		t.Error("legacy: no change and under a minute must wait")
	}
	if send, _ := Decide(false, false, false, HeartbeatSec, now); !send {
		t.Error("legacy: the heartbeat must fire after a minute")
	}
}

func TestDecideIdle(t *testing.T) {
	Reset()
	now := int64(5000)
	Update(true, false, now)

	if send, held := Decide(true, false, false, 0, now); !send || held {
		t.Error("idle: an urgent change goes at once")
	}
	send, held := Decide(false, true, false, 5, now)
	if send || !held {
		t.Error("idle: a minor change waits and is held")
	}
	// Nothing new arrives, but the held change goes out with the heartbeat.
	if send, held = Decide(false, false, true, HeartbeatSec, now); !send || held {
		t.Error("idle: a held change goes out after a minute")
	}
	if send, held = Decide(false, false, false, HeartbeatSec-1, now); send || held {
		t.Error("idle: nothing to send before the heartbeat")
	}
}

func TestDecideWatched(t *testing.T) {
	Reset()
	now := int64(5000)
	Update(true, true, now)

	if send, held := Decide(false, true, false, relayGapWatchedSec-1, now); send || !held {
		t.Error("watched: a change within the gap is held")
	}
	if send, held := Decide(false, false, true, relayGapWatchedSec, now); !send || held {
		t.Error("watched: a held change goes out once the gap has passed")
	}
	if send, _ := Decide(false, false, false, relayGapWatchedSec, now); send {
		t.Error("watched: an unchanged printer still only sends the heartbeat")
	}
}

func TestPollDelay(t *testing.T) {
	cases := []struct {
		in   int64
		want time.Duration
	}{
		{0, PollLegacy},
		{-5, PollLegacy},
		{500, pollMin},
		{5000, 5 * time.Second},
		{30000, 30 * time.Second},
		{10 * 60 * 1000, pollMax},
	}
	for _, c := range cases {
		if got := PollDelay(c.in); got != c.want {
			t.Errorf("PollDelay(%d) = %v, want %v", c.in, got, c.want)
		}
	}
	if ErrorDelay(401) != pollAfterRefused || ErrorDelay(403) != pollAfterRefused {
		t.Error("a refused token must back off")
	}
	if ErrorDelay(500) != pollAfterError || ErrorDelay(0) != pollAfterError {
		t.Error("other failures retry sooner")
	}
}
