package main

import (
	"log"
	"sync"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/version"
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
	plugRelayLast = map[string]plugRelaySent{} // printer name -> last plug-only report
)

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
		plugRelayMu.Unlock()
		switch {
		case pl.On && !(sent && !last.on):
			continue // on, and FoxTrack was not told it was off: nothing to correct
		case sent && last.on == pl.On && now.Sub(last.at) < plugRelayHeartbeat:
			continue
		}
		if legacyKey == "" && v2Key == "" {
			continue
		}
		payload := plugOnlyPayload(p, pl)
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
			}
		}
		if legacyKey != "" {
			id := p.Name
			if isBambuPrinterConfig(p) {
				id = p.Serial
			}
			if err := sendPlugRelay(legacyKey, webhook.URL, id, p.Name, payload); err != nil {
				log.Printf("[%s] plug report (legacy): %v", p.Name, err)
			}
		}
		plugRelayMu.Lock()
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
	plugRelayMu.Unlock()
}

// printerSpeaking reports a printer that sent a real report recently, so its
// own relay carries the plug.
func printerSpeaking(name string, now time.Time) bool {
	s, ok := printerLiveState(name)
	return ok && s != nil && now.Unix()-s.Timestamp < int64(plugRelayQuiet/time.Second)
}

func plugOnlyPayload(p config.Printer, pl *webhook.RelayPlug) webhook.RelayPayload {
	kind := p.Kind()
	model := ""
	if kind == config.KindBambu {
		model = mqttpkg.ModelFromSerial(p.Serial)
	}
	return webhook.RelayPayload{Print: webhook.RelayPrint{
		GcodeState: "OFFLINE",
		// No Alarms: the key is left out, which FoxTrack reads as no change.
		// This report says nothing about the printer's alarms, and [] would
		// clear the ones it reported before its power went. The remaining time
		// is unknown.
		McRemainingTime:    -1,
		Plug:               pl,
		PrinterKind:        kind,
		PrinterModel:       model,
		BridgeVersion:      version.AppVersion,
		BridgeCapabilities: mqttpkg.WithPlugCaps(nil, true),
	}}
}
