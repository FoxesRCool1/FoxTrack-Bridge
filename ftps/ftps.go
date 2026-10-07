// Package ftps uploads one file to a Bambu Lab printer over implicit FTPS
// (TLS from the first byte, port 990, user "bblp", password = LAN access
// code). Only what a Bambu printer needs is implemented: login, DELE, PASV,
// STOR, SIZE. Pure stdlib.
//
// Things the printers force on us (each verified against real printers by
// other open source projects):
//   - vsftpd on X1C/P1S refuses a data connection that does not resume the
//     control connection's TLS session ("522 SSL connection failed: session
//     reuse required"). One tls.Config with a session cache and the same
//     ServerName for both connections makes Go resume it.
//   - Only TLS 1.2 works; a P2S truncated uploads under TLS 1.3.
//   - The 226 after the upload can take 30 s or more, or arrive as 426 although
//     the whole file is there, so the final reply is waited for generously and
//     a failed one is double-checked with SIZE.
//   - A1 / A1 mini can hang on a TLS data channel: Config.ClearData sends
//     PROT C so the data channel is plain TCP.
package ftps

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrLogin means the printer refused the user name or access code.
var ErrLogin = errors.New("ftps login refused")

// ErrStalled means the upload made no progress for Config.StallTimeout.
var ErrStalled = errors.New("ftps upload stalled")

// Config describes one upload.
type Config struct {
	Host     string // printer IP or host name
	Port     int    // 0 = 990
	User     string // "bblp"
	Password string // the printer's LAN access code
	// ClearData sends PROT C: the data channel is plain TCP (A1 / A1 mini
	// fallback). The control channel stays TLS.
	ClearData bool
	// Timeouts. Zero = default.
	StepTimeout  time.Duration // each control reply, dial, handshake (default 15 s)
	StallTimeout time.Duration // no data written for this long (default 30 s)
	FinalWait    time.Duration // wait for the 226 after the last byte (default 120 s)
	// Logf receives progress lines. nil = silent.
	Logf func(format string, args ...any)
}

func (c *Config) defaults() {
	if c.Port == 0 {
		c.Port = 990
	}
	if c.StepTimeout == 0 {
		c.StepTimeout = 15 * time.Second
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = 30 * time.Second
	}
	if c.FinalWait == 0 {
		c.FinalWait = 120 * time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

// Upload stores the local file as remoteName in the printer's FTP root.
// Cancelling ctx closes both connections and returns promptly.
func Upload(ctx context.Context, cfg Config, localPath, remoteName string) (err error) {
	cfg.defaults()
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}

	// Closes whatever is open when ctx ends; every blocking call below then
	// fails at once.
	var conns connSet
	stop := context.AfterFunc(ctx, conns.closeAll)
	defer stop()
	defer conns.closeAll()
	defer func() {
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("%w (%v)", ctx.Err(), err)
		}
	}()

	// One config for control and data. ServerName must be non-empty and equal
	// on both: it is the session cache key (else ip:port, which differs).
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true, // printers use a self-signed certificate
		MinVersion:         tls.VersionTLS12,
		MaxVersion:         tls.VersionTLS12,
		ServerName:         cfg.Host,
		ClientSessionCache: tls.NewLRUClientSessionCache(8),
	}
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = "printer"
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	raw, err := dialTCP(ctx, addr, cfg.StepTimeout)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := conns.add(raw); err != nil {
		return err
	}
	ctl := tls.Client(raw, tlsCfg)
	if err := conns.add(ctl); err != nil {
		return err
	}
	if err := handshake(ctx, ctl, cfg.StepTimeout); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	c := &control{conn: ctl, tp: textproto.NewConn(ctl), step: cfg.StepTimeout}
	defer func() { // best effort; the reply is not read
		c.step = 2 * time.Second
		c.send("QUIT")
	}()

	if code, msg, err := c.reply(); err != nil || code != 220 {
		return replyErr("greeting", code, msg, err)
	}
	code, msg, err := c.cmd("USER %s", cfg.User)
	if err != nil || (code != 331 && code != 230) {
		return replyErr("USER", code, msg, err)
	}
	if code == 331 {
		code, msg, err = c.cmd("PASS %s", cfg.Password)
		if err != nil {
			return replyErr("PASS", code, msg, err)
		}
		if code == 530 || code == 430 {
			return fmt.Errorf("%w: %d %s", ErrLogin, code, msg)
		}
		if code != 230 {
			return replyErr("PASS", code, msg, nil)
		}
	}
	for _, step := range []struct{ cmd string }{{"PBSZ 0"}, {"PROT " + map[bool]string{true: "C", false: "P"}[cfg.ClearData]}, {"TYPE I"}} {
		if code, msg, err := c.cmd("%s", step.cmd); err != nil || code != 200 {
			return replyErr(step.cmd, code, msg, err)
		}
	}

	// Delete first: the printer refuses STOR over an existing file (553), and
	// Bridge always uploads under the same name. 550 = there was none.
	code, msg, err = c.cmd("DELE %s", remoteName)
	if err != nil || (code != 250 && code != 550) {
		return replyErr("DELE", code, msg, err)
	}

	// PASV: use only the port. Printers behind NAT or with odd addresses
	// report a wrong host; Python's ftplib connects to the control host too.
	code, msg, err = c.cmd("PASV")
	if err != nil || code != 227 {
		return replyErr("PASV", code, msg, err)
	}
	port, err := parsePASVPort(msg)
	if err != nil {
		return fmt.Errorf("PASV reply %q: %w", msg, err)
	}
	dataRaw, err := dialTCP(ctx, net.JoinHostPort(cfg.Host, strconv.Itoa(port)), cfg.StepTimeout)
	if err != nil {
		return fmt.Errorf("data connect: %w", err)
	}
	if err := conns.add(dataRaw); err != nil {
		return err
	}

	// STOR first, then the data channel's TLS handshake (as ftplib does):
	// the server starts its handshake only after the 150.
	code, msg, err = c.cmd("STOR %s", remoteName)
	if err != nil || (code != 150 && code != 125) {
		return replyErr("STOR", code, msg, err)
	}
	var data net.Conn = dataRaw
	if !cfg.ClearData {
		dt := tls.Client(dataRaw, tlsCfg)
		if err := conns.add(dt); err != nil {
			return err
		}
		if err := handshake(ctx, dt, cfg.StepTimeout); err != nil {
			return fmt.Errorf("data tls handshake: %w", err)
		}
		cfg.Logf("data connection resumed the control TLS session: %v", dt.ConnectionState().DidResume)
		data = dt
	}

	start := time.Now()
	n, err := copyWithStall(data, f, cfg.StallTimeout)
	if err != nil {
		return fmt.Errorf("send data after %d of %d bytes: %w", n, st.Size(), err)
	}
	data.Close() // clean close: TLS close_notify, then TCP FIN
	cfg.Logf("sent %d bytes in %s, waiting for the printer to confirm", n, time.Since(start).Round(time.Millisecond))

	// Wait for the 226. If it never comes or is an error, the file may still
	// be complete: compare SIZE.
	c.step = cfg.FinalWait
	code, msg, err = c.reply()
	c.step = cfg.StepTimeout
	if err == nil && (code == 226 || code == 250) {
		return nil
	}
	cfg.Logf("final reply was %d %q (err %v), checking SIZE", code, msg, err)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	size, serr := c.size(remoteName)
	if serr != nil {
		return replyErr("upload result", code, msg, errors.Join(err, serr))
	}
	if size != st.Size() {
		return fmt.Errorf("printer stored %d of %d bytes (final reply %d %s)", size, st.Size(), code, msg)
	}
	cfg.Logf("SIZE matches (%d bytes): upload counted as done", size)
	return nil
}

// QUIT is best effort only: closing the connections ends the session, and some
// printers answer slowly although the file is already stored.

// connSet is the list of open connections, closed together on ctx cancel.
type connSet struct {
	mu     sync.Mutex
	cs     []net.Conn
	closed bool
}

// add keeps c to be closed with the rest. After closeAll has run (a cancel
// that came in between two dials) it closes c at once and returns an error, so
// the cancel is honoured.
func (s *connSet) add(c net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return errConnsClosed
	}
	s.cs = append(s.cs, c)
	return nil
}

var errConnsClosed = errors.New("upload cancelled")

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, c := range s.cs {
		c.Close()
	}
}

type control struct {
	conn net.Conn
	tp   *textproto.Conn
	step time.Duration
}

func (c *control) reply() (int, string, error) {
	c.conn.SetReadDeadline(time.Now().Add(c.step))
	code, msg, err := c.tp.ReadResponse(0)
	var te *textproto.Error
	if errors.As(err, &te) { // unexpected shape still carries a code
		return te.Code, te.Msg, nil
	}
	return code, msg, err
}

func (c *control) cmd(format string, args ...any) (int, string, error) {
	c.conn.SetWriteDeadline(time.Now().Add(c.step))
	if err := c.tp.PrintfLine(format, args...); err != nil {
		return 0, "", err
	}
	return c.reply()
}

// size asks for the stored size, skipping late replies of the upload (226,
// 426) that may still be queued.
func (c *control) size(name string) (int64, error) {
	if err := c.send("SIZE %s", name); err != nil {
		return 0, err
	}
	for i := 0; i < 4; i++ {
		code, msg, err := c.reply()
		if err != nil {
			return 0, err
		}
		switch {
		case code == 213:
			return strconv.ParseInt(strings.TrimSpace(msg), 10, 64)
		case code == 226 || code == 250 || code == 426 || code == 451 || code == 125 || code == 150:
			continue
		default:
			return 0, fmt.Errorf("SIZE: %d %s", code, msg)
		}
	}
	return 0, errors.New("SIZE: no answer")
}

func (c *control) send(format string, args ...any) error {
	c.conn.SetWriteDeadline(time.Now().Add(c.step))
	return c.tp.PrintfLine(format, args...)
}

func replyErr(what string, code int, msg string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: unexpected reply %d %s", what, code, msg)
}

// parsePASVPort reads "Entering Passive Mode (h1,h2,h3,h4,p1,p2)".
func parsePASVPort(msg string) (int, error) {
	i, j := strings.IndexByte(msg, '('), strings.LastIndexByte(msg, ')')
	if i < 0 || j < i {
		return 0, errors.New("no address in reply")
	}
	parts := strings.Split(msg[i+1:j], ",")
	if len(parts) != 6 {
		return 0, errors.New("want six numbers")
	}
	hi, err1 := strconv.Atoi(strings.TrimSpace(parts[4]))
	lo, err2 := strconv.Atoi(strings.TrimSpace(parts[5]))
	if err1 != nil || err2 != nil || hi < 0 || hi > 255 || lo < 0 || lo > 255 {
		return 0, errors.New("bad port numbers")
	}
	return hi<<8 | lo, nil
}

func dialTCP(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", addr)
}

func handshake(ctx context.Context, c *tls.Conn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.HandshakeContext(ctx)
}

// copyWithStall copies src to dst; every write must finish within stall.
func copyWithStall(dst net.Conn, src io.Reader, stall time.Duration) (int64, error) {
	br := bufio.NewReaderSize(src, 64<<10)
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			dst.SetWriteDeadline(time.Now().Add(stall))
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				var ne net.Error
				if errors.As(werr, &ne) && ne.Timeout() {
					return total, ErrStalled
				}
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
