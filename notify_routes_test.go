//go:build headless

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/notify"
)

func seedNotifications(t *testing.T, n *config.Notifications) {
	t.Helper()
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Fox"}, {ID: "p2", Name: "Wolf"}}, Notifications: n}
	configMutex.Unlock()
	notifyMu.Lock()
	notifyLast = map[string]notifyResult{}
	notifyMu.Unlock()
}

func storedChannels() []config.NotifyChannel {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.Notifications == nil {
		return nil
	}
	return configStore.Notifications.Channels
}

func notifyCall(t *testing.T, h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if method == "GET" {
		req = httptest.NewRequest("GET", path, nil)
	} else {
		req = jsonPost(path, body)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func secretChannels() *config.Notifications {
	return &config.Notifications{Channels: []config.NotifyChannel{
		{ID: "d1", Name: "Family", Kind: "discord", Enabled: true, URL: "https://discord.com/api/webhooks/1/HOOKSECRET"},
		{ID: "t1", Kind: "telegram", Enabled: true, Token: "TGSECRET", Target: "42"},
		{ID: "e1", Kind: "email", Enabled: true, Target: "a@b.c", SMTP: &config.SMTP{Host: "smtp.x", Username: "u", Password: "PWSECRET", From: "a@b.c"}},
		{ID: "n1", Kind: "ntfy", Enabled: true, URL: "https://ntfy.sh/topic", Token: "NTFYSECRET"},
	}}
}

func TestNotificationsGETRedactsSecrets(t *testing.T) {
	seedNotifications(t, secretChannels())
	recordNotify("t1", errors.New("Telegram refused the bot token"))
	rec := notifyCall(t, handleNotifications, "GET", "/api/notifications", "")
	body := rec.Body.String()
	for _, s := range []string{"HOOKSECRET", "TGSECRET", "PWSECRET", "NTFYSECRET"} {
		if strings.Contains(body, s) {
			t.Fatalf("GET leaked %s: %s", s, body)
		}
	}
	var v notifyView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Channels) != 4 || !v.Channels[0].URLSet || v.Channels[0].URLHint != "discord.com/…" || !v.Channels[1].TokenSet ||
		!v.Channels[2].SMTP.PasswordSet || !v.Channels[3].TokenSet || v.Channels[3].URL != "https://ntfy.sh/topic" {
		t.Fatalf("view: %+v", v.Channels)
	}
	if l := v.Channels[1].Last; l == nil || l.OK || l.Error != "Telegram refused the bot token" {
		t.Fatalf("last result: %+v", l)
	}
	if len(v.Channels[0].Events) != len(events.Default) {
		t.Fatalf("unset events should show the default set: %v", v.Channels[0].Events)
	}
	if len(v.Events) != len(events.All) || len(v.Services) != 8 {
		t.Fatalf("lists: %d events, %d services", len(v.Events), len(v.Services))
	}
}

func TestNotificationsEmptyGET(t *testing.T) {
	seedNotifications(t, nil)
	if body := notifyCall(t, handleNotifications, "GET", "/api/notifications", "").Body.String(); !strings.Contains(body, `"channels":[]`) {
		t.Fatalf("an unset block still lists channels as []: %s", body)
	}
}

func TestNotificationsPOSTKeepsBlankSecrets(t *testing.T) {
	seedNotifications(t, secretChannels())
	body := `{"channels":[
		{"id":"d1","name":"Family 2","kind":"discord","enabled":true},
		{"id":"t1","kind":"telegram","enabled":true,"target":"43"},
		{"id":"e1","kind":"email","enabled":true,"target":"a@b.c","smtp":{"host":"smtp.x","username":"u","from":"a@b.c"}},
		{"id":"n1","kind":"ntfy","enabled":true,"url":"https://ntfy.sh/topic"}]}`
	rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ch := storedChannels()
	if ch[0].URL != "https://discord.com/api/webhooks/1/HOOKSECRET" || ch[0].Name != "Family 2" || ch[1].Token != "TGSECRET" || ch[1].Target != "43" ||
		ch[2].SMTP.Password != "PWSECRET" || ch[3].Token != "NTFYSECRET" {
		t.Fatalf("blank secrets must keep the stored ones: %+v", ch)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("POST reply leaked a secret: %s", rec.Body)
	}
}

func TestNotificationsPOSTReplacesAndClearsSecrets(t *testing.T) {
	seedNotifications(t, secretChannels())
	body := `{"channels":[
		{"id":"d1","kind":"discord","enabled":true,"url":"https://discord.com/api/webhooks/2/NEW"},
		{"id":"t1","kind":"telegram","enabled":true,"token":"TGNEW","target":"42"},
		{"id":"e1","kind":"email","enabled":true,"target":"a@b.c","clear_password":true,"smtp":{"host":"smtp.x","username":"u","from":"a@b.c"}},
		{"id":"n1","kind":"ntfy","enabled":true,"url":"https://ntfy.sh/topic","clear_token":true}]}`
	if rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", body); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ch := storedChannels()
	if ch[0].URL != "https://discord.com/api/webhooks/2/NEW" || ch[1].Token != "TGNEW" || ch[2].SMTP.Password != "" || ch[3].Token != "" {
		t.Fatalf("%+v", ch)
	}
}

// A stored token is never sent to an address the user just typed.
func TestNotificationsSecretStaysWithItsEndpoint(t *testing.T) {
	seedNotifications(t, secretChannels())
	body := `{"channels":[
		{"id":"n1","kind":"ntfy","enabled":true,"url":"https://elsewhere.example/topic"},
		{"id":"e1","kind":"email","enabled":true,"target":"a@b.c","smtp":{"host":"other.host","username":"u","from":"a@b.c"}}]}`
	if rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", body); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ch := storedChannels()
	if ch[0].Token != "" || ch[1].SMTP.Password != "" {
		t.Fatalf("secrets followed a changed endpoint: %+v %+v", ch[0], ch[1].SMTP)
	}
}

func TestNotificationsPOSTNewChannelGetsAnID(t *testing.T) {
	seedNotifications(t, nil)
	body := `{"channels":[{"kind":"gotify","enabled":true,"url":"http://10.0.0.5:8080","token":"abc","snapshot":true,"events":["print_finished","print_finished"," "]},
		{"kind":"discord","enabled":true,"snapshot":true,"url":"https://discord.com/api/webhooks/1/x"}],"quiet_start":"22:00","quiet_end":"07:00"}`
	rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", body)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ch := storedChannels()
	if len(ch) != 2 || ch[0].ID == "" || ch[1].ID == "" || ch[0].ID == ch[1].ID {
		t.Fatalf("ids: %+v", ch)
	}
	if ch[0].Snapshot || !ch[1].Snapshot || len(ch[0].Events) != 1 {
		t.Fatalf("gotify cannot carry a picture and events are de-duplicated: %+v", ch)
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.Notifications.QuietStart != "22:00" || len(configStore.Printers) != 2 {
		t.Fatalf("quiet hours / printers: %+v", configStore)
	}
}

func TestNotificationsPOSTValidation(t *testing.T) {
	seedNotifications(t, secretChannels())
	for name, body := range map[string]string{
		"unknown kind":   `{"channels":[{"kind":"fax","enabled":true}]}`,
		"bad url scheme": `{"channels":[{"kind":"slack","enabled":true,"url":"ftp://x"}]}`,
		"bad event":      `{"channels":[{"kind":"ntfy","url":"https://ntfy.sh/x","events":["nope"]}]}`,
		"missing token":  `{"channels":[{"kind":"telegram","target":"1"}]}`,
		"bad quiet time": `{"quiet_start":"25:00","quiet_end":"07:00"}`,
		"one quiet time": `{"quiet_start":"22:00","quiet_end":""}`,
		"duplicate ids":  `{"channels":[{"id":"a","kind":"ntfy","url":"https://x/y"},{"id":"a","kind":"ntfy","url":"https://x/z"}]}`,
		"emptying":       `{"channels":[]}`,
		"bad email to":   `{"channels":[{"kind":"email","target":"nope","smtp":{"host":"h","from":"a@b.c"}}]}`,
	} {
		rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", body)
		if rec.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
	if len(storedChannels()) != 4 {
		t.Fatal("a refused save must change nothing")
	}
	if rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", `{"channels":[],"confirm_clear_channels":true}`); rec.Code != 200 || len(storedChannels()) != 0 {
		t.Fatalf("an explicit clear works: %d %s", rec.Code, rec.Body)
	}
}

func TestNotificationsPOSTWithoutChannelsKeepsThem(t *testing.T) {
	seedNotifications(t, secretChannels())
	if rec := notifyCall(t, handleNotifications, "POST", "/api/notifications", `{"quiet_start":"23:00","quiet_end":"06:00"}`); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(storedChannels()) != 4 {
		t.Fatal("a body without channels is a partial update")
	}
}

func TestNotificationsRefuseNonJSON(t *testing.T) {
	seedNotifications(t, nil)
	req := httptest.NewRequest("POST", "/api/notifications", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handleNotifications(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d", rec.Code)
	}
}

// stubSend replaces the sender for a test and returns what it was given.
func stubSend(t *testing.T, fn func(ch config.NotifyChannel, m notify.Message) error) {
	t.Helper()
	old := notifySend
	notifySend = func(_ context.Context, ch config.NotifyChannel, m notify.Message) error { return fn(ch, m) }
	t.Cleanup(func() { notifySend = old })
}

func stubSnapshot(t *testing.T, fn func(p config.Printer) ([]byte, error)) {
	t.Helper()
	old := notifySnapshot
	notifySnapshot = func(_ context.Context, p config.Printer) ([]byte, error) { return fn(p) }
	t.Cleanup(func() { notifySnapshot = old })
}

func TestNotificationTest(t *testing.T) {
	seedNotifications(t, secretChannels())
	var mu sync.Mutex
	var got config.NotifyChannel
	var msg notify.Message
	var sendErr error
	stubSend(t, func(ch config.NotifyChannel, m notify.Message) error {
		mu.Lock()
		defer mu.Unlock()
		got, msg = ch, m
		return sendErr
	})
	stubSnapshot(t, func(p config.Printer) ([]byte, error) {
		if p.ID == "p1" {
			return nil, errNoCamera // the second printer is the first with a camera
		}
		return []byte("jpeg"), nil
	})

	rec := notifyCall(t, handleNotificationTest, "POST", "/api/notifications/test", `{"channel_id":"t1"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) || got.Token != "TGSECRET" || msg.Event.Kind != events.Test || msg.Snapshot != nil {
		t.Fatalf("by id: %d %s %+v", rec.Code, rec.Body, got)
	}
	notifyMu.Lock()
	r := notifyLast["t1"]
	notifyMu.Unlock()
	if !r.OK || r.Time == 0 {
		t.Fatalf("the result is kept: %+v", r)
	}

	// The unsaved form, with a blank token: the stored one fills in. Picture on: one snapshot.
	rec = notifyCall(t, handleNotificationTest, "POST", "/api/notifications/test", `{"channel":{"id":"d1","kind":"discord","enabled":true,"snapshot":true}}`)
	if !strings.Contains(rec.Body.String(), `"ok":true`) || got.URL != "https://discord.com/api/webhooks/1/HOOKSECRET" || string(msg.Snapshot) != "jpeg" {
		t.Fatalf("unsaved form: %s %+v snap %q", rec.Body, got, msg.Snapshot)
	}

	// A form for a channel that was never saved has no stored secret to fall back on.
	rec = notifyCall(t, handleNotificationTest, "POST", "/api/notifications/test", `{"channel":{"kind":"telegram","target":"1"}}`)
	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "bot token") {
		t.Fatalf("validation: %s", rec.Body)
	}

	sendErr = errors.New("Telegram refused the bot token")
	rec = notifyCall(t, handleNotificationTest, "POST", "/api/notifications/test", `{"channel_id":"t1"}`)
	var out struct {
		OK    bool
		Error string
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || out.Error != "Telegram refused the bot token" {
		t.Fatalf("error text is returned: %s", rec.Body)
	}

	if rec = notifyCall(t, handleNotificationTest, "POST", "/api/notifications/test", `{"channel_id":"zzz"}`); rec.Code != 404 {
		t.Fatalf("unknown id: %d", rec.Code)
	}
}

func TestDispatchFiltersQuietHoursAndSharesOneSnapshot(t *testing.T) {
	var snaps atomic.Int32
	stubSnapshot(t, func(p config.Printer) ([]byte, error) { snaps.Add(1); return []byte("jpeg"), nil })
	var mu sync.Mutex
	sent := map[string]notify.Message{}
	stubSend(t, func(ch config.NotifyChannel, m notify.Message) error {
		mu.Lock()
		sent[ch.ID] = m
		mu.Unlock()
		if ch.ID == "bad" {
			return errors.New("boom")
		}
		return nil
	})
	set := func(n *config.Notifications) {
		seedNotifications(t, n)
		snaps.Store(0)
		sent = map[string]notify.Message{}
	}
	wc := func(id string) config.NotifyChannel {
		return config.NotifyChannel{ID: id, Kind: "webhook", URL: "http://x", Enabled: true, Snapshot: true}
	}

	// Two picture channels, one off, one for the other printer, one that cannot carry a picture.
	off, other, slack := wc("off"), wc("other"), config.NotifyChannel{ID: "slack", Kind: "slack", URL: "http://x", Enabled: true, Snapshot: true}
	off.Enabled = false
	other.Printers = []string{"p2"}
	set(&config.Notifications{Channels: []config.NotifyChannel{wc("a"), wc("bad"), off, other, slack}})
	dispatchNotification(events.Event{Kind: events.PrintFinished, PrinterID: "p1", Printer: "Fox"})
	if len(sent) != 3 || sent["a"].Snapshot == nil || sent["bad"].Snapshot == nil || snaps.Load() != 1 {
		t.Fatalf("sent to %d channels with %d snapshots", len(sent), snaps.Load())
	}
	notifyMu.Lock()
	okA, bad := notifyLast["a"], notifyLast["bad"]
	notifyMu.Unlock()
	if !okA.OK || bad.OK || bad.Error != "boom" {
		t.Fatalf("results: %+v %+v", okA, bad)
	}

	// Only a channel that cannot use a picture matches: the camera is not touched.
	set(&config.Notifications{Channels: []config.NotifyChannel{slack}})
	dispatchNotification(events.Event{Kind: events.PrintFinished, PrinterID: "p1"})
	if len(sent) != 1 || snaps.Load() != 0 {
		t.Fatalf("no picture wanted: sent %d, snapshots %d", len(sent), snaps.Load())
	}

	// A lost printer is not worth waiting on a camera for.
	a := wc("a")
	a.Events = []string{"printer_offline"}
	set(&config.Notifications{Channels: []config.NotifyChannel{a}})
	dispatchNotification(events.Event{Kind: events.PrinterOffline, PrinterID: "p1"})
	if len(sent) != 1 || snaps.Load() != 0 {
		t.Fatalf("offline: sent %d, snapshots %d", len(sent), snaps.Load())
	}

	// Quiet hours hold back a finished print but not an alarm.
	now := time.Now()
	q := &config.Notifications{Channels: []config.NotifyChannel{wc("a")},
		QuietStart: now.Add(-time.Hour).Format("15:04"), QuietEnd: now.Add(time.Hour).Format("15:04")}
	set(q)
	dispatchNotification(events.Event{Kind: events.PrintFinished, PrinterID: "p1"})
	if len(sent) != 0 {
		t.Fatal("quiet hours must hold back a finished print")
	}
	dispatchNotification(events.Event{Kind: events.PrinterError, PrinterID: "p1"})
	if len(sent) != 1 {
		t.Fatal("an alarm passes quiet hours")
	}
}
