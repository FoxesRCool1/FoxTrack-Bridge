//go:build headless

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real ffmpeg reads a stream through the ffconcat script. ffmpeg cannot play
// one stream to another (its "listen" mode only takes pushed streams), so the
// test serves one itself: H.264 over RTSP on TCP, behind Basic sign-in.

type testRTSP struct {
	ln   net.Listener
	nals [][]byte

	mu         sync.Mutex
	signedIn   int      // requests that carried the right user and password
	transports []string // the Transport header of each SETUP
}

// testNALs makes a few seconds of H.264 with ffmpeg and cuts it into NAL units.
func testNALs(t *testing.T, bin string) [][]byte {
	t.Helper()
	out, err := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-frames:v", "50", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-g", "10", "-f", "h264", "-").Output()
	if err != nil || len(out) == 0 {
		t.Skipf("this ffmpeg cannot make H.264 test video: %v", err)
	}
	var nals [][]byte
	for _, p := range bytes.Split(out, []byte{0, 0, 1})[1:] {
		nals = append(nals, bytes.TrimRight(p, "\x00"))
	}
	return nals
}

func startTestRTSP(t *testing.T, nals [][]byte) *testRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testRTSP{ln: ln, nals: nals}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *testRTSP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	tp := textproto.NewReader(r)
	var wmu sync.Mutex
	write := func(b []byte) { wmu.Lock(); c.Write(b); wmu.Unlock() }
	done := make(chan struct{})
	defer close(done)
	for {
		if b, err := r.Peek(1); err != nil {
			return
		} else if b[0] == '$' { // an interleaved receiver report: skip it
			var h [4]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return
			}
			io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint16(h[2:])))
			continue
		}
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return
		}
		method, url := f[0], f[1]
		reply := func(status, body string, extra ...string) {
			var b strings.Builder
			fmt.Fprintf(&b, "RTSP/1.0 %s\r\nCSeq: %s\r\n", status, hdr.Get("Cseq"))
			for _, e := range extra {
				b.WriteString(e + "\r\n")
			}
			if body != "" {
				fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
			}
			b.WriteString("\r\n" + body)
			write([]byte(b.String()))
		}
		if method != "OPTIONS" {
			if hdr.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")) {
				reply("401 Unauthorized", "", `WWW-Authenticate: Basic realm="test"`)
				continue
			}
			s.mu.Lock()
			s.signedIn++
			s.mu.Unlock()
		}
		switch method {
		case "OPTIONS":
			reply("200 OK", "", "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN")
		case "DESCRIBE":
			sdp := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=test\r\nc=IN IP4 0.0.0.0\r\nt=0 0\r\n" +
				"m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1\r\na=control:streamid=0\r\n"
			reply("200 OK", sdp, "Content-Type: application/sdp", "Content-Base: "+url+"/")
		case "SETUP":
			tr := hdr.Get("Transport")
			s.mu.Lock()
			s.transports = append(s.transports, tr)
			s.mu.Unlock()
			if !strings.HasPrefix(tr, "RTP/AVP/TCP") {
				reply("461 Unsupported Transport", "")
				continue
			}
			reply("200 OK", "", "Transport: RTP/AVP/TCP;unicast;interleaved=0-1", "Session: 12345678")
		case "PLAY":
			reply("200 OK", "", "Session: 12345678", "Range: npt=0.000-")
			go s.stream(write, done)
		case "TEARDOWN":
			reply("200 OK", "", "Session: 12345678")
			return
		default:
			reply("405 Method Not Allowed", "")
		}
	}
}

// stream sends the NAL units as interleaved RTP, 25 pictures a second, over
// and over until done closes.
func (s *testRTSP) stream(write func([]byte), done <-chan struct{}) {
	seq, ts := uint16(1000), uint32(90000)
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, nal := range s.nals {
			typ := nal[0] & 0x1f
			pic := typ == 1 || typ == 5 // a picture's last NAL unit ends its time stamp
			pkt := make([]byte, 4+12+len(nal))
			pkt[0] = '$'
			binary.BigEndian.PutUint16(pkt[2:], uint16(12+len(nal)))
			rtp := pkt[4:]
			rtp[0], rtp[1] = 0x80, 96
			if pic {
				rtp[1] |= 0x80 // marker
			}
			binary.BigEndian.PutUint16(rtp[2:], seq)
			binary.BigEndian.PutUint32(rtp[4:], ts)
			binary.BigEndian.PutUint32(rtp[8:], 0x1234)
			copy(rtp[12:], nal)
			seq++
			write(pkt)
			if pic {
				ts += 3600
				select {
				case <-done:
					return
				case <-tick.C:
				}
			}
		}
	}
}

func TestRTSPRealFFmpegReadsThroughTheScript(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	srv := startTestRTSP(t, testNALs(t, bin))
	streamURL := "rtsp://user:pass@" + srv.ln.Addr().String() + "/live/1"

	for _, oneShot := range []bool{true, false} {
		t.Run("oneShot="+strconv.FormatBool(oneShot), func(t *testing.T) {
			args, remove, err := rtspInput(streamURL, oneShot)
			if err != nil {
				t.Fatal(err)
			}
			defer remove()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, args...)
			var stderr tailBuffer
			cmd.Stderr = &stderr
			cmd.WaitDelay = 2 * time.Second
			out, err := cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
			}
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "linux" { // what other users can read
				if raw, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline"); err == nil && bytes.Contains(raw, []byte("pass")) {
					t.Errorf("the running ffmpeg's command line holds the password: %q", raw)
				}
			}
			want, n := 3, 0
			if oneShot {
				want = 1
			}
			splitJPEG(out, func(frame []byte) {
				img, err := jpeg.Decode(bytes.NewReader(frame))
				if err != nil || img.Bounds().Dx() != 320 || img.Bounds().Dy() != 240 {
					t.Errorf("frame %d is not the 320x240 picture: %v", n, err)
				}
				if n++; n >= want {
					cancel() // enough; stops a stream
				}
			})
			cmd.Wait()
			if n < want {
				t.Fatalf("got %d pictures, want %d; ffmpeg said %q", n, want, stderr.lastLine("pass"))
			}
		})
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.signedIn == 0 {
		t.Error("ffmpeg never signed in with the user and password from the script")
	}
	if len(srv.transports) == 0 {
		t.Error("the server saw no SETUP")
	}
	for _, tr := range srv.transports {
		if !strings.HasPrefix(tr, "RTP/AVP/TCP") {
			t.Errorf("SETUP asked for %q: rtsp_transport tcp did not apply", tr)
		}
	}
}
