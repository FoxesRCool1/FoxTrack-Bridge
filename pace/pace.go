// Package pace decides how often Bridge talks to FoxTrack.
//
// Every telemetry post, camera snapshot and command poll is a billed call on
// the FoxTrack side, and most of them used to go out whether or not anyone
// was looking. The bridge-commands reply now says whether someone has a
// FoxTrack page with printers open ("watched"). While watched, Bridge sends
// quick updates. Otherwise it sends status changes at once and everything
// else on a one-minute heartbeat, which is what keeps a printer online on the
// website.
//
// Until a reply carries the field (an older FoxTrack server, no FoxTrack 2
// key, or the first seconds after start) Bridge keeps its old pace, so nothing
// gets slower by accident.
package pace

import (
	"net/http"
	"sync"
	"time"
)

const (
	// HeartbeatSec is how often an unchanged printer still reports. The website
	// marks a printer offline after 90 s of silence, so this must stay well
	// under that in every mode.
	HeartbeatSec = 60

	// SnapshotGapLegacySec is the old fixed snapshot pace, kept for Bridge
	// running against a FoxTrack server that does not send "watched".
	SnapshotGapLegacySec = 25

	// Minimum seconds between two non-urgent telemetry sends. The website
	// refreshes every 5 s, so sending faster than every 10 s buys nothing.
	relayGapWatchedSec = 10
	relayGapIdleSec    = HeartbeatSec

	// Minimum seconds between two snapshots of a printing printer.
	snapshotGapWatchedSec = 30
	snapshotGapIdleSec    = 10 * 60

	// A "watched" reply keeps quick mode this long, so one slow or failed
	// poll does not drop Bridge to its slow pace while someone is looking.
	watchHoldSec = 90

	// PollLegacy is the old fixed command-poll delay.
	PollLegacy = 4 * time.Second
	pollMin    = 2 * time.Second
	pollMax    = 2 * time.Minute

	// Delays after a failed command poll. A refused token or plan gives the
	// same answer every time, so there is no point asking every few seconds.
	pollAfterRefused = 5 * time.Minute
	pollAfterError   = 30 * time.Second
)

var (
	mu           sync.Mutex
	known        bool  // a bridge-commands reply said whether anyone is watching
	watchedUntil int64 // unix seconds; quick mode until then
)

// Update records what the last bridge-commands reply said. hasWatched is false
// when the reply had no "watched" field: Bridge then keeps its old pace.
func Update(hasWatched, watched bool, now int64) {
	mu.Lock()
	defer mu.Unlock()
	if !hasWatched {
		known = false
		watchedUntil = 0
		return
	}
	known = true
	if watched {
		watchedUntil = now + watchHoldSec
	} else {
		watchedUntil = 0
	}
}

// Refused records that FoxTrack turned the Bridge token down (revoked token or
// a plan without Bridge). Nobody can watch these printers, so Bridge drops to
// its slow pace instead of sending rejected updates every second.
func Refused() {
	mu.Lock()
	defer mu.Unlock()
	known = true
	watchedUntil = 0
}

// Reset forgets every reply. For tests.
func Reset() {
	Update(false, false, 0)
}

// legacy reports whether no reply has said whether anyone is watching.
// watched reports quick mode. Both read under the lock.
func state(now int64) (legacy, watched bool) {
	mu.Lock()
	defer mu.Unlock()
	if !known {
		return true, false
	}
	return false, now < watchedUntil
}

// Watched reports whether someone had a FoxTrack page with printers open at
// the last poll. It is also true while the server has not said (old pace).
func Watched(now int64) bool {
	legacy, watched := state(now)
	return legacy || watched
}

// RelayGap is the minimum number of seconds between two non-urgent
// telemetry sends for one printer.
func RelayGap(now int64) int64 {
	legacy, watched := state(now)
	switch {
	case legacy:
		return 0
	case watched:
		return relayGapWatchedSec
	default:
		return relayGapIdleSec
	}
}

// SnapshotGap is the minimum number of seconds between two camera snapshots
// of one printing printer.
func SnapshotGap(now int64) int64 {
	legacy, watched := state(now)
	switch {
	case legacy:
		return SnapshotGapLegacySec
	case watched:
		return snapshotGapWatchedSec
	default:
		return snapshotGapIdleSec
	}
}

// Decide reports whether one telemetry update goes to FoxTrack now.
//
//	urgent:    FoxTrack must see it at once: first reading, or the status,
//	           file, error or light changed.
//	changed:   another field FoxTrack shows changed (progress, temperatures,
//	           time left).
//	held:      an earlier change was not sent yet.
//	sinceLast: seconds since the last send for this printer.
//
// stillHeld reports whether a change is still waiting after this call; the
// caller passes it back as held next time. When send is true the caller must
// record now as the last send.
func Decide(urgent, changed, held bool, sinceLast, now int64) (send, stillHeld bool) {
	if urgent {
		return true, false
	}
	if changed || held {
		if sinceLast >= RelayGap(now) {
			return true, false
		}
		return false, true
	}
	return sinceLast >= HeartbeatSec, false
}

// PollDelay is how long to wait before the next command poll after a good
// reply. pollAfterMs is the reply's poll_after_ms; 0 means the server did not
// say, so Bridge keeps the old 4 s.
func PollDelay(pollAfterMs int64) time.Duration {
	if pollAfterMs <= 0 {
		return PollLegacy
	}
	d := time.Duration(pollAfterMs) * time.Millisecond
	if d < pollMin {
		return pollMin
	}
	if d > pollMax {
		return pollMax
	}
	return d
}

// ErrorDelay is how long to wait before the next command poll after a failed
// one. status is the HTTP status, or 0 when the request never got an answer.
func ErrorDelay(status int) time.Duration {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return pollAfterRefused
	}
	return pollAfterError
}
