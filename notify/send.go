package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
)

// Service is one kind of destination, for the dashboard's picker.
type Service struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Picture bool   `json:"picture"` // can carry the camera picture
}

var Services = []Service{
	{"discord", "Discord", true},
	{"slack", "Slack", false},
	{"telegram", "Telegram", true},
	{"ntfy", "ntfy", true},
	{"pushover", "Pushover", true},
	{"gotify", "Gotify", false},
	{"email", "Email", true},
	{"webhook", "Webhook", true},
}

func service(kind string) (Service, bool) {
	for _, s := range Services {
		if s.Kind == kind {
			return s, true
		}
	}
	return Service{}, false
}

// Picture reports whether a service of this kind can carry the snapshot.
func Picture(kind string) bool {
	s, ok := service(kind)
	return ok && s.Picture
}

// HasSecretURL is true for the services whose address is itself the secret:
// anyone holding a Discord or Slack webhook URL can post to the channel.
func HasSecretURL(kind string) bool { return kind == "discord" || kind == "slack" }

// Send delivers m through the channel. The picture rides along only when the
// channel asks for it and its service can carry one.
func Send(ctx context.Context, ch config.NotifyChannel, m Message) error {
	if !ch.Snapshot || !Picture(ch.Kind) {
		m.Snapshot = nil
	}
	switch ch.Kind {
	case "discord":
		return sendDiscord(ctx, ch, m)
	case "slack":
		return sendSlack(ctx, ch, m)
	case "telegram":
		return sendTelegram(ctx, ch, m)
	case "ntfy":
		return sendNtfy(ctx, ch, m)
	case "pushover":
		return sendPushover(ctx, ch, m)
	case "gotify":
		return sendGotify(ctx, ch, m)
	case "email":
		return sendEmail(ctx, ch, m)
	case "webhook":
		return sendWebhook(ctx, ch, m)
	}
	return fmt.Errorf("%q is not a service the Bridge knows", ch.Kind)
}

// Validate checks that a channel has what its service needs, in words a
// person can act on. Secrets must already be filled in from the stored copy.
func Validate(ch config.NotifyChannel) error {
	s, ok := service(ch.Kind)
	if !ok {
		return fmt.Errorf("%q is not a service the Bridge knows", ch.Kind)
	}
	for _, k := range ch.Events {
		if _, ok := Labels[events.Kind(k)]; !ok {
			return fmt.Errorf("%q is not an event the Bridge sends", k)
		}
	}
	needURL := func(what string) error {
		u, err := url.Parse(strings.TrimSpace(ch.URL))
		if ch.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s needs %s, starting with http:// or https://", s.Name, what)
		}
		return nil
	}
	need := func(v, what string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s needs %s", s.Name, what)
		}
		return nil
	}
	switch ch.Kind {
	case "discord", "slack":
		return needURL("a webhook URL")
	case "webhook":
		return needURL("a URL")
	case "ntfy":
		return needURL("a topic URL")
	case "gotify":
		if err := needURL("the server address"); err != nil {
			return err
		}
		return need(ch.Token, "an app token")
	case "telegram":
		if err := need(ch.Token, "a bot token"); err != nil {
			return err
		}
		return need(ch.Target, "a chat ID")
	case "pushover":
		if err := need(ch.Token, "an app token"); err != nil {
			return err
		}
		return need(ch.Target, "a user key")
	case "email":
		sm := ch.SMTP
		if sm == nil || strings.TrimSpace(sm.Host) == "" {
			return errors.New("Email needs a mail server")
		}
		if sm.Port < 0 || sm.Port > 65535 {
			return errors.New("Email needs a port between 1 and 65535")
		}
		if sm.Security != "" && sm.Security != "starttls" && sm.Security != "tls" && sm.Security != "none" {
			return errors.New("Email security must be starttls, tls or none")
		}
		if err := checkSignIn(sm); err != nil {
			return err
		}
		if _, err := parseFrom(sm); err != nil {
			return err
		}
		_, err := recipients(ch.Target)
		return err
	}
	return nil
}

// ---- HTTP plumbing ----

var (
	client = &http.Client{Timeout: 10 * time.Second}
	// retryDefault is the wait before the one retry when the service sent no
	// Retry-After.
	retryDefault = 2 * time.Second
)

const (
	maxReply    = 64 << 10
	maxRetryFor = 30 * time.Second
)

type reply struct {
	status int
	body   []byte
}

// do sends the request build makes. A 429 or 5xx gets one retry after
// Retry-After (at most 30 s). The body of a reply is capped.
func do(ctx context.Context, svc string, build func() (*http.Request, error)) (reply, error) {
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return reply{}, err
		}
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return reply{}, netErr(svc, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
		resp.Body.Close()
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt == 0 {
			t := time.NewTimer(retryAfter(resp.Header.Get("Retry-After")))
			select {
			case <-t.C:
				continue
			case <-ctx.Done():
				t.Stop()
				return reply{}, netErr(svc, ctx.Err())
			}
		}
		return reply{resp.StatusCode, body}, nil
	}
}

func retryAfter(h string) time.Duration {
	d := retryDefault
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = time.Until(t)
	}
	return max(0, min(d, maxRetryFor))
}

// netErr words a transport failure. The URL is left out on purpose: it holds
// the bot token or the webhook secret.
func netErr(svc string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%s did not answer within 10 seconds", svc)
	}
	return fmt.Errorf("could not reach %s: %v", svc, err)
}

// check turns a non-2xx reply into a plain-English error. cred names what to
// blame for a 401 or 403.
func check(svc, cred string, r reply) error {
	if r.status/100 == 2 {
		return nil
	}
	switch {
	case r.status == 401 || r.status == 403:
		return fmt.Errorf("%s refused the %s", svc, cred)
	case r.status == 404:
		return fmt.Errorf("%s could not find that address (404). Check the URL", svc)
	case r.status == 429:
		return fmt.Errorf("%s is limiting how often the Bridge may send. Try again in a minute", svc)
	case r.status >= 500:
		return fmt.Errorf("%s had a problem on its side (HTTP %d). Try again later", svc, r.status)
	}
	if d := detail(r.body); d != "" {
		return fmt.Errorf("%s said no (HTTP %d): %s", svc, r.status, d)
	}
	return fmt.Errorf("%s said no (HTTP %d)", svc, r.status)
}

// detail pulls the reason out of a service's error body, if it gave one.
func detail(body []byte) string {
	var j map[string]any
	if json.Unmarshal(body, &j) == nil {
		for _, k := range []string{"description", "message", "error", "errors"} {
			switch v := j[k].(type) {
			case string:
				return clip(v)
			case []any:
				var parts []string
				for _, x := range v {
					if s, ok := x.(string); ok {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					return clip(strings.Join(parts, ", "))
				}
			}
		}
		return ""
	}
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(s, "<") {
		return ""
	}
	return clip(s)
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return trunc(s, 160)
}

// trunc cuts s to n runes, marking the cut.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func jsonReq(method, u string, v any) (*http.Request, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// multi builds a multipart body of text fields plus one JPEG file.
func multi(fields map[string]string, fileField string, jpeg []byte) (io.Reader, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="snapshot.jpg"`, fileField))
	h.Set("Content-Type", "image/jpeg")
	p, _ := w.CreatePart(h)
	p.Write(jpeg)
	w.Close()
	return &b, w.FormDataContentType()
}

// ---- the services ----

func sendDiscord(ctx context.Context, ch config.NotifyChannel, m Message) error {
	payload := map[string]any{
		"username":         "FoxTrack Bridge",
		"content":          trunc(m.Markdown(), 1900),
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	r, err := do(ctx, "Discord", func() (*http.Request, error) {
		if m.Snapshot == nil {
			return jsonReq("POST", ch.URL, payload)
		}
		b, _ := json.Marshal(payload)
		body, ctype := multi(map[string]string{"payload_json": string(b)}, "files[0]", m.Snapshot)
		req, err := http.NewRequest("POST", ch.URL, body)
		if req != nil {
			req.Header.Set("Content-Type", ctype)
		}
		return req, err
	})
	if err != nil {
		return err
	}
	return check("Discord", "webhook URL", r)
}

func sendSlack(ctx context.Context, ch config.NotifyChannel, m Message) error {
	r, err := do(ctx, "Slack", func() (*http.Request, error) {
		return jsonReq("POST", ch.URL, map[string]any{"text": m.Slack()})
	})
	if err != nil {
		return err
	}
	return check("Slack", "webhook URL", r)
}

// telegramAPI is a variable so tests can point it at a local server.
var telegramAPI = "https://api.telegram.org"

func sendTelegram(ctx context.Context, ch config.NotifyChannel, m Message) error {
	call := func(method string, fields map[string]string, jpeg []byte) error {
		u := telegramAPI + "/bot" + ch.Token + "/" + method
		r, err := do(ctx, "Telegram", func() (*http.Request, error) {
			if jpeg == nil {
				form := url.Values{}
				for k, v := range fields {
					form.Set(k, v)
				}
				req, err := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
				if req != nil {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				}
				return req, err
			}
			body, ctype := multi(fields, "photo", jpeg)
			req, err := http.NewRequest("POST", u, body)
			if req != nil {
				req.Header.Set("Content-Type", ctype)
			}
			return req, err
		})
		if err != nil {
			return err
		}
		return telegramCheck(r)
	}
	text := m.Telegram()
	chat := strings.TrimSpace(ch.Target)
	quiet := "false"
	if m.Event.Kind.Silent() {
		quiet = "true" // arrives without a sound
	}
	if m.Snapshot != nil {
		fields := map[string]string{"chat_id": chat, "disable_notification": quiet}
		// A caption holds 1024 characters; a longer message follows the photo.
		if len([]rune(text)) <= 1024 {
			fields["caption"], fields["parse_mode"] = text, "HTML"
			return call("sendPhoto", fields, m.Snapshot)
		}
		if err := call("sendPhoto", fields, m.Snapshot); err != nil {
			return err
		}
	}
	// No link preview: the alarm link would add a large card to every alarm.
	return call("sendMessage", map[string]string{
		"chat_id": chat, "text": trunc(text, 4000), "parse_mode": "HTML",
		"disable_notification": quiet, "link_preview_options": `{"is_disabled":true}`,
	}, nil)
}

func telegramCheck(r reply) error {
	if r.status/100 == 2 {
		return nil
	}
	d := detail(r.body)
	switch {
	case r.status == 401 || r.status == 404:
		return errors.New("Telegram refused the bot token")
	case r.status == 400 && strings.Contains(d, "chat not found"):
		return errors.New("Telegram can't find that chat. Send your bot a message first, then check the chat ID")
	case r.status == 403:
		return errors.New("Telegram won't let the bot message that chat. Open the chat with the bot and press Start, or add the bot to the group")
	}
	return check("Telegram", "bot token", r)
}

// header makes a value safe for an HTTP header: one line, RFC 2047 when it
// is not plain ASCII.
func header(s string) string {
	return mime.QEncoding.Encode("utf-8", strings.Join(strings.Fields(s), " "))
}

var ntfyTags = map[events.Kind]string{
	events.PrintStarted: "arrow_forward", events.PrintPaused: "pause_button", events.PrintResumed: "arrow_forward",
	events.PrintFinished: "white_check_mark", events.PrintFailed: "x", events.PrintCancelled: "stop_button",
	events.PrinterError: "rotating_light", events.PrinterAlarmCleared: "white_check_mark",
	events.PrinterOffline: "electric_plug", events.PrinterOnline: "electric_plug", events.MaintenanceDue: "wrench",
	events.PrintAlmostDone: "hourglass_half", events.PrintFailureSuspected: "warning",
}

func sendNtfy(ctx context.Context, ch config.NotifyChannel, m Message) error {
	prio := "default"
	if m.Event.Kind.Loud() {
		prio = "high"
	} else if m.Event.Kind.Silent() {
		prio = "low"
	}
	tag := ntfyTags[m.Event.Kind]
	if tag == "" {
		tag = "printer"
	}
	r, err := do(ctx, "ntfy", func() (*http.Request, error) {
		var req *http.Request
		var err error
		if m.Snapshot != nil {
			// The picture is the body, so the words go in a header (\n is a line break).
			req, err = http.NewRequest("PUT", ch.URL, bytes.NewReader(m.Snapshot))
			if req != nil {
				req.Header.Set("Filename", "snapshot.jpg")
				req.Header.Set("Message", header(strings.ReplaceAll(m.Body(), "\n", `\n`)))
			}
		} else {
			req, err = http.NewRequest("POST", ch.URL, strings.NewReader(m.Body()))
		}
		if err != nil {
			return nil, err
		}
		req.Header.Set("Title", header(m.Title))
		req.Header.Set("Priority", prio)
		req.Header.Set("Tags", tag)
		if m.Link != "" {
			req.Header.Set("Click", m.Link)
		}
		if ch.Token != "" {
			req.Header.Set("Authorization", "Bearer "+ch.Token)
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	return check("ntfy", "access token", r)
}

// pushoverAPI is a variable so tests can point it at a local server.
var pushoverAPI = "https://api.pushover.net/1/messages.json"

func sendPushover(ctx context.Context, ch config.NotifyChannel, m Message) error {
	fields := map[string]string{
		"token": ch.Token, "user": strings.TrimSpace(ch.Target),
		"title": trunc(m.Title, 250), "message": trunc(m.Body(), 1024),
		"timestamp": strconv.FormatInt(m.Event.Time.Unix(), 10),
	}
	if m.Event.Time.IsZero() {
		delete(fields, "timestamp")
	}
	if m.Event.Kind.Loud() {
		fields["priority"] = "1"
	} else if m.Event.Kind.Silent() {
		fields["priority"] = "-1" // no sound or vibration
	}
	if m.Link != "" {
		fields["url"], fields["url_title"] = m.Link, linkText
		fields["message"] = trunc(strings.Join(m.Lines, "\n"), 1024)
	}
	r, err := do(ctx, "Pushover", func() (*http.Request, error) {
		var body io.Reader
		ctype := "application/x-www-form-urlencoded"
		if m.Snapshot != nil {
			body, ctype = multi(fields, "attachment", m.Snapshot)
		} else {
			form := url.Values{}
			for k, v := range fields {
				form.Set(k, v)
			}
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequest("POST", pushoverAPI, body)
		if req != nil {
			req.Header.Set("Content-Type", ctype)
		}
		return req, err
	})
	if err != nil {
		return err
	}
	if r.status == 400 {
		d := strings.ToLower(detail(r.body))
		if strings.Contains(d, "application token") {
			return errors.New("Pushover refused the app token")
		}
		if strings.Contains(d, "user identifier") {
			return errors.New("Pushover refused the user key")
		}
	}
	return check("Pushover", "app token", r)
}

func sendGotify(ctx context.Context, ch config.NotifyChannel, m Message) error {
	prio := 5
	if m.Event.Kind.Loud() {
		prio = 8
	} else if m.Event.Kind.Silent() {
		prio = 2 // Gotify's Android app shows 1-3 without a sound
	}
	payload := map[string]any{"title": m.Title, "message": m.Body(), "priority": prio}
	if m.Link != "" {
		payload["extras"] = map[string]any{"client::notification": map[string]any{"click": map[string]string{"url": m.Link}}}
	}
	u := strings.TrimRight(strings.TrimSpace(ch.URL), "/") + "/message?token=" + url.QueryEscape(ch.Token)
	r, err := do(ctx, "Gotify", func() (*http.Request, error) { return jsonReq("POST", u, payload) })
	if err != nil {
		return err
	}
	return check("Gotify", "app token", r)
}

// webhookRecord is the finished print, as the webhook payload shows it.
type webhookRecord struct {
	Result       string  `json:"result"`
	StartedAt    int64   `json:"started_at,omitempty"`
	EndedAt      int64   `json:"ended_at,omitempty"`
	DurationSec  int64   `json:"duration_seconds"`
	FilamentG    float64 `json:"filament_grams,omitempty"`
	EnergyWh     float64 `json:"energy_wh,omitempty"`
	PercentAtEnd int     `json:"percent_at_end,omitempty"`
}

// webhookAlarm is one alarm of a printer_error or printer_alarm_cleared event.
type webhookAlarm struct {
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Severity   string `json:"severity,omitempty"`
	LastedSecs int64  `json:"lasted_seconds,omitempty"`
}

// WebhookPayload is the JSON a "webhook" channel receives.
type WebhookPayload struct {
	Event     string         `json:"event"`
	Time      string         `json:"time"`
	Title     string         `json:"title"`
	Text      string         `json:"text"`
	PrinterID string         `json:"printer_id,omitempty"`
	Printer   string         `json:"printer,omitempty"`
	File      string         `json:"file,omitempty"`
	Progress  int            `json:"progress"`
	Message   string         `json:"message,omitempty"`
	Code      string         `json:"code,omitempty"`
	Severity  string         `json:"severity,omitempty"`
	Alarms    []webhookAlarm `json:"alarms,omitempty"`
	Record    *webhookRecord `json:"record,omitempty"`
	Snapshot  string         `json:"snapshot_base64,omitempty"`
}

func sendWebhook(ctx context.Context, ch config.NotifyChannel, m Message) error {
	e := m.Event
	t := e.Time
	if t.IsZero() {
		t = time.Now()
	}
	p := WebhookPayload{
		Event: string(e.Kind), Time: t.UTC().Format(time.RFC3339), Title: m.Title, Text: m.Body(),
		PrinterID: e.PrinterID, Printer: e.Printer, File: e.File, Progress: e.Progress,
		Message: e.Message, Code: e.Code, Severity: e.Severity,
	}
	for _, a := range e.Alarms {
		p.Alarms = append(p.Alarms, webhookAlarm{a.Code, a.Message, a.Severity, int64(a.Lasted.Seconds())})
	}
	if r := e.Record; r != nil {
		p.Record = &webhookRecord{Result: r.Result, StartedAt: r.StartTime, EndedAt: r.EndTime, DurationSec: r.Duration, FilamentG: r.TotalGrams(), PercentAtEnd: r.PercentAtEnd}
		if r.EnergyWh != nil {
			p.Record.EnergyWh = *r.EnergyWh
		}
	}
	if m.Snapshot != nil {
		p.Snapshot = base64.StdEncoding.EncodeToString(m.Snapshot)
	}
	r, err := do(ctx, "The webhook", func() (*http.Request, error) { return jsonReq("POST", ch.URL, p) })
	if err != nil {
		return err
	}
	return check("The webhook", "credentials", r)
}
