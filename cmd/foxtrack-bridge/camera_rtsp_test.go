//go:build headless

package main

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"foxtrack-bridge/config"
)

func jpegOf(payload string) []byte { return append(append([]byte{0xFF, 0xD8}, payload...), 0xFF, 0xD9) }

func TestSplitJPEG(t *testing.T) {
	a, b, c := jpegOf("first"), jpegOf("second frame"), jpegOf("3")
	// Junk between pictures, a lone FF before one, and a picture cut short at the end.
	stream := bytes.Join([][]byte{[]byte("junk"), a, {0xFF}, b, []byte("xx"), c, {0xFF, 0xD8, 'c', 'u', 't'}}, nil)
	for name, r := range map[string]io.Reader{
		"whole":     bytes.NewReader(stream),
		"one byte":  iotest.OneByteReader(bytes.NewReader(stream)), // markers split across reads
		"half read": iotest.HalfReader(bytes.NewReader(stream)),
	} {
		var got [][]byte
		if err := splitJPEG(r, func(f []byte) { got = append(got, f) }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 3 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
			t.Fatalf("%s: got %d pictures %q, want the 3 whole ones", name, len(got), got)
		}
	}
}

func TestRTSPModel(t *testing.T) {
	cases := []struct {
		model, serial string
		want          bool
	}{
		{"X1 Carbon", "", true}, {"X1E", "", true}, {"X1", "", true},
		{"H2D", "", true}, {"H2S", "", true}, {"H2C", "", true}, {"P2S", "", true},
		{"Bambu Lab H2D Pro", "", true},
		{"P1S", "", false}, {"P1P", "", false}, {"A1", "", false}, {"A1 Mini", "", false},
		// Before the first report there is no model: the serial prefix decides.
		{"", "00M00A123456789", true},  // X1 Carbon
		{"", "03W00A123456789", true},  // X1E
		{"", "094abc123456789", true},  // H2D
		{"", "22E00A123456789", true},  // P2S
		{"", "01P00A123456789", false}, // P1S
		{"", "039abc123456789", false}, // A1
		{"", "", false},
		// The live model wins over the serial; a name it does not know defers to it.
		{"P1S", "00M00A123456789", false},
		{"O1D", "094abc123456789", true},
		{"Klipper", "", false},
	}
	for _, c := range cases {
		if got := rtspModel(c.model, c.serial); got != c.want {
			t.Errorf("rtspModel(%q, %q) = %v, want %v", c.model, c.serial, got, c.want)
		}
	}
	if !useRTSP(config.Printer{Name: "no live state", Serial: "00M00A123456789"}) {
		t.Error("useRTSP did not fall back to the serial number before the first report")
	}
}

func TestRTSPURLNeverShowsTheCode(t *testing.T) {
	const code = "s3cr3tC0de"
	u := rtspURL("192.168.1.50", code)
	if got := u.String(); got != "rtsps://bblp:"+code+"@192.168.1.50:322/streaming/live/1" {
		t.Fatalf("stream address = %q", got)
	}
	if strings.Contains(u.Redacted(), code) {
		t.Errorf("the redacted address still holds the code: %s", u.Redacted())
	}
	out := maskCode("Error opening input rtsps://bblp:"+code+"@192.168.1.50:322/x: refused", code)
	if strings.Contains(out, code) || !strings.Contains(out, "bblp:***@") {
		t.Errorf("maskCode left %q", out)
	}
}

func TestRTSPArgs(t *testing.T) {
	stream := strings.Join(rtspArgs("/x/in.ffconcat", false), " ")
	for _, want := range []string{"-f concat -safe 0", "-protocol_whitelist file,crypto,data,rtsp,rtsps,tcp,tls,udp,rtp,srtp", "-i /x/in.ffconcat", "-an", "-f image2pipe", "-vcodec mjpeg", "-q:v 5", "-r 10"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream args %q lack %q", stream, want)
		}
	}
	if strings.Contains(stream, "-frames:v") {
		t.Error("a stream must not stop after one frame")
	}
	if one := strings.Join(rtspArgs("/x/in.ffconcat", true), " "); !strings.Contains(one, "-frames:v 1") || strings.Contains(one, "-r 10") {
		t.Errorf("snapshot args = %q", one)
	}
}

// The access code goes into a private script, never onto the command line.
func TestRTSPInputKeepsTheCodeOffTheCommandLine(t *testing.T) {
	const code = "it's-s3cret"
	args, remove, err := rtspInput(rtspURL("192.168.1.50", code).String(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "s3cret") || strings.Contains(strings.Join(args, " "), "bblp") {
		t.Fatalf("the command line holds the address: %q", args)
	}
	script := args[slices.Index(args, "-i")+1]
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	want := "ffconcat version 1.0\nfile 'rtsps://bblp:it%27s-s3cret@192.168.1.50:322/streaming/live/1'\noption rtsp_transport tcp\n"
	if string(data) != want {
		t.Errorf("script = %q, want %q", data, want)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(script)
		di, _ := os.Stat(filepath.Dir(script))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("modes: file %v, folder %v; want 0600 and 0700", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	remove()
	if _, err := os.Stat(filepath.Dir(script)); err == nil {
		t.Error("the folder was not removed")
	}

	// A quote in the address is escaped the ffconcat way; a line break is refused.
	args, remove, err = rtspInput("rtsp://h/it's", true)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	data, _ = os.ReadFile(args[slices.Index(args, "-i")+1])
	if !strings.Contains(string(data), `file 'rtsp://h/it'\''s'`) {
		t.Errorf("quote not escaped: %q", data)
	}
	if _, _, err := rtspInput("rtsp://h/a\nfile '/etc/passwd'", true); err == nil {
		t.Error("a line break in the address was accepted")
	}
}

func TestRTSPWithoutFFmpeg(t *testing.T) {
	t.Setenv("FOXTRACK_BRIDGE_FFMPEG", "")
	t.Setenv("PATH", t.TempDir())
	if ffmpegPath() != "" {
		t.Fatal("found an ffmpeg in an empty PATH")
	}
	p := config.Printer{Name: "X1C", LANCode: "12345678"}

	rec := httptest.NewRecorder()
	rtspCameraStream(rec, httptest.NewRequest("GET", "/api/camera/X1C", nil), p, "192.168.1.5")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), rtspNeedsFFmpeg) {
		t.Errorf("stream = %d %q, want 503 with the ffmpeg message", rec.Code, rec.Body.String())
	}
	if _, err := rtspSnapshot(context.Background(), p, "192.168.1.5"); err == nil || err.Error() != rtspNeedsFFmpeg {
		t.Errorf("snapshot error = %v, want %q", err, rtspNeedsFFmpeg)
	}
}

// The tests below run a shell script in place of ffmpeg. It writes its pid and
// a line per start to files, then prints "one" for a snapshot (-frames:v) or
// a stream of "str" pictures.
const fakeFFmpeg = `#!/bin/sh
echo $$ > "$FAKE_DIR/pid"
echo start >> "$FAKE_DIR/starts"
case " $* " in *" -frames:v "*) printf '\377\330one\377\331'; exit 0;; esac
%s
while :; do printf '\377\330str\377\331'; sleep 0.02; done
`

func useFakeFFmpeg(t *testing.T, preamble string) (dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a shell script")
	}
	dir = t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	body := strings.Replace(fakeFFmpeg, "%s", preamble, 1)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOXTRACK_BRIDGE_FFMPEG", script)
	t.Setenv("FAKE_DIR", dir)

	oldIdle, oldFirst, oldStall, oldBackoff := rtspIdleStop, rtspFirstFrameTimeout, rtspViewerStall, rtspBackoffMin
	t.Cleanup(func() {
		rtspMu.Lock()
		for _, h := range rtspHubs {
			h.stopLocked()
		}
		rtspMu.Unlock()
		rtspIdleStop, rtspFirstFrameTimeout, rtspViewerStall, rtspBackoffMin = oldIdle, oldFirst, oldStall, oldBackoff
	})
	return dir
}

func fakeStarts(dir string) int {
	data, _ := os.ReadFile(filepath.Join(dir, "starts"))
	return strings.Count(string(data), "start")
}

func fakePID(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func processGone(pid int) bool {
	p, _ := os.FindProcess(pid)
	return p.Signal(syscall.Signal(0)) != nil // a zombie would still answer
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRTSPViewersShareOneFFmpeg(t *testing.T) {
	dir := useFakeFFmpeg(t, "")
	rtspIdleStop = 100 * time.Millisecond
	p := config.Printer{Name: "X1C", LANCode: "12345678"}

	a, err := rtspSubscribe(p, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := rtspSubscribe(p, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*rtspSub{"a": a, "b": b} {
		select {
		case f := <-s.ch:
			if !bytes.Equal(f, jpegOf("str")) {
				t.Errorf("viewer %s got %q", name, f)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("viewer %s got no picture", name)
		}
	}
	if n := fakeStarts(dir); n != 1 {
		t.Errorf("ffmpeg started %d times for two viewers, want 1", n)
	}

	// With a stream running, a snapshot is its latest frame, not a new ffmpeg.
	snap, err := rtspSnapshot(context.Background(), p, "127.0.0.1")
	if err != nil || !bytes.Equal(snap, jpegOf("str")) {
		t.Errorf("snapshot = %q, %v; want the stream's frame", snap, err)
	}
	if n := fakeStarts(dir); n != 1 {
		t.Errorf("the snapshot started ffmpeg again (%d starts)", n)
	}

	// One viewer leaving keeps ffmpeg; the last one leaving stops it after the idle time.
	pid := fakePID(t, dir)
	a.close()
	time.Sleep(300 * time.Millisecond)
	if processGone(pid) {
		t.Fatal("ffmpeg stopped while a viewer was still watching")
	}
	b.close()
	waitFor(t, "ffmpeg to stop and be reaped after the last viewer left", func() bool { return processGone(pid) })
	rtspMu.Lock()
	left := len(rtspHubs)
	rtspMu.Unlock()
	if left != 0 {
		t.Errorf("%d hubs left after the last viewer", left)
	}
}

func TestRTSPSnapshotOneShot(t *testing.T) {
	dir := useFakeFFmpeg(t, "")
	p := config.Printer{Name: "P2S", LANCode: "12345678"}
	got, err := rtspSnapshot(context.Background(), p, "127.0.0.1")
	if err != nil || !bytes.Equal(got, jpegOf("one")) {
		t.Fatalf("snapshot = %q, %v", got, err)
	}
	if n := fakeStarts(dir); n != 1 {
		t.Errorf("starts = %d, want 1", n)
	}
	if pid := fakePID(t, dir); !processGone(pid) {
		t.Error("the one-shot ffmpeg was left behind")
	}
}

func TestRTSPFailureNeverShowsTheCode(t *testing.T) {
	useFakeFFmpeg(t, `echo "rtsps://bblp:topsecret1@10.0.0.1:322/streaming/live/1: Connection refused" >&2; exit 1`)
	rtspFirstFrameTimeout = 300 * time.Millisecond
	p := config.Printer{Name: "H2D", LANCode: "topsecret1"}

	// The failing script still answers a snapshot with a picture, so test the stream path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rtspCameraStream(w, r, p, "10.0.0.1")
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "Connection refused") {
		t.Errorf("got %d %q, want 502 with ffmpeg's reason", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "topsecret1") {
		t.Errorf("the error shown to the viewer holds the access code: %s", body)
	}
}

func TestRTSPStreamsMultipartLikePort6000(t *testing.T) {
	dir := useFakeFFmpeg(t, "")
	rtspIdleStop = 50 * time.Millisecond
	p := config.Printer{Name: "X1E", LANCode: "12345678"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rtspCameraStream(w, r, p, "127.0.0.1")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/x-mixed-replace" || params["boundary"] != "bambu" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-cache, no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	mr := multipart.NewReader(resp.Body, "bambu")
	for i := 0; i < 3; i++ {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		data, _ := io.ReadAll(part)
		if part.Header.Get("Content-Type") != "image/jpeg" || !bytes.Equal(data, jpegOf("str")) {
			t.Fatalf("part %d: %q %q", i, part.Header.Get("Content-Type"), data)
		}
	}
	pid := fakePID(t, dir)
	cancel()
	resp.Body.Close()
	waitFor(t, "ffmpeg to stop after the viewer disconnected", func() bool { return processGone(pid) })
}
