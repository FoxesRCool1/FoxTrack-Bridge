package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/notify"
)

// Notifications: the dispatcher that turns events into messages, and the
// /api/notifications endpoints the dashboard's Notifications tab uses.

type notifyResult struct {
	Time  int64  `json:"time"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var (
	notifyMu   sync.Mutex
	notifyLast = map[string]notifyResult{} // by channel ID, in memory only
	// notifySeen: when each channel last got a message with this exact text,
	// so the same words never go out twice in notifyRepeatAfter.
	notifySeen = map[string]time.Time{}

	// Swapped by tests.
	notifySnapshot = printerSnapshot
	notifySend     = notify.Send
)

func init() {
	events.Subscribe(dispatchNotification)
	http.HandleFunc("/api/notifications", handleNotifications)
	http.HandleFunc("/api/notifications/test", handleNotificationTest)
}

func channelName(ch config.NotifyChannel) string {
	if ch.Name != "" {
		return ch.Name
	}
	for _, s := range notify.Services {
		if s.Kind == ch.Kind {
			return s.Name
		}
	}
	return ch.Kind
}

func recordNotify(id string, err error) {
	r := notifyResult{Time: time.Now().Unix(), OK: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	notifyMu.Lock()
	notifyLast[id] = r
	notifyMu.Unlock()
}

// notifyRepeatAfter is how soon the same text may go to the same channel again.
const notifyRepeatAfter = 2 * time.Minute

// firstSend reports whether this channel has not had this text in the last
// notifyRepeatAfter, and notes it as sent. The test button always goes.
func firstSend(ch config.NotifyChannel, m notify.Message, now time.Time) bool {
	if m.Event.Kind == events.Test {
		return true
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	for k, t := range notifySeen {
		if now.Sub(t) >= notifyRepeatAfter {
			delete(notifySeen, k)
		}
	}
	key := ch.ID + "\x00" + m.Plain()
	if _, ok := notifySeen[key]; ok {
		return false
	}
	notifySeen[key] = now
	return true
}

// forgetSent drops the note firstSend made, so a send that failed does not
// suppress the next identical message. The test button is never recorded.
func forgetSent(ch config.NotifyChannel, m notify.Message) {
	if m.Event.Kind == events.Test {
		return
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	delete(notifySeen, ch.ID+"\x00"+m.Plain())
}

// dispatchNotification sends one event to every channel that wants it. The
// camera is read once, only if a matching channel can use the picture, and
// shared by all of them.
func dispatchNotification(e events.Event) {
	configMutex.RLock()
	n := configStore.Notifications // replaced, never edited in place
	configMutex.RUnlock()
	if n == nil || len(n.Channels) == 0 {
		return
	}
	if !e.Kind.Urgent() && notify.Quiet(n, time.Now()) {
		return
	}
	var chans []config.NotifyChannel
	wantPicture := false
	for _, ch := range n.Channels {
		if notify.Wants(ch, e) {
			chans = append(chans, ch)
			wantPicture = wantPicture || (ch.Snapshot && notify.Picture(ch.Kind))
		}
	}
	if len(chans) == 0 {
		return
	}
	msg := notify.Compose(e)
	// A printer that stopped reporting has no camera worth waiting for.
	if wantPicture && e.PrinterID != "" && e.Kind != events.PrinterOffline {
		if p, ok := printerByID(e.PrinterID); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			b, err := notifySnapshot(ctx, p)
			cancel()
			if err != nil {
				log.Printf("[notify] no picture from %s: %v", p.Name, err)
			} else {
				msg.Snapshot = b
			}
		}
	}
	var wg sync.WaitGroup
	for _, ch := range chans {
		if !firstSend(ch, msg, time.Now()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			err := notifySend(ctx, ch, msg)
			if err != nil {
				log.Printf("[notify] %s: %v", channelName(ch), err)
				forgetSent(ch, msg)
			}
			recordNotify(ch.ID, err)
		}()
	}
	wg.Wait()
}

// ---- views ----

type notifySMTPView struct {
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	Username    string `json:"username,omitempty"`
	PasswordSet bool   `json:"password_set"`
	From        string `json:"from,omitempty"`
	Security    string `json:"security,omitempty"`
}

type notifyChannelView struct {
	ID       string          `json:"id"`
	Name     string          `json:"name,omitempty"`
	Kind     string          `json:"kind"`
	Enabled  bool            `json:"enabled"`
	URL      string          `json:"url,omitempty"`      // not for Discord and Slack, whose URL is the secret
	URLSet   bool            `json:"url_set,omitempty"`  // ... they get this and a hint
	URLHint  string          `json:"url_hint,omitempty"` // e.g. "discord.com/…"
	TokenSet bool            `json:"token_set"`
	Target   string          `json:"target,omitempty"`
	SMTP     *notifySMTPView `json:"smtp,omitempty"`
	Events   []string        `json:"events"` // what it sends now (the default set when none was chosen)
	Printers []string        `json:"printers"`
	Snapshot bool            `json:"snapshot"`
	Last     *notifyResult   `json:"last,omitempty"`
}

type notifyEventInfo struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

type notifyView struct {
	Channels          []notifyChannelView `json:"channels"`
	QuietStart        string              `json:"quiet_start"`
	QuietEnd          string              `json:"quiet_end"`
	AlmostDoneMinutes int                 `json:"almost_done_minutes"`
	Services          []notify.Service    `json:"services"`
	Events            []notifyEventInfo   `json:"events"`
	FoxTrackAlerts    map[string][]string `json:"foxtrack_alerts"`
}

func redactNotifications(n *config.Notifications) notifyView {
	v := notifyView{Channels: []notifyChannelView{}, Services: notify.Services}
	v.FoxTrackAlerts = foxtrackAlertsView(time.Now())
	isDefault := map[events.Kind]bool{}
	for _, k := range events.Default {
		isDefault[k] = true
	}
	for _, k := range events.All {
		v.Events = append(v.Events, notifyEventInfo{string(k), notify.Labels[k], isDefault[k]})
	}
	if n == nil {
		return v
	}
	v.QuietStart, v.QuietEnd = n.QuietStart, n.QuietEnd
	v.AlmostDoneMinutes = n.AlmostDoneMinutes
	notifyMu.Lock()
	defer notifyMu.Unlock()
	for _, ch := range n.Channels {
		cv := notifyChannelView{
			ID: ch.ID, Name: ch.Name, Kind: ch.Kind, Enabled: ch.Enabled, TokenSet: ch.Token != "",
			Target: ch.Target, Events: []string{}, Printers: append([]string{}, ch.Printers...), Snapshot: ch.Snapshot,
		}
		if notify.HasSecretURL(ch.Kind) {
			cv.URLSet = ch.URL != ""
			if u, err := url.Parse(ch.URL); err == nil && u.Host != "" {
				cv.URLHint = u.Host + "/…"
			}
		} else {
			cv.URL = ch.URL
		}
		if ch.SMTP != nil {
			cv.SMTP = &notifySMTPView{Host: ch.SMTP.Host, Port: ch.SMTP.Port, Username: ch.SMTP.Username, PasswordSet: ch.SMTP.Password != "", From: ch.SMTP.From, Security: ch.SMTP.Security}
		}
		if len(ch.Events) > 0 {
			cv.Events = append(cv.Events, ch.Events...)
		} else {
			for _, k := range events.Default {
				cv.Events = append(cv.Events, string(k))
			}
		}
		if r, ok := notifyLast[ch.ID]; ok {
			r := r
			cv.Last = &r
		}
		v.Channels = append(v.Channels, cv)
	}
	return v
}

// ---- saving ----

// notifyChannelReq is a channel as the dashboard sends it. The secrets are
// blank when the user left them alone; clear_* removes a stored one.
type notifyChannelReq struct {
	config.NotifyChannel
	ClearToken    bool `json:"clear_token"`
	ClearPassword bool `json:"clear_password"`
}

type notifyReq struct {
	Channels          *[]notifyChannelReq `json:"channels"` // absent keeps the stored list
	QuietStart        *string             `json:"quiet_start"`
	QuietEnd          *string             `json:"quiet_end"`
	AlmostDoneMinutes *int                `json:"almost_done_minutes"`
	ConfirmClear      bool                `json:"confirm_clear_channels"`
}

// badNotify is a mistake in the request, shown to the user as it is.
type badNotify string

func (e badNotify) Error() string { return string(e) }

var channelID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func newChannelID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "ch_" + hex.EncodeToString(b)
}

func trimAll(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// fillChannel completes a channel from the request: a blank secret keeps the
// stored one with the same ID, but only while it still goes to the same place
// (a new topic URL or mail host must be given its token or password again,
// so a stored secret is never sent to an address the user just typed).
func fillChannel(in notifyChannelReq, old *config.NotifyChannel) config.NotifyChannel {
	ch := in.NotifyChannel
	ch.ID, ch.Name = strings.TrimSpace(ch.ID), strings.TrimSpace(ch.Name)
	ch.URL, ch.Token, ch.Target = strings.TrimSpace(ch.URL), strings.TrimSpace(ch.Token), strings.TrimSpace(ch.Target)
	ch.Events, ch.Printers = trimAll(ch.Events), trimAll(ch.Printers)
	if !notify.Picture(ch.Kind) {
		ch.Snapshot = false
	}
	if ch.Kind != "email" {
		ch.SMTP = nil
	} else if ch.SMTP != nil {
		s := *ch.SMTP
		s.Host, s.Username, s.From = strings.TrimSpace(s.Host), strings.TrimSpace(s.Username), strings.TrimSpace(s.From)
		ch.SMTP = &s
	}
	switch ch.Kind {
	case "telegram", "pushover", "ntfy", "gotify":
	default:
		ch.Token = "" // this service has no token
	}
	if old == nil || old.Kind != ch.Kind {
		return ch
	}
	if notify.HasSecretURL(ch.Kind) && ch.URL == "" {
		ch.URL = old.URL
	}
	sameURL := ch.URL == old.URL
	if ch.Token == "" && !in.ClearToken && (sameURL || (ch.Kind != "ntfy" && ch.Kind != "gotify")) {
		ch.Token = old.Token
	}
	if ch.SMTP != nil && old.SMTP != nil && ch.SMTP.Password == "" && !in.ClearPassword &&
		ch.SMTP.Host == old.SMTP.Host && ch.SMTP.Username == old.SMTP.Username {
		ch.SMTP.Password = old.SMTP.Password
	}
	return ch
}

func findChannel(list []config.NotifyChannel, id string) *config.NotifyChannel {
	for i := range list {
		if list[i].ID == id && id != "" {
			return &list[i]
		}
	}
	return nil
}

const maxChannels = 20

// applyNotifications builds the new settings from the request. It runs under
// mutateConfig's lock and replaces the block rather than editing it.
func applyNotifications(c *config.Config, req notifyReq) error {
	next := &config.Notifications{}
	var stored []config.NotifyChannel
	if old := c.Notifications; old != nil {
		*next = *old
		stored = old.Channels
	}
	if req.QuietStart != nil {
		next.QuietStart = strings.TrimSpace(*req.QuietStart)
	}
	if req.QuietEnd != nil {
		next.QuietEnd = strings.TrimSpace(*req.QuietEnd)
	}
	if req.AlmostDoneMinutes != nil {
		m := *req.AlmostDoneMinutes
		if m < 1 {
			m = 0 // a cleared field: back to the default of 10
		}
		if m > 120 {
			m = 120
		}
		next.AlmostDoneMinutes = m
	}
	if !notify.ValidClock(next.QuietStart) || !notify.ValidClock(next.QuietEnd) {
		return badNotify("Quiet hours need times like 22:00 and 07:00.")
	}
	if (next.QuietStart == "") != (next.QuietEnd == "") {
		return badNotify("Set both quiet hours times, or neither.")
	}
	if req.Channels != nil {
		in := *req.Channels
		if len(in) > maxChannels {
			return badNotify(fmt.Sprintf("The Bridge keeps up to %d channels.", maxChannels))
		}
		// The same guard as printers: an empty list never replaces a full one by accident.
		if len(in) == 0 && len(stored) > 0 && !req.ConfirmClear {
			return badNotify("That would remove every notification channel. Reload the page and try again.")
		}
		out := make([]config.NotifyChannel, 0, len(in))
		seen := map[string]bool{}
		for i, r := range in {
			ch := fillChannel(r, findChannel(stored, strings.TrimSpace(r.ID)))
			if !channelID.MatchString(ch.ID) {
				ch.ID = newChannelID()
			}
			if seen[ch.ID] {
				return badNotify(fmt.Sprintf("Channel %d has the same ID as another one.", i+1))
			}
			seen[ch.ID] = true
			if len([]rune(ch.Name)) > 60 {
				return badNotify("A channel name can be up to 60 characters.")
			}
			if err := notify.Validate(ch); err != nil {
				who := channelName(ch)
				return badNotify(who + ": " + err.Error() + ".")
			}
			out = append(out, ch)
		}
		next.Channels = out
	}
	c.Notifications = next
	return nil
}

func handleNotifications(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		n := configStore.Notifications
		configMutex.RUnlock()
		json.NewEncoder(w).Encode(redactNotifications(n))
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		var req notifyReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "That is not valid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		err := mutateConfig(func(c *config.Config) error { return applyNotifications(c, req) })
		var bad badNotify
		if errors.As(err, &bad) {
			http.Error(w, bad.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		configMutex.RLock()
		n := configStore.Notifications
		configMutex.RUnlock()
		json.NewEncoder(w).Encode(redactNotifications(n))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- test message ----

// firstSnapshot reads a picture from the first printer whose camera answers.
func firstSnapshot(ctx context.Context) []byte {
	configMutex.RLock()
	printers := append([]config.Printer(nil), configStore.Printers...)
	configMutex.RUnlock()
	for _, p := range printers {
		if p.CameraHidden {
			continue
		}
		if b, err := notifySnapshot(ctx, p); err == nil && len(b) > 0 {
			return b
		}
	}
	return nil
}

func handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "POST":
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var req struct {
		ChannelID string            `json:"channel_id"`
		Channel   *notifyChannelReq `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "That is not valid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	reply := func(err error) {
		out := map[string]any{"ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		json.NewEncoder(w).Encode(out)
	}

	id := req.ChannelID
	if req.Channel != nil {
		id = strings.TrimSpace(req.Channel.ID)
	}
	configMutex.RLock()
	var stored *config.NotifyChannel
	if n := configStore.Notifications; n != nil {
		if s := findChannel(n.Channels, id); s != nil {
			cp := *s
			stored = &cp
		}
	}
	configMutex.RUnlock()

	var ch config.NotifyChannel
	switch {
	case req.Channel != nil:
		ch = fillChannel(*req.Channel, stored)
	case stored != nil:
		ch = *stored
	default:
		http.Error(w, "There is no channel with that ID. Reload the page.", http.StatusNotFound)
		return
	}
	if err := notify.Validate(ch); err != nil {
		reply(err)
		return
	}

	msg := notify.Compose(events.Event{Kind: events.Test, Time: time.Now(), Progress: -1})
	if ch.Snapshot && notify.Picture(ch.Kind) {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		msg.Snapshot = firstSnapshot(ctx)
		cancel()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	err := notifySend(ctx, ch, msg)
	if req.Channel == nil && stored != nil { // an edited, unsaved form says nothing about the saved channel
		recordNotify(stored.ID, err)
	}
	if err != nil {
		log.Printf("[notify] %s: %v", channelName(ch), err)
	}
	reply(err)
}
