package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
)

// appAlerts holds the last app-alerts map FoxTrack sent, so the dashboard can
// show which channel kinds cover which events.
var appAlerts struct {
	mu sync.Mutex
	m  map[string][]string
	at time.Time
}

// appAlertsMaxAge is how long a stored map stays fresh.
const appAlertsMaxAge = 10 * time.Minute

// notifyHeader builds the X-Bridge-Notify header: compact JSON of channel kind
// to the sorted, de-duplicated FoxTrack event names the enabled channels cover.
// A channel with no events of its own uses events.Default. Kinds with no
// mapped events are left out, so an empty config gives "{}". It contains only
// kind and event names: no URL, target, token, name or ID.
func notifyHeader(n *config.Notifications) string {
	m := map[string][]string{}
	if n != nil {
		for _, ch := range n.Channels {
			if !ch.Enabled {
				continue
			}
			evs := ch.Events
			if len(evs) == 0 {
				evs = make([]string, len(events.Default))
				for i, k := range events.Default {
					evs[i] = string(k)
				}
			}
			for _, s := range evs {
				name, ok := events.FoxTrackName(events.Kind(s))
				if !ok {
					continue
				}
				m[ch.Kind] = append(m[ch.Kind], name)
			}
		}
	}
	for kind, evs := range m {
		sort.Strings(evs)
		m[kind] = dedupe(evs)
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(body)
}

// dedupe returns the sorted list with duplicates removed.
func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// storeAppAlerts keeps a copy of the app-alerts map FoxTrack sent, with the
// time it arrived. Callers pass only non-nil maps.
func storeAppAlerts(m map[string][]string, now time.Time) {
	copied := make(map[string][]string, len(m))
	for k, v := range m {
		copied[k] = append([]string(nil), v...)
	}
	appAlerts.mu.Lock()
	appAlerts.m = copied
	appAlerts.at = now
	appAlerts.mu.Unlock()
}

// foxtrackAlertsView maps the stored FoxTrack event names back to the Bridge
// event kind strings each channel kind covers, sorted and de-duplicated. It
// returns nil while nothing is stored or the stored map is older than
// appAlertsMaxAge; otherwise a non-nil map (possibly empty).
func foxtrackAlertsView(now time.Time) map[string][]string {
	appAlerts.mu.Lock()
	m, at := appAlerts.m, appAlerts.at
	appAlerts.mu.Unlock()
	if m == nil || now.Sub(at) > appAlertsMaxAge {
		return nil
	}
	out := map[string][]string{}
	for kind, names := range m {
		evs := []string{}
		for _, name := range names {
			k, ok := events.FromFoxTrackName(name)
			if !ok {
				continue
			}
			evs = append(evs, string(k))
		}
		if len(evs) == 0 {
			continue
		}
		sort.Strings(evs)
		out[kind] = dedupe(evs)
	}
	return out
}
