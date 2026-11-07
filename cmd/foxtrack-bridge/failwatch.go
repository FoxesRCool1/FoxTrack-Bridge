package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"strings"
	"time"

	"foxtrack-bridge/ai"
	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
)

// Camera failure watch (Settings, Assistant). Off unless the user turns it on,
// and only with "Let the assistant see the camera" on too, because every check
// sends a camera picture to the assistant's provider.
//
// While a printer prints, its camera is checked every few minutes. Two
// "failed" answers in a row send one print_failure_suspected notification for
// that print. The watch never pauses or stops anything: a model can be wrong,
// and a person decides.

const (
	failWatchTick     = 30 * time.Second
	failWatchWarmup   = 5 * time.Minute // the first layers say little in a picture
	failWatchStrikes  = 2
	failWatchDefault  = 5
	failWatchPrompt   = `You check 3D printer camera pictures for failed prints. Reply with JSON only, no other text: {"failed": true or false, "reason": "a few words"}. Answer failed=true only for a clear failure: spaghetti (loose strands of plastic everywhere), a part knocked loose or moved on the bed, or a blob of plastic around the nozzle. A normal print in progress, an empty bed, a dark or blurry picture, or anything you are unsure about is failed=false.`
	failWatchQuestion = "Has this print failed?"
)

type failWatchState struct {
	file      string
	since     time.Time
	lastCheck time.Time
	strikes   int
	alerted   bool
	lastErr   string
}

func init() {
	onStartup(func() { go failWatchLoop() })
}

func failWatchInterval(a config.AI) time.Duration {
	m := a.WatchEveryMin
	if m <= 0 {
		m = failWatchDefault
	}
	m = max(2, min(m, 60))
	return time.Duration(m) * time.Minute
}

func failWatchEnabled(a config.AI) bool {
	return a.WatchFailures && a.AllowCamera && a.Configured()
}

// failWatchRecord counts one answer and reports whether to alert now.
func failWatchRecord(st *failWatchState, failed bool) bool {
	if !failed {
		st.strikes = 0
		return false
	}
	st.strikes++
	if st.strikes >= failWatchStrikes && !st.alerted {
		st.alerted = true
		return true
	}
	return false
}

// parseFailWatchReply reads the model's JSON answer, tolerating code fences
// or words around it. Anything it cannot read counts as "not failed".
func parseFailWatchReply(s string) (bool, string) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return false, ""
	}
	var v struct {
		Failed bool   `json:"failed"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(s[i:j+1]), &v) != nil {
		return false, ""
	}
	r := strings.TrimSpace(v.Reason)
	if len([]rune(r)) > 200 {
		r = string([]rune(r)[:200])
	}
	return v.Failed, r
}

func failWatchAsk(ctx context.Context, a config.AI, jpg []byte) (bool, string, error) {
	client := ai.NewClient(ai.Settings{Preset: a.Preset, BaseURL: a.BaseURL, Model: a.Model, APIKey: a.APIKey, AllowCamera: true})
	reply, err := client.Chat(ctx, []ai.Message{
		ai.SystemMessage(failWatchPrompt),
		ai.UserMessageWithImage(failWatchQuestion, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(jpg)),
	}, nil, "")
	if err != nil {
		return false, "", err
	}
	failed, reason := parseFailWatchReply(reply.Content)
	return failed, reason, nil
}

func printerTelemetry(name string) *mqttpkg.TelemetryData {
	if t := mqttpkg.GetPrintersState()[name]; t != nil {
		return t
	}
	return lanCtrl.GetStates()[name]
}

func failWatchLoop() {
	states := map[string]*failWatchState{}
	for range time.Tick(failWatchTick) {
		a := aiSettings()
		if !failWatchEnabled(a) {
			clear(states)
			continue
		}
		configMutex.RLock()
		printers := append([]config.Printer(nil), configStore.Printers...)
		configMutex.RUnlock()
		seen := map[string]bool{}
		now := time.Now()
		for _, p := range printers {
			t := printerTelemetry(p.Name)
			if t != nil && t.Status == "paused" {
				seen[p.ID] = true // keep its state, so a resume does not alert twice
				continue
			}
			if t == nil || t.Status != "printing" || p.CameraHidden {
				continue
			}
			seen[p.ID] = true
			st := states[p.ID]
			if st == nil || st.file != t.FileName {
				st = &failWatchState{file: t.FileName, since: now}
				states[p.ID] = st
			}
			if now.Sub(st.since) < failWatchWarmup || now.Sub(st.lastCheck) < failWatchInterval(a) || st.alerted {
				continue
			}
			st.lastCheck = now
			failWatchCheck(a, p, t, st)
		}
		for id := range states {
			if !seen[id] {
				delete(states, id)
			}
		}
	}
}

func failWatchCheck(a config.AI, p config.Printer, t *mqttpkg.TelemetryData, st *failWatchState) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	jpg, err := printerSnapshot(ctx, p)
	if err == nil {
		var failed bool
		var reason string
		failed, reason, err = failWatchAsk(ctx, a, jpg)
		if err == nil {
			st.lastErr = ""
			if failWatchRecord(st, failed) {
				log.Printf("[failure-watch] %s: possible failure (%s)", p.Name, reason)
				events.Publish(events.Event{
					Kind: events.PrintFailureSuspected, PrinterID: p.ID, Printer: p.Name,
					File: t.FileName, Progress: int(t.Progress), Remaining: int(t.TimeRemaining),
					Message: reason,
				})
			}
			return
		}
	}
	// Log a failing check once, not every few minutes.
	if msg := err.Error(); msg != st.lastErr {
		st.lastErr = msg
		log.Printf("[failure-watch] %s: check skipped: %v", p.Name, err)
	}
}
