package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"foxtrack-bridge/config"
)

// smtpRoots lets tests trust a self-signed server; nil means the system's.
var smtpRoots *x509.CertPool

func parseFrom(s *config.SMTP) (*mail.Address, error) {
	from := strings.TrimSpace(s.From)
	if from == "" {
		from = strings.TrimSpace(s.Username)
	}
	a, err := mail.ParseAddress(from)
	if err != nil {
		return nil, errors.New("Email needs a From address, like bridge@example.com")
	}
	return a, nil
}

func recipients(target string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(target, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		a, err := mail.ParseAddress(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("%q is not an email address", strings.TrimSpace(f))
		}
		out = append(out, a.Address)
	}
	if len(out) == 0 {
		return nil, errors.New("Email needs at least one address to send to")
	}
	return out, nil
}

func sendEmail(ctx context.Context, ch config.NotifyChannel, m Message) error {
	if ch.SMTP == nil {
		return errors.New("Email needs a mail server")
	}
	s := ch.SMTP
	from, err := parseFrom(s)
	if err != nil {
		return err
	}
	to, err := recipients(ch.Target)
	if err != nil {
		return err
	}
	msg := buildMail(from, to, m, time.Now())

	host := strings.TrimSpace(s.Host)
	sec := s.Security
	if sec == "" {
		sec = "starttls"
	}
	port := s.Port
	if port == 0 {
		port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[sec]
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: smtpRoots}
	d := &net.Dialer{Timeout: 10 * time.Second}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var conn net.Conn
	if sec == "tls" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return netErr("the mail server", err)
	}
	defer conn.Close()
	dl := time.Now().Add(30 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(dl) {
		dl = t
	}
	conn.SetDeadline(dl)

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return smtpErr("greeting the mail server", err)
	}
	defer c.Close()
	if sec == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("The mail server does not offer STARTTLS. Pick \"None\" or \"TLS\" as the security")
		}
		if err := c.StartTLS(cfg); err != nil {
			return smtpErr("starting TLS", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return smtpErr("signing in", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return smtpErr("setting the sender", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return smtpErr("adding "+r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return smtpErr("starting the message", err)
	}
	if _, err := w.Write(msg); err != nil {
		return smtpErr("sending the message", err)
	}
	if err := w.Close(); err != nil {
		return smtpErr("sending the message", err)
	}
	c.Quit()
	return nil
}

// smtpErr words a mail server failure.
func smtpErr(doing string, err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		if te.Code == 535 || te.Code == 534 || te.Code == 530 {
			return errors.New("The mail server refused the username or password")
		}
		return fmt.Errorf("The mail server said no while %s: %d %s", doing, te.Code, clip(te.Msg))
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("The mail server did not answer in time")
	}
	return fmt.Errorf("Mail failed while %s: %v", doing, err)
}

// buildMail makes the whole message. With a picture it is multipart/related:
// the words (plain and HTML) and the JPEG the HTML shows inline.
func buildMail(from *mail.Address, to []string, m Message, now time.Time) []byte {
	subject := m.Title
	if p := m.Event.Printer; p != "" && !strings.Contains(subject, p) {
		subject += ": " + p
	}
	id := make([]byte, 12)
	rand.Read(id)
	domain := "foxtrack-bridge.local"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}

	var alt bytes.Buffer
	aw := multipart.NewWriter(&alt)
	htmlBody := "<html><body>" + m.HTML()
	if m.Snapshot != nil {
		htmlBody += `<p><img src="cid:snapshot@foxtrack-bridge" alt="Camera picture" style="max-width:100%"></p>`
	}
	htmlBody += "</body></html>"
	for _, p := range [][2]string{{"text/plain", m.Plain()}, {"text/html", htmlBody}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p[0]+"; charset=utf-8")
		h.Set("Content-Transfer-Encoding", "quoted-printable")
		pw, _ := aw.CreatePart(h)
		q := quotedprintable.NewWriter(pw)
		q.Write([]byte(p[1]))
		q.Close()
	}
	aw.Close()
	altType := "multipart/alternative; boundary=" + aw.Boundary()

	body, bodyType := alt.Bytes(), altType
	if m.Snapshot != nil {
		var rel bytes.Buffer
		rw := multipart.NewWriter(&rel)
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", altType)
		pw, _ := rw.CreatePart(h)
		pw.Write(alt.Bytes())
		h = textproto.MIMEHeader{}
		h.Set("Content-Type", `image/jpeg; name="snapshot.jpg"`)
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-Disposition", `inline; filename="snapshot.jpg"`)
		h.Set("Content-ID", "<snapshot@foxtrack-bridge>")
		pw, _ = rw.CreatePart(h)
		enc := base64.StdEncoding.EncodeToString(m.Snapshot)
		for len(enc) > 76 {
			pw.Write([]byte(enc[:76] + "\r\n"))
			enc = enc[76:]
		}
		pw.Write([]byte(enc + "\r\n"))
		rw.Close()
		body, bodyType = rel.Bytes(), `multipart/related; type="multipart/alternative"; boundary=`+rw.Boundary()
	}

	toAddrs := make([]string, len(to))
	for i, a := range to {
		toAddrs[i] = (&mail.Address{Address: a}).String()
	}
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from.String())
	hdr("To", strings.Join(toAddrs, ", "))
	hdr("Subject", header(subject))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", bodyType)
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}
