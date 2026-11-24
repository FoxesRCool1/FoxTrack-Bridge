package main

import (
	"log"
	"sync"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// A printer's plug state reaches FoxTrack on the printer's own reports. A
// printer its plug has switched off sends none, so FoxTrack would show the
// plug as it was and, 90 seconds on, the whole printer as offline with every
// button off: there was no way to switch it back on from FoxTrack. While a
// printer is quiet and its plug is off, the Bridge reports the plug itself,
// with gcode_state OFFLINE (the printer has no power) and only the plug
// commands. A plug that is on says nothing about the printer, so it is sent
// once, right after it was switched on, and the printer's own reports take
// over when it boots.

const (
	plugRelayTick      = 10 * time.Second // the plug poller's pace
	plugRelayHeartbeat = 60 * time.Second // under FoxTrack's 90 s online window
	// A report newer than this means the printer speaks for itself.
	plugRelayQuiet = 90 * time.Second
)

// sendPlugRelay is webhook.SendRelay; tests swap it.
var sendPlugRelay = webhook.SendRelay

var (
	plugRelayMu   sync.Mutex
	plugRelayLast = map[string]plugRelaySent{} // printer name -> last plug-only report FoxTrack took
	// printer name -> when a report that reached no project may be tried again
	plugRelayRetry = map[string]time.Time{}
)

// plugRelayRetryGap spaces the tries of a report FoxTrack did not take: half
// a heartbeat, so a FoxTrack that is down is not sent one per poller tick.
const plugRelayRetryGap = plugRelayHeartbeat / 2

type plugRelaySent struct {
	on bool
	at time.Time
}

func startPlugRelay() {
	go func() {
		t := time.NewTicker(plugRelayTick)
		defer t.Stop()
		for range t.C {
			relayQuietPlugs(time.Now())
		}
	}()
}

// relayQuietPlugs sends the plug-only report for every quiet printer whose
// plug is off, or was off in the last one sent.
func relayQuietPlugs(now time.Time) {
	configMutex.RLock()
	legacyKey, v2Key := configStore.APIKey, configStore.FoxTrack2APIKey
	printers := append([]config.Printer(nil), configStore.Printers...)
	configMutex.RUnlock()

	keep := map[string]bool{}
	for _, p := range printers {
		if !p.HasPlug() || p.Kind() == config.KindHomeAssistant {
			continue
		}
		keep[p.Name] = true
		pl := plugs.Current(p.Name)
		if pl == nil || !pl.Reachable || printerSpeaking(p.Name, now) {
			continue
		}
		plugRelayMu.Lock()
		last, sent := plugRelayLast[p.Name]
		retryAt := plugRelayRetry[p.Name]
		plugRelayMu.Unlock()
		switch {
		case now.Before(retryAt):
			continue
		case pl.On && !(sent && !last.on):
			continue // on, and FoxTrack was not told it was off: nothing to correct
		case sent && last.on == pl.On && now.Sub(last.at) < plugRelayHeartbeat:
			continue
		}
		if legacyKey == "" && v2Key == "" {
			continue
		}
		payload := plugOnlyPayload(p, pl)
		delivered := false
		// The current project keys a printer on its serial (Bambu) or config ID,
		// and gets the earlier names to re-key by. The legacy project keeps the
		// name for a non-Bambu printer and has no re-keying. The current
		// project goes first, so a dead legacy host never delays it.
		if v2Key != "" {
			id, legacyID := p.RelayIdentity()
			v2Payload := payload
			v2Payload.LegacyID = legacyID
			if err := sendPlugRelay(v2Key, webhook.RelayURLV2, id, p.Name, v2Payload); err != nil {
				log.Printf("[%s] plug report: %v", p.Name, err)
			} else {
				delivered = true
			}
		}
		if legacyKey != "" {
			id := p.Name
			if isBambuPrinterConfig(p) {
				id = p.Serial
			}
			if err := sendPlugRelay(legacyKey, webhook.URL, id, p.Name, payload); err != nil {
				log.Printf("[%s] plug report (legacy): %v", p.Name, err)
			} else {
				delivered = true
			}
		}
		plugRelayMu.Lock()
		if !delivered {
			// FoxTrack still has the old state: keep what it was last told, so
			// this report (the one "on" after a switch too) is tried again.
			plugRelayRetry[p.Name] = now.Add(plugRelayRetryGap)
			plugRelayMu.Unlock()
			continue
		}
		delete(plugRelayRetry, p.Name)
		if pl.On {
			delete(plugRelayLast, p.Name) // told once; the printer's reports take over
		} else {
			plugRelayLast[p.Name] = plugRelaySent{on: false, at: now}
		}
		plugRelayMu.Unlock()
	}
	plugRelayMu.Lock()
	for name := range plugRelayLast {
		if !keep[name] {
			delete(plugRelayLast, name)
		}
	}
	for name := range plugRelayRetry {
		if !keep[name] {
			delete(plugRelayRetry, name)
		}
	}
	plugRelayMu.Unlock()
}

// printerSpeaking reports a printer that sent a real report recently, so its
// own relay carries the plug.
func printerSpeaking(name string, now time.Time) bool {
	s, ok := printerLiveState(name)
	return ok && s != nil && now.Unix()-s.Timestamp < int64(plugRelayQuiet/time.Second)
}

// plugOnlyPayload is the OFFLINE report of a printer whose plug cut its
// power. FoxTrack stores each report as sent, and the printer's own offline
// chain (mqtt reportOffline, the lan poller's failed poll) reports it too, so
// both must post the same body: the last real report carried over (file,
// progress, filament slots), not an empty one that would blank them on every
// other post.
func plugOnlyPayload(p config.Printer, pl *webhook.RelayPlug) webhook.RelayPayload {
	if isBambuPrinterConfig(p) {
		out := mqttpkg.OfflineRelayPayload(mqttpkg.Printer{Name: p.Name, Serial: p.Serial, Cloud: p.IsCloud()})
		if out.Print.Plug == nil { // the plug commands were not announced without it
			out.Print.BridgeCapabilities = mqttpkg.WithPlugCaps(out.Print.BridgeCapabilities, true)
		}
		out.Print.Plug = pl
		return out
	}
	kind := p.Kind()
	last := lanCtrl.LastReport(p.Name)
	model := ""
	if kind == config.KindBambu {
		model = mqttpkg.ModelFromSerial(p.Serial)
	} else if last != nil {
		model = last.Print.PrinterModel
	}
	// No Alarms: OfflinePayload leaves the key out, which FoxTrack reads as no
	// change. This report says nothing about the printer's alarms, and [] would
	// clear the ones it reported before its power went. The remaining time is
	// unknown.
	return mqttpkg.OfflinePayload(last, kind, model, pl)
}
