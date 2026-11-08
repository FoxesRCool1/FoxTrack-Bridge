package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"foxtrack-bridge/config"
)

// A print in progress is tracked in memory, so a Bridge restart in the middle
// of one would start it again with the restart time as its start and an empty
// spool snapshot. The printer packages keep a copy of each active session in
// sessions.json next to history.json: written when a session starts and about
// once a minute while it runs, removed when it ends. When a printer is found
// printing the same file after a restart, the saved copy is taken back.

// RestoreMaxGap is how long (seconds) a saved session may have gone unseen and
// still be taken back. Longer than that the print is more likely a later run of
// the same file than the same print.
const RestoreMaxGap = 3600

// Resumable reports whether a saved session is the print that is running now:
// the same file, seen recently, and not further along than the printer says
// (progress only goes forward within one print).
func Resumable(savedFile string, savedSeen int64, savedPercent int, file string, percent int, now int64) bool {
	return file != "" && savedFile == file && now-savedSeen <= RestoreMaxGap && percent >= savedPercent
}

var sessMu sync.Mutex

func sessionsPath() string {
	return filepath.Join(config.ConfigDir(), "sessions.json")
}

// readSessions returns the saved sessions by key. A missing or unreadable file
// is no sessions: a print that cannot be taken back just starts over.
func readSessions() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if data, err := os.ReadFile(sessionsPath()); err == nil {
		if json.Unmarshal(data, &out) != nil || out == nil {
			out = map[string]json.RawMessage{}
		}
	}
	return out
}

// writeSessions replaces the file (temp file, then rename, so a crash never
// leaves half a file) or removes it when nothing is left.
func writeSessions(m map[string]json.RawMessage) error {
	p := sessionsPath()
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return config.WriteFileAtomic(p, data, 0600)
}

// SaveSession keeps v as the active session for key (a printer), replacing the
// one before.
func SaveSession(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	m := readSessions()
	m[key] = raw
	return writeSessions(m)
}

// LoadSession fills v from the session saved under key and reports whether there
// was one.
func LoadSession(key string, v any) bool {
	sessMu.Lock()
	defer sessMu.Unlock()
	raw, ok := readSessions()[key]
	return ok && json.Unmarshal(raw, v) == nil
}

// DeleteSession forgets the session saved under key. No session is no error.
func DeleteSession(key string) error {
	sessMu.Lock()
	defer sessMu.Unlock()
	m := readSessions()
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return writeSessions(m)
}
