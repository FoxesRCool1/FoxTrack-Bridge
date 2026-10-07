package ftps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an implicit-FTPS server that behaves like the printers:
// TLS 1.2 only, and a data connection that does not resume the control
// session is refused with 522.
type fakeServer struct {
	ln       net.Listener
	password string
	final    string // reply sent after the data connection closes; "" = never send one
	pasvHost string // host written in the 227 reply
	drop     int    // bytes the server loses from the end of an upload

	mu       sync.Mutex
	stored   map[string][]byte
	resumed  []bool
	verbs    []string // DELE and STOR, in order
	dataSeen chan struct{}
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake printer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{
		ln: ln, password: "12345678", final: "226 Transfer complete.",
		pasvHost: "10.255.255.1", stored: map[string][]byte{}, dataSeen: make(chan struct{}, 8),
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *fakeServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeServer) serve(c net.Conn, dataCfg *tls.Config) {
	defer c.Close()
	ctl := c.(*tls.Conn)
	if ctl.Handshake() != nil {
		return
	}
	say := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	say("220 fake bblp")
	rd := bufio.NewReader(c)
	var pasv net.Listener
	defer func() {
		if pasv != nil {
			pasv.Close()
		}
	}()
	var user string
	var clear bool
	var authed bool
	var stored []byte
	var storedName string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "USER":
			user = arg
			say("331 password please")
		case "PASS":
			if user == "bblp" && arg == s.password {
				authed = true
				say("230 ok")
			} else {
				say("530 Login incorrect.")
			}
		case "PBSZ":
			say("200 PBSZ=0")
		case "PROT":
			clear = arg == "C"
			say("200 PROT ok")
		case "TYPE":
			say("200 binary")
		case "PASV":
			if !authed {
				say("530 login first")
				continue
			}
			pasv, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				say("425 no port")
				continue
			}
			p := pasv.Addr().(*net.TCPAddr).Port
			say("227 Entering Passive Mode (%s,%d,%d)", strings.ReplaceAll(s.pasvHost, ".", ","), p>>8, p&255)
		case "DELE":
			s.mu.Lock()
			s.verbs = append(s.verbs, "DELE "+arg)
			_, ok := s.stored[arg]
			delete(s.stored, arg)
			s.mu.Unlock()
			if ok {
				say("250 Delete operation successful.")
			} else {
				say("550 Delete operation failed.")
			}
		case "STOR":
			s.mu.Lock()
			s.verbs = append(s.verbs, "STOR "+arg)
			_, exists := s.stored[arg]
			s.mu.Unlock()
			if exists { // like the printers: no overwrite
				say("553 Could not create file.")
				continue
			}
			dc, err := pasv.Accept()
			if err != nil {
				say("425 no data connection")
				continue
			}
			say("150 Ok to send data.")
			var tc net.Conn = dc
			if !clear {
				tt := tls.Server(dc, dataCfg)
				if err := tt.Handshake(); err != nil {
					dc.Close()
					continue
				}
				s.mu.Lock()
				s.resumed = append(s.resumed, tt.ConnectionState().DidResume)
				s.mu.Unlock()
				if !tt.ConnectionState().DidResume {
					tt.Close()
					say("522 SSL connection failed: session reuse required")
					continue
				}
				tc = tt
			}
			stored, _ = io.ReadAll(tc)
			tc.Close()
			stored = stored[:len(stored)-s.drop]
			storedName = arg
			s.mu.Lock()
			s.stored[arg] = stored
			s.mu.Unlock()
			s.dataSeen <- struct{}{}
			if s.final != "" {
				say("%s", s.final)
			}
		case "SIZE":
			s.mu.Lock()
			b, ok := s.stored[arg]
			s.mu.Unlock()
			if ok {
				say("213 %d", len(b))
			} else {
				say("550 no such file")
			}
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
		_ = storedName
	}
}

func (s *fakeServer) config() Config {
	return Config{
		Host: "127.0.0.1", Port: s.port(), User: "bblp", Password: s.password,
		StepTimeout: 5 * time.Second, StallTimeout: 5 * time.Second, FinalWait: 3 * time.Second,
	}
}

func writeTemp(t *testing.T, n int) (string, []byte) {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	p := filepath.Join(t.TempDir(), "x.gcode.3mf")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func TestUpload_StoresBytesAndResumesSession(t *testing.T) {
	s := newFakeServer(t)
	path, want := writeTemp(t, 3<<20+17)
	if err := Upload(context.Background(), s.config(), path, "job.gcode.3mf"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bytes.Equal(s.stored["job.gcode.3mf"], want) {
		t.Fatalf("stored %d bytes, want %d identical bytes", len(s.stored["job.gcode.3mf"]), len(want))
	}
	if len(s.resumed) != 1 || !s.resumed[0] {
		t.Fatalf("data connection did not resume the control session: %v", s.resumed)
	}
}

// Bridge always uploads under one name; the printer refuses STOR over an
// existing file, so each upload deletes it first (550 when there is none).
func TestUpload_DeletesBeforeStoreSoTheSecondUploadOverwrites(t *testing.T) {
	s := newFakeServer(t)
	first, _ := writeTemp(t, 1000)
	second, want := writeTemp(t, 2000)
	for _, path := range []string{first, second} {
		if err := Upload(context.Background(), s.config(), path, "job.gcode.3mf"); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !bytes.Equal(s.stored["job.gcode.3mf"], want) {
		t.Fatalf("stored %d bytes, want the second file", len(s.stored["job.gcode.3mf"]))
	}
	got := strings.Join(s.verbs, " | ")
	if wantVerbs := "DELE job.gcode.3mf | STOR job.gcode.3mf | DELE job.gcode.3mf | STOR job.gcode.3mf"; got != wantVerbs {
		t.Fatalf("verbs = %s, want %s", got, wantVerbs)
	}
}

// The 227 reply names a host that does not exist; only the port may be used.
func TestUpload_PASVHostIsIgnored(t *testing.T) {
	s := newFakeServer(t)
	s.pasvHost = "203.0.113.9"
	path, want := writeTemp(t, 1000)
	if err := Upload(context.Background(), s.config(), path, "a.gcode.3mf"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !bytes.Equal(s.stored["a.gcode.3mf"], want) {
		t.Fatal("bytes differ")
	}
}

// P2S answers 426 although the file arrived; SIZE decides.
func TestUpload_426WithMatchingSizeIsSuccess(t *testing.T) {
	s := newFakeServer(t)
	s.final = "426 Failure writing network stream."
	path, _ := writeTemp(t, 5000)
	if err := Upload(context.Background(), s.config(), path, "b.gcode.3mf"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
}

// No reply at all after the data (H2D can take very long): SIZE still decides
// once FinalWait is over.
func TestUpload_NoFinalReplyButFullSizeIsSuccess(t *testing.T) {
	s := newFakeServer(t)
	s.final = ""
	cfg := s.config()
	cfg.FinalWait = 300 * time.Millisecond
	path, _ := writeTemp(t, 5000)
	if err := Upload(context.Background(), cfg, path, "c.gcode.3mf"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
}

func TestUpload_426WithWrongSizeIsError(t *testing.T) {
	s := newFakeServer(t)
	s.final = "426 Failure writing network stream."
	s.drop = 10
	path, _ := writeTemp(t, 5000)
	if err := Upload(context.Background(), s.config(), path, "d.gcode.3mf"); err == nil {
		t.Fatal("want an error: the printer stored fewer bytes")
	}
}

func TestUpload_ClearDataChannel(t *testing.T) {
	s := newFakeServer(t)
	cfg := s.config()
	cfg.ClearData = true
	path, want := writeTemp(t, 70000)
	if err := Upload(context.Background(), cfg, path, "h.gcode.3mf"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !bytes.Equal(s.stored["h.gcode.3mf"], want) || len(s.resumed) != 0 {
		t.Fatalf("clear upload: stored ok = %v, TLS data connections = %d", bytes.Equal(s.stored["h.gcode.3mf"], want), len(s.resumed))
	}
}

func TestUpload_WrongPassword(t *testing.T) {
	s := newFakeServer(t)
	cfg := s.config()
	cfg.Password = "00000000"
	path, _ := writeTemp(t, 10)
	err := Upload(context.Background(), cfg, path, "f.gcode.3mf")
	if !errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v, want ErrLogin", err)
	}
}

func TestUpload_ContextCancelStops(t *testing.T) {
	s := newFakeServer(t)
	s.final = "" // the server never confirms
	cfg := s.config()
	cfg.FinalWait = time.Minute
	path, _ := writeTemp(t, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-s.dataSeen
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	go func() { done <- Upload(ctx, cfg, path, "g.gcode.3mf") }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload did not stop after cancel")
	}
}

func TestParsePASVPort(t *testing.T) {
	for in, want := range map[string]int{
		"Entering Passive Mode (192,168,1,5,31,64).": 31*256 + 64,
		"Entering Passive Mode (0,0,0,0,0,21)":       21,
	} {
		got, err := parsePASVPort(in)
		if err != nil || got != want {
			t.Errorf("parsePASVPort(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"nothing", "(1,2,3)", "(1,2,3,4,300,1)"} {
		if _, err := parsePASVPort(in); err == nil {
			t.Errorf("parsePASVPort(%q): want error", in)
		}
	}
}

// A cancel that lands between two dials: the conn added after closeAll must be
// closed at once and the upload must stop.
func TestConnSet_AddAfterCloseAllClosesConn(t *testing.T) {
	var s connSet
	a, aPeer := net.Pipe()
	defer aPeer.Close()
	if err := s.add(a); err != nil {
		t.Fatalf("add before close: %v", err)
	}
	s.closeAll()
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("conn added before closeAll was not closed")
	}

	b, bPeer := net.Pipe()
	defer bPeer.Close()
	if err := s.add(b); err == nil {
		t.Fatal("add after closeAll returned nil")
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("conn added after closeAll was left open")
	}
}
