package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/history"
)

var jpeg = []byte("\xff\xd8\xff\xe0fakejpeg")

func finished() events.Event {
	return events.Event{
		Kind: events.PrintFinished, Time: time.Unix(1_700_000_000, 0), PrinterID: "p1", Printer: "Fox <1>", File: "my_part-v2.3mf", Progress: 100,
		Record: &history.Record{Result: "finished", Duration: 2*3600 + 14*60, Filaments: []history.FilamentUse{{Grams: 30}, {Grams: 8.4}}},
	}
}

func TestComposeWording(t *testing.T) {
	m := Compose(finished())
	if m.Title != "Print finished" || m.Lines[0] != "Fox <1> · my_part-v2.3mf" || m.Lines[1] != "Took 2h 14m, 38 g of filament" {
		t.Fatalf("finished: %q %q", m.Title, m.Lines)
	}
	code := "0700_2000_0002_0001"
	m = Compose(events.Event{Kind: events.PrinterError, Printer: "Fox", Message: "Filament ran out", Code: code})
	if len(m.Lines) != 2 || m.Lines[1] != "Code "+code || m.Link != "https://wiki.bambulab.com/en/hms/error-code?code="+code {
		t.Fatalf("alarm: %q link %q", m.Lines, m.Link)
	}
	if m = Compose(events.Event{Kind: events.PrinterError, Message: "Klipper shut down", Code: "shutdown"}); m.Link != "" {
		t.Fatalf("a non-HMS code must not get a wiki link: %q", m.Link)
	}
	m = Compose(events.Event{Kind: events.PrintFailed, Printer: "Fox", File: "a.gcode", Progress: 45, Record: &history.Record{Duration: 3600}})
	if m.Lines[1] != "Failed after 1h 00m at 45%" {
		t.Fatalf("failed: %q", m.Lines)
	}
	m = Compose(events.Event{Kind: events.MaintenanceDue, Printer: "Fox", Message: "Lubricate the rods"})
	if m.Title != "Maintenance due" || m.Lines[1] != "Lubricate the rods" {
		t.Fatalf("maintenance: %q", m.Lines)
	}
}

func TestEscapingPerService(t *testing.T) {
	m := Compose(finished())
	if got := m.Telegram(); !strings.Contains(got, "Fox &lt;1&gt;") || strings.Contains(got, "<1>") {
		t.Fatalf("telegram html: %q", got)
	}
	if got := m.Markdown(); !strings.Contains(got, `my\_part\-v2`) {
		t.Fatalf("discord markdown: %q", got)
	}
	if got := m.Slack(); !strings.Contains(got, "Fox &lt;1&gt;") {
		t.Fatalf("slack: %q", got)
	}
	if got := m.HTML(); !strings.Contains(got, "Fox &lt;1&gt;") {
		t.Fatalf("email html: %q", got)
	}
}

func at(h, m int) time.Time { return time.Date(2026, 1, 2, h, m, 0, 0, time.Local) }

func TestQuietHours(t *testing.T) {
	cases := []struct {
		start, end string
		now        time.Time
		want       bool
	}{
		{"", "", at(3, 0), false},
		{"22:00", "", at(23, 0), false},
		{"13:00", "13:00", at(13, 0), false},
		{"01:00", "06:00", at(3, 0), true},
		{"01:00", "06:00", at(6, 0), false},
		{"22:00", "07:00", at(23, 30), true}, // across midnight, before it
		{"22:00", "07:00", at(2, 0), true},   // across midnight, after it
		{"22:00", "07:00", at(7, 0), false},
		{"22:00", "07:00", at(12, 0), false},
		{"22:00", "07:00", at(22, 0), true},
		{"nope", "07:00", at(2, 0), false},
	}
	for _, c := range cases {
		if got := Quiet(&config.Notifications{QuietStart: c.start, QuietEnd: c.end}, c.now); got != c.want {
			t.Errorf("%s-%s at %s: got %v", c.start, c.end, c.now.Format("15:04"), got)
		}
	}
	if Quiet(nil, at(3, 0)) {
		t.Error("no settings means no quiet hours")
	}
	if !events.PrinterError.Urgent() || events.PrintFinished.Urgent() {
		t.Error("errors pass quiet hours, finished prints do not")
	}
}

func TestWants(t *testing.T) {
	ch := config.NotifyChannel{Enabled: true}
	e := events.Event{Kind: events.PrintFinished, PrinterID: "p1"}
	if !Wants(ch, e) {
		t.Error("empty events means the default set")
	}
	if Wants(ch, events.Event{Kind: events.PrintStarted}) {
		t.Error("print_started is not in the default set")
	}
	ch.Events = []string{"print_started"}
	if !Wants(ch, events.Event{Kind: events.PrintStarted}) || Wants(ch, e) {
		t.Error("an explicit list replaces the default")
	}
	ch.Events, ch.Printers = nil, []string{"p2"}
	if Wants(ch, e) || !Wants(ch, events.Event{Kind: events.PrintFinished, PrinterID: "p2"}) {
		t.Error("printer filter")
	}
	ch.Printers, ch.Enabled = nil, false
	if Wants(ch, e) {
		t.Error("a disabled channel wants nothing")
	}
}

func TestValidate(t *testing.T) {
	bad := []config.NotifyChannel{
		{Kind: "fax"},
		{Kind: "discord", URL: "ftp://x/y"},
		{Kind: "discord", URL: ""},
		{Kind: "slack", URL: "javascript:alert(1)"},
		{Kind: "gotify", URL: "http://h"},
		{Kind: "telegram", Token: "t"},
		{Kind: "pushover", Target: "u"},
		{Kind: "email", Target: "a@b.c"},
		{Kind: "email", SMTP: &config.SMTP{Host: "h", From: "a@b.c"}, Target: "not an address"},
		{Kind: "email", SMTP: &config.SMTP{Host: "h", From: "a@b.c", Security: "ssl"}, Target: "a@b.c"},
		{Kind: "ntfy", URL: "https://ntfy.sh/x", Events: []string{"nonsense"}},
	}
	for i, ch := range bad {
		if err := Validate(ch); err == nil {
			t.Errorf("case %d should be refused: %+v", i, ch)
		}
	}
	good := []config.NotifyChannel{
		{Kind: "discord", URL: "https://discord.com/api/webhooks/1/x"},
		{Kind: "ntfy", URL: "https://ntfy.sh/x", Events: []string{"print_finished"}},
		{Kind: "gotify", URL: "http://h", Token: "t"},
		{Kind: "telegram", Token: "t", Target: "1"},
		{Kind: "email", SMTP: &config.SMTP{Host: "h", From: "a@b.c"}, Target: "a@b.c, d@e.f"},
	}
	for i, ch := range good {
		if err := Validate(ch); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

// capture records one request.
type captured struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func server(t *testing.T, status int, reply string) (*httptest.Server, func() captured) {
	t.Helper()
	var mu sync.Mutex
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = captured{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b}
		mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, func() captured { mu.Lock(); defer mu.Unlock(); return got }
}

func parts(t *testing.T, c captured) map[string]string {
	t.Helper()
	_, params, err := mime.ParseMediaType(c.header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	mr := multipart.NewReader(strings.NewReader(string(c.body)), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return out
		}
		b, _ := io.ReadAll(p)
		out[p.FormName()] = string(b)
	}
}

func send(ch config.NotifyChannel, m Message) error {
	return Send(context.Background(), ch, m)
}

func TestDiscord(t *testing.T) {
	srv, got := server(t, 204, "")
	m := Compose(finished())
	ch := config.NotifyChannel{Kind: "discord", URL: srv.URL + "/hook"}
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Content string
		Allowed map[string][]string `json:"allowed_mentions"`
	}
	json.Unmarshal(got().body, &p)
	if !strings.HasPrefix(p.Content, `**Print finished**`) || !strings.Contains(p.Content, `my\_part`) || len(p.Allowed["parse"]) != 0 {
		t.Fatalf("discord payload: %s", got().body)
	}

	// The picture goes as a file next to payload_json, and only when the channel wants it.
	m.Snapshot = jpeg
	send(ch, m)
	if c := got(); !strings.HasPrefix(c.header.Get("Content-Type"), "application/json") {
		t.Fatalf("Snapshot off must send plain JSON, got %s", c.header.Get("Content-Type"))
	}
	ch.Snapshot = true
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	ps := parts(t, got())
	if ps["files[0]"] != string(jpeg) || !strings.Contains(ps["payload_json"], "Print finished") {
		t.Fatalf("multipart: %v", ps)
	}
}

func TestSlack(t *testing.T) {
	srv, got := server(t, 200, "ok")
	if err := send(config.NotifyChannel{Kind: "slack", URL: srv.URL}, Compose(finished())); err != nil {
		t.Fatal(err)
	}
	var p struct{ Text string }
	json.Unmarshal(got().body, &p)
	if !strings.HasPrefix(p.Text, "*Print finished*") || !strings.Contains(p.Text, "Fox &lt;1&gt;") {
		t.Fatalf("slack: %s", got().body)
	}
	srv2, _ := server(t, 404, "no_service")
	if err := send(config.NotifyChannel{Kind: "slack", URL: srv2.URL}, Compose(finished())); err == nil || !strings.Contains(err.Error(), "Slack could not find") {
		t.Fatalf("404: %v", err)
	}
}

func TestTelegram(t *testing.T) {
	srv, got := server(t, 200, `{"ok":true}`)
	old := telegramAPI
	telegramAPI = srv.URL
	defer func() { telegramAPI = old }()
	ch := config.NotifyChannel{Kind: "telegram", Token: "123:abc", Target: "42", Snapshot: true}
	m := Compose(finished())
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	if c := got(); c.path != "/bot123:abc/sendMessage" || !strings.Contains(string(c.body), "parse_mode=HTML") || !strings.Contains(string(c.body), "Fox+%26lt%3B1%26gt%3B") {
		t.Fatalf("sendMessage: %s %s", c.path, c.body)
	}
	m.Snapshot = jpeg
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	c := got()
	ps := parts(t, c)
	if c.path != "/bot123:abc/sendPhoto" || ps["photo"] != string(jpeg) || ps["chat_id"] != "42" || !strings.Contains(ps["caption"], "<b>Print finished</b>") {
		t.Fatalf("sendPhoto: %s %v", c.path, ps)
	}
	// A message too long for a caption follows the photo.
	m.Event.Message = strings.Repeat("x", 1100)
	m.Lines = append(m.Lines, m.Event.Message)
	send(ch, m)
	if got().path != "/bot123:abc/sendMessage" {
		t.Fatalf("long text should end with sendMessage, got %s", got().path)
	}

	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"ok":false,"description":"Unauthorized"}`, "Telegram refused the bot token"},
		{404, `{"ok":false}`, "Telegram refused the bot token"},
		{400, `{"ok":false,"description":"Bad Request: chat not found"}`, "can't find that chat"},
		{403, `{"ok":false,"description":"Forbidden: bot was blocked by the user"}`, "Open the chat"},
	} {
		s, _ := server(t, c.status, c.body)
		telegramAPI = s.URL
		if err := send(ch, Compose(finished())); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: got %v, want %q", c.status, err, c.want)
		}
	}
}

func TestNtfy(t *testing.T) {
	srv, got := server(t, 200, "{}")
	ch := config.NotifyChannel{Kind: "ntfy", URL: srv.URL + "/topic", Token: "tk_secret"}
	e := finished()
	e.Printer = "Füchsin"
	m := Compose(e)
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	c := got()
	title, _ := new(mime.WordDecoder).DecodeHeader(c.header.Get("Title"))
	if c.method != "POST" || title != "Print finished" || c.header.Get("Priority") != "default" || c.header.Get("Tags") != "white_check_mark" ||
		c.header.Get("Authorization") != "Bearer tk_secret" || !strings.Contains(string(c.body), "Füchsin") {
		t.Fatalf("ntfy text: %s %v %s", c.method, c.header, c.body)
	}
	m.Snapshot = jpeg
	ch.Snapshot = true
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	c = got()
	msg, _ := new(mime.WordDecoder).DecodeHeader(c.header.Get("Message"))
	if c.method != "PUT" || string(c.body) != string(jpeg) || c.header.Get("Filename") != "snapshot.jpg" || !strings.Contains(msg, `\n`) || !strings.Contains(msg, "Füchsin") {
		t.Fatalf("ntfy picture: %s %v", c.method, c.header)
	}
	// Failed prints are urgent; an HMS alarm carries the wiki link as the click target.
	send(ch, Compose(events.Event{Kind: events.PrinterError, Printer: "Fox", Message: "Jam", Code: "0700_2000_0002_0001"}))
	if c = got(); c.header.Get("Priority") != "high" || !strings.Contains(c.header.Get("Click"), "wiki.bambulab.com") {
		t.Fatalf("ntfy alarm: %v", c.header)
	}
}

func TestPushover(t *testing.T) {
	srv, got := server(t, 200, `{"status":1}`)
	old := pushoverAPI
	pushoverAPI = srv.URL
	defer func() { pushoverAPI = old }()
	ch := config.NotifyChannel{Kind: "pushover", Token: "apptok", Target: "userkey", Snapshot: true}
	m := Compose(finished())
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	if b := string(got().body); !strings.Contains(b, "token=apptok") || !strings.Contains(b, "user=userkey") || !strings.Contains(b, "title=Print+finished") {
		t.Fatalf("pushover form: %s", b)
	}
	m.Snapshot = jpeg
	send(ch, m)
	if ps := parts(t, got()); ps["attachment"] != string(jpeg) || ps["user"] != "userkey" {
		t.Fatalf("pushover attachment: %v", ps)
	}
	bad, _ := server(t, 400, `{"errors":["application token is invalid"],"status":0}`)
	pushoverAPI = bad.URL
	if err := send(ch, m); err == nil || err.Error() != "Pushover refused the app token" {
		t.Fatalf("bad token: %v", err)
	}
}

func TestGotify(t *testing.T) {
	srv, got := server(t, 200, "{}")
	ch := config.NotifyChannel{Kind: "gotify", URL: srv.URL + "/", Token: "AbC"}
	if err := send(ch, Compose(finished())); err != nil {
		t.Fatal(err)
	}
	c := got()
	var p struct {
		Title    string
		Priority int
	}
	json.Unmarshal(c.body, &p)
	if c.path != "/message" || c.query != "token=AbC" || p.Title != "Print finished" || p.Priority != 5 {
		t.Fatalf("gotify: %s?%s %s", c.path, c.query, c.body)
	}
	denied, _ := server(t, 401, "")
	ch.URL = denied.URL
	if err := send(ch, Compose(finished())); err == nil || err.Error() != "Gotify refused the app token" {
		t.Fatalf("401: %v", err)
	}
}

func TestWebhook(t *testing.T) {
	srv, got := server(t, 200, "")
	e := finished()
	e.Code, e.Severity = "c", "serious"
	m := Compose(e)
	m.Snapshot = jpeg
	ch := config.NotifyChannel{Kind: "webhook", URL: srv.URL}
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	var p WebhookPayload
	json.Unmarshal(got().body, &p)
	if p.Event != "print_finished" || p.Printer != "Fox <1>" || p.File != "my_part-v2.3mf" || p.Progress != 100 || p.Code != "c" || p.Severity != "serious" ||
		p.Record == nil || p.Record.DurationSec != 8040 || p.Record.FilamentG != 38.4 || p.Time != "2023-11-14T22:13:20Z" {
		t.Fatalf("payload: %s", got().body)
	}
	if p.Snapshot != "" {
		t.Fatal("the picture is sent only when Snapshot is on")
	}
	ch.Snapshot = true
	send(ch, m)
	json.Unmarshal(got().body, &p)
	if b, _ := base64.StdEncoding.DecodeString(p.Snapshot); string(b) != string(jpeg) {
		t.Fatalf("snapshot_base64: %q", p.Snapshot)
	}
}

func TestRetryAndLimits(t *testing.T) {
	old := retryDefault
	retryDefault = time.Millisecond
	defer func() { retryDefault = old }()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := send(config.NotifyChannel{Kind: "slack", URL: srv.URL}, Compose(finished())); err != nil || calls.Load() != 2 {
		t.Fatalf("one retry after 429: err %v calls %d", err, calls.Load())
	}

	srv5, _ := server(t, 503, "down")
	err := send(config.NotifyChannel{Kind: "slack", URL: srv5.URL}, Compose(finished()))
	if err == nil || !strings.Contains(err.Error(), "had a problem on its side") {
		t.Fatalf("5xx: %v", err)
	}
	srv4, _ := server(t, 429, "")
	if err := send(config.NotifyChannel{Kind: "slack", URL: srv4.URL}, Compose(finished())); err == nil || !strings.Contains(err.Error(), "limiting") {
		t.Fatalf("429: %v", err)
	}

	if retryAfter("999") != 30*time.Second || retryAfter("3") != 3*time.Second || retryAfter("") != time.Millisecond {
		t.Fatalf("Retry-After: %v %v %v", retryAfter("999"), retryAfter("3"), retryAfter(""))
	}

	// A cancelled context ends the wait for the retry.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(503)
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := Send(ctx, config.NotifyChannel{Kind: "slack", URL: slow.URL}, Compose(finished())); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("expected a prompt error, got %v after %v", err, time.Since(start))
	}
}

func TestErrorsNeverCarryTheSecret(t *testing.T) {
	srv, _ := server(t, 200, "")
	url := srv.URL
	srv.Close() // nothing listens any more
	old := telegramAPI
	telegramAPI = url
	defer func() { telegramAPI = old }()
	err := send(config.NotifyChannel{Kind: "telegram", Token: "SECRETTOKEN", Target: "1"}, Compose(finished()))
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") || !strings.Contains(err.Error(), "could not reach Telegram") {
		t.Fatalf("got %v", err)
	}
	err = send(config.NotifyChannel{Kind: "discord", URL: url + "/api/webhooks/1/SECRETHOOK"}, Compose(finished()))
	if err == nil || strings.Contains(err.Error(), "SECRETHOOK") {
		t.Fatalf("got %v", err)
	}
}

func TestResponseBodyIsCapped(t *testing.T) {
	srv, _ := server(t, 400, strings.Repeat("a", 1<<20))
	err := send(config.NotifyChannel{Kind: "slack", URL: srv.URL}, Compose(finished()))
	if err == nil || len(err.Error()) > 300 {
		t.Fatalf("error text should be short: %v", err)
	}
}

// ---- email against a fake SMTP server ----

type fakeSMTP struct {
	addr  string
	mu    sync.Mutex
	mechs string // what the AUTH line offers; set under mu before sending
	from  string
	rcpts []string
	auth  string
	data  string
}

// newFakeSMTP listens on localhost. mode is "none", "starttls" or "tls".
func newFakeSMTP(t *testing.T, mode string) *fakeSMTP {
	t.Helper()
	cert := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(cert.Close)
	smtpRoots = x509.NewCertPool()
	smtpRoots.AddCert(cert.Certificate())
	t.Cleanup(func() { smtpRoots = nil })
	cfg := &tls.Config{Certificates: cert.TLS.Certificates}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mode == "tls" {
		ln = tls.NewListener(ln, cfg)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String(), mechs: "PLAIN"}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, cfg, mode == "starttls")
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn, cfg *tls.Config, offerTLS bool) {
	defer func() { c.Close() }()
	r := bufio.NewReader(c)
	say := func(s string) { c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		f.mu.Lock()
		switch {
		case strings.HasPrefix(up, "EHLO"):
			say("250-fake")
			if offerTLS {
				say("250-STARTTLS")
			}
			if f.mechs == "" {
				say("250 OK")
			} else {
				say("250 AUTH " + f.mechs)
			}
		case up == "AUTH LOGIN":
			b64 := base64.StdEncoding
			say("334 " + b64.EncodeToString([]byte("Username:")))
			u, _ := r.ReadString('\n')
			say("334 " + b64.EncodeToString([]byte("Password:")))
			p, _ := r.ReadString('\n')
			du, _ := b64.DecodeString(strings.TrimSpace(u))
			dp, _ := b64.DecodeString(strings.TrimSpace(p))
			f.auth = "LOGIN " + string(du) + " " + string(dp)
			say("235 ok")
		case up == "STARTTLS":
			say("220 go ahead")
			c = tls.Server(c, cfg)
			r = bufio.NewReader(c)
			offerTLS = false
		case strings.HasPrefix(up, "AUTH PLAIN"):
			f.auth = line
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.from = line
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.rcpts = append(f.rcpts, line)
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.data = b.String()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			f.mu.Unlock()
			return
		default:
			say("250 ok")
		}
		f.mu.Unlock()
	}
}

func emailChannel(f *fakeSMTP, security string) config.NotifyChannel {
	host, port, _ := net.SplitHostPort(f.addr)
	p, _ := strconv.Atoi(port)
	return config.NotifyChannel{
		Kind: "email", Target: "mum@example.com, dad@example.com", Snapshot: true,
		SMTP: &config.SMTP{Host: host, Port: p, Username: "bridge", Password: "pw", From: "Fox Bridge <bridge@example.com>", Security: security},
	}
}

func TestEmail(t *testing.T) {
	for _, mode := range []string{"none", "starttls", "tls"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeSMTP(t, mode)
			e := finished()
			e.Printer = "Füchsin"
			m := Compose(e)
			m.Snapshot = jpeg
			if err := send(emailChannel(f, mode), m); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !strings.Contains(f.from, "<bridge@example.com>") || len(f.rcpts) != 2 || !strings.Contains(f.rcpts[1], "dad@example.com") {
				t.Fatalf("envelope: %q %q", f.from, f.rcpts)
			}
			if raw, _ := base64.StdEncoding.DecodeString(strings.Fields(f.auth)[2]); string(raw) != "\x00bridge\x00pw" {
				t.Fatalf("auth: %q", raw)
			}
			msg, err := mail.ReadMessage(strings.NewReader(f.data))
			if err != nil {
				t.Fatal(err)
			}
			subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
			if subj != "Print finished: Füchsin" || msg.Header.Get("To") == "" || !strings.Contains(msg.Header.Get("From"), "bridge@example.com") ||
				!strings.HasSuffix(msg.Header.Get("Message-ID"), "@example.com>") {
				t.Fatalf("headers: %v", msg.Header)
			}
			if _, err := msg.Header.Date(); err != nil {
				t.Fatalf("Date: %v", err)
			}
			ct, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
			if ct != "multipart/related" {
				t.Fatalf("content type %s", ct)
			}
			mr := multipart.NewReader(msg.Body, params["boundary"])
			alt, _ := mr.NextPart()
			if !strings.HasPrefix(alt.Header.Get("Content-Type"), "multipart/alternative") {
				t.Fatalf("first part: %v", alt.Header)
			}
			ab, _ := io.ReadAll(alt)
			if !strings.Contains(string(ab), "text/plain") || !strings.Contains(string(ab), "cid:snapshot@foxtrack-bridge") {
				t.Fatalf("alternative: %s", ab)
			}
			img, _ := mr.NextPart()
			ib, _ := io.ReadAll(img)
			raw, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(ib), "\r\n", ""))
			if img.Header.Get("Content-ID") != "<snapshot@foxtrack-bridge>" || string(raw) != string(jpeg) {
				t.Fatalf("image part: %v %q", img.Header, raw)
			}
		})
	}
}

func TestEmailWithoutPictureAndErrors(t *testing.T) {
	f := newFakeSMTP(t, "none")
	ch := emailChannel(f, "none")
	ch.Snapshot = false
	m := Compose(finished())
	m.Snapshot = jpeg // dropped: the channel does not want it
	if err := send(ch, m); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if !strings.Contains(f.data, "Content-Type: multipart/alternative") || strings.Contains(f.data, "image/jpeg") {
		t.Fatalf("plain mail: %s", f.data)
	}
	f.mu.Unlock()

	// STARTTLS asked for but not offered.
	if err := send(emailChannel(f, "starttls"), m); err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("no starttls: %v", err)
	}
	// Nothing listening.
	dead := emailChannel(f, "none")
	dead.SMTP.Port = 1
	if err := send(dead, m); err == nil || !strings.Contains(err.Error(), "could not reach the mail server") {
		t.Fatalf("dead: %v", err)
	}
}

func TestPickAuth(t *testing.T) {
	mech := func(offered string) string {
		a, err := pickAuth(offered, "u", "p", "mail.example.com")
		if err != nil {
			return "none"
		}
		name, _, _ := a.Start(&smtp.ServerInfo{Name: "mail.example.com", TLS: true})
		return name
	}
	for offered, want := range map[string]string{
		"PLAIN LOGIN": "PLAIN", "LOGIN PLAIN": "PLAIN", "plain": "PLAIN",
		"CRAM-MD5 LOGIN": "LOGIN", "login": "LOGIN",
		"CRAM-MD5": "CRAM-MD5", "XOAUTH2": "none", "": "none",
	} {
		if got := mech(offered); got != want {
			t.Errorf("server offers %q: picked %s, want %s", offered, got, want)
		}
	}
}

func TestLoginAuth(t *testing.T) {
	a := loginAuth{"bridge", "pw"}
	name, first, err := a.Start(&smtp.ServerInfo{Name: "mail.example.com", TLS: true})
	if name != "LOGIN" || first != nil || err != nil {
		t.Fatalf("Start = %q %q %v", name, first, err)
	}
	if got, err := a.Next([]byte("Username:"), true); string(got) != "bridge" || err != nil {
		t.Errorf("username answer = %q, %v", got, err)
	}
	if got, err := a.Next([]byte("Password:"), true); string(got) != "pw" || err != nil {
		t.Errorf("password answer = %q, %v", got, err)
	}
	if _, err := a.Next([]byte("Surprise:"), true); err == nil {
		t.Error("an unknown question was answered")
	}
	if got, err := a.Next(nil, false); got != nil || err != nil {
		t.Errorf("end of the exchange = %q, %v", got, err)
	}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "mail.example.com"}); err == nil {
		t.Error("LOGIN started over an unencrypted connection to another machine")
	}
	if _, _, err := a.Start(&smtp.ServerInfo{Name: "127.0.0.1"}); err != nil {
		t.Errorf("LOGIN refused on this machine: %v", err)
	}
}

// The server offers only LOGIN, so the Bridge signs in that way.
func TestEmailSignsInWithLogin(t *testing.T) {
	f := newFakeSMTP(t, "none")
	f.mu.Lock()
	f.mechs = "LOGIN"
	f.mu.Unlock()
	if err := send(emailChannel(f, "none"), Compose(finished())); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "LOGIN bridge pw" {
		t.Errorf("auth = %q", f.auth)
	}
}

func TestEmailSignInErrors(t *testing.T) {
	f := newFakeSMTP(t, "none")
	m := Compose(finished())
	for mechs, want := range map[string]string{
		"XOAUTH2": "offers no sign-in the Bridge can use (it offers: XOAUTH2)",
		"":        "does not offer sign-in",
	} {
		f.mu.Lock()
		f.mechs = mechs
		f.mu.Unlock()
		if err := send(emailChannel(f, "none"), m); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("server offers %q: %v, want %q", mechs, err, want)
		}
	}
}

func TestEmailSignInNeedsEncryption(t *testing.T) {
	smtpCfg := func(host, security, user string) *config.SMTP {
		return &config.SMTP{Host: host, Username: user, Password: "pw", From: "b@example.com", Security: security}
	}
	for _, c := range []struct {
		s    *config.SMTP
		deny bool
	}{
		{smtpCfg("mail.example.com", "none", "bridge"), true},
		{smtpCfg("mail.example.com", "none", ""), false},
		{smtpCfg("mail.example.com", "starttls", "bridge"), false},
		{smtpCfg("mail.example.com", "", "bridge"), false},
		{smtpCfg("mail.example.com", "tls", "bridge"), false},
		{smtpCfg("localhost", "none", "bridge"), false},
		{smtpCfg("127.0.0.1", "none", "bridge"), false},
		{smtpCfg("::1", "none", "bridge"), false},
	} {
		err := checkSignIn(c.s)
		if c.deny != (err != nil) || (err != nil && !strings.Contains(err.Error(), "Sign-in needs TLS or STARTTLS")) {
			t.Errorf("%s/%s/%q: %v", c.s.Host, c.s.Security, c.s.Username, err)
		}
	}
	// Sending refuses too, before it connects to anything.
	ch := config.NotifyChannel{Kind: "email", Target: "a@example.com", SMTP: smtpCfg("mail.example.com", "none", "bridge")}
	if err := send(ch, Compose(finished())); err == nil || !strings.Contains(err.Error(), "Sign-in needs TLS or STARTTLS") {
		t.Errorf("send = %v", err)
	}
}

// Only 535 means a wrong username or password; other codes say what they say.
func TestSMTPErrWording(t *testing.T) {
	const refused = "The mail server refused the username or password"
	if got := smtpErr("signing in", &textproto.Error{Code: 535, Msg: "5.7.8 Bad credentials"}); got.Error() != refused {
		t.Errorf("535: %v", got)
	}
	for _, code := range []int{530, 534} {
		got := smtpErr("signing in", &textproto.Error{Code: code, Msg: " 5.7.0 Must issue a\n STARTTLS command first "}).Error()
		if got == refused || !strings.Contains(got, strconv.Itoa(code)+" 5.7.0 Must issue a STARTTLS command first") {
			t.Errorf("%d: %q", code, got)
		}
	}
}
