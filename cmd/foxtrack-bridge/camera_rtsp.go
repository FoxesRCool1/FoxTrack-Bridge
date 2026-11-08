package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// RTSP cameras. The X1, H2 and P2S series stream their camera over RTSPS on
// port 322 (user "bblp", the LAN access code, a self-signed certificate)
// instead of the port-6000 JPEG stream of the A1 and P1. The Bridge reads it
// with ffmpeg, which turns the video into JPEG frames on a pipe.
//
// One ffmpeg runs per printer and every viewer shares it: Bambu printers
// allow only a few RTSP sessions, and decoding the stream twice would be
// wasted work. It starts with the first viewer and stops rtspIdleStop after
// the last one leaves.

// rtspNeedsFFmpeg is what the dashboard shows when ffmpeg is not installed.
const rtspNeedsFFmpeg = "This printer's camera needs ffmpeg. Install it (see the README) and restart the Bridge."

const rtspNoFrames = "the printer's camera sent no picture. Check the LAN access code, and that Liveview is switched on in the printer's settings"

// Variables so tests can shorten them.
var (
	rtspIdleStop          = 15 * time.Second // ffmpeg keeps running this long after the last viewer
	rtspFirstFrameTimeout = 20 * time.Second // a viewer waits this long for the first picture
	rtspViewerStall       = 30 * time.Second // a viewer is dropped after this long without a picture
	rtspStallTimeout      = 20 * time.Second // ffmpeg is restarted after this long without a picture
	rtspSnapshotTimeout   = 20 * time.Second
	rtspBackoffMin        = time.Second
	rtspBackoffMax        = 30 * time.Second
	rtspFreshFrame        = 5 * time.Second // a stream's latest frame is good for a snapshot this long
)

// useRTSP reports whether the printer's camera is the RTSP stream on port 322
// (X1, H2 and P2S series) rather than the port-6000 JPEG stream. The model
// comes from the live telemetry, or from the serial number before the first
// report.
func useRTSP(p config.Printer) bool {
	model := ""
	if s := liveStates()[p.Name]; s != nil {
		model = s.PrinterModel
	}
	return rtspModel(model, p.Serial)
}

// rtspModel decides from the reported model name, and from the serial number's
// model prefix when the name is missing or not one it knows.
func rtspModel(model, serial string) bool {
	if rtsp, known := modelCamera(model); known {
		return rtsp
	}
	rtsp, _ := modelCamera(mqttpkg.ModelFromSerial(serial))
	return rtsp
}

// modelCamera classifies "X1 Carbon", "Bambu Lab H2D", "P1S" and the like.
// known is false for a name that is none of the Bambu models with a camera.
func modelCamera(model string) (rtsp, known bool) {
	m := strings.ToUpper(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "BAMBU LAB ")
	first, _, _ := strings.Cut(m, " ")
	switch first {
	case "X1", "X1C", "X1E", "H2D", "H2S", "H2C", "P2S":
		return true, true
	case "A1", "A1M", "P1P", "P1S":
		return false, true
	}
	return false, false
}

// ffmpegPath finds ffmpeg: FOXTRACK_BRIDGE_FFMPEG when set, else the PATH.
// It returns "" when there is none. It looks each time, so ffmpeg installed
// later is found without a code change (the camera still needs a restart to
// show it in the dashboard's notes).
func ffmpegPath() string {
	for _, name := range []string{strings.TrimSpace(os.Getenv("FOXTRACK_BRIDGE_FFMPEG")), "ffmpeg"} {
		if name == "" {
			continue
		}
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}

// rtspURL is the printer's stream address. It holds the access code, so it is
// never logged: use Redacted() for that.
func rtspURL(ip, code string) *url.URL {
	return &url.URL{
		Scheme: "rtsps",
		User:   url.UserPassword("bblp", code),
		Host:   net.JoinHostPort(ip, "322"),
		Path:   "/streaming/live/1",
	}
}

// maskCode hides the access code wherever ffmpeg's output repeats it.
func maskCode(s, code string) string {
	if code == "" {
		return s
	}
	for _, c := range []string{code, url.QueryEscape(code), url.PathEscape(code)} {
		s = strings.ReplaceAll(s, c, "***")
	}
	return s
}

// rtspArgs is ffmpeg's command line: the stream read through an ffconcat
// script (see rtspInput), JPEG frames out on stdout. A snapshot stops after
// one frame; a stream is limited to 10 per second.
func rtspArgs(script string, oneShot bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-protocol_whitelist", "file,crypto,data,rtsp,rtsps,tcp,tls,udp,rtp,srtp",
		"-i", script, "-an"}
	if oneShot {
		args = append(args, "-frames:v", "1")
	}
	args = append(args, "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "5")
	if !oneShot {
		args = append(args, "-r", "10")
	}
	return append(args, "-")
}

// rtspInput keeps the stream address, which holds the access code, off
// ffmpeg's command line, where other users could read it in ps. It writes an
// ffconcat script (the address, RTSP over TCP) to a private folder and returns
// ffmpeg's arguments for it. The caller runs ffmpeg and then calls remove.
// removeStaleRTSPDirs deletes input folders left by a Bridge that was killed
// while ffmpeg ran. They hold the access code, so they must not pile up.
func removeStaleRTSPDirs() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "foxtrack-rtsp-*"))
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && time.Since(fi.ModTime()) > 24*time.Hour {
			os.RemoveAll(d)
		}
	}
}

func rtspInput(streamURL string, oneShot bool) (args []string, remove func(), err error) {
	if strings.ContainsAny(streamURL, "\r\n") {
		return nil, nil, errors.New("the stream address has a line break")
	}
	removeStaleRTSPDirs()
	dir, err := os.MkdirTemp("", "foxtrack-rtsp-") // mode 0700
	if err != nil {
		return nil, nil, fmt.Errorf("making a folder for ffmpeg's input: %w", err)
	}
	remove = func() { os.RemoveAll(dir) }
	// Inside single quotes everything is literal: close the quote, escape one, reopen.
	quoted := strings.ReplaceAll(streamURL, "'", `'\''`)
	script := "ffconcat version 1.0\nfile '" + quoted + "'\noption rtsp_transport tcp\n"
	path := filepath.Join(dir, "input.ffconcat")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		remove()
		return nil, nil, fmt.Errorf("writing ffmpeg's input: %w", err)
	}
	return rtspArgs(path, oneShot), remove, nil
}

// splitJPEG reads concatenated JPEGs from r and calls emit with each one,
// cut at the start (FF D8) and end (FF D9) markers. Inside a JPEG's image
// data FF is always followed by 00 or a restart marker, so FF D9 only ends
// a picture. It returns nil at the end of r.
func splitJPEG(r io.Reader, emit func(frame []byte)) error {
	const maxFrame = 16 << 20
	soi, eoi := []byte{0xFF, 0xD8}, []byte{0xFF, 0xD9}
	var buf []byte
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			s := bytes.Index(buf, soi)
			if s < 0 {
				// Keep a trailing FF: the D8 may arrive in the next read.
				if len(buf) > 0 && buf[len(buf)-1] == 0xFF {
					buf = append(buf[:0], 0xFF)
				} else {
					buf = buf[:0]
				}
				break
			}
			e := bytes.Index(buf[s+2:], eoi)
			if e < 0 {
				buf = buf[s:]
				break
			}
			end := s + 2 + e + 2
			emit(append([]byte(nil), buf[s:end]...))
			buf = buf[end:]
		}
		if len(buf) > maxFrame {
			buf = buf[:0] // not a picture
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// tailBuffer keeps the end of ffmpeg's error output, which says why it died.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 1024 {
		t.b = t.b[len(t.b)-1024:]
	}
	return len(p), nil
}

// lastLine is the last non-empty line of ffmpeg's output, access code masked.
func (t *tailBuffer) lastLine(code string) string {
	lines := strings.Split(strings.TrimSpace(string(t.b)), "\n")
	return maskCode(strings.TrimSpace(lines[len(lines)-1]), code)
}

// rtspHub is the one ffmpeg of one printer and the viewers sharing it.
type rtspHub struct {
	name, code, bin string
	url             string // holds the access code
	cancel          context.CancelFunc

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	last    []byte
	lastAt  time.Time
	failure string // why the last run ended, access code masked
	idle    *time.Timer
	stopped bool
}

var (
	rtspMu    sync.Mutex
	rtspHubs  = map[string]*rtspHub{} // by printer name
	rtspSems  = map[string]chan struct{}{}
	rtspRoot  context.Context
	rtspClose context.CancelFunc
)

func init() { rtspRoot, rtspClose = context.WithCancel(context.Background()) }

// rtspShutdown stops every ffmpeg the camera code started. Call it when the
// Bridge exits.
func rtspShutdown() { rtspClose() }

// rtspSub is one viewer's feed of frames.
type rtspSub struct {
	ch  chan []byte
	hub *rtspHub
}

func (s *rtspSub) close() { s.hub.remove(s.ch) }

// failure is why the stream last failed, for the error shown to the viewer.
func (s *rtspSub) failure() string {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.hub.failure
}

// rtspSubscribe joins the printer's shared stream, starting ffmpeg if no one
// is watching yet.
func rtspSubscribe(p config.Printer, ip string) (*rtspSub, error) {
	bin := ffmpegPath()
	if bin == "" {
		return nil, errors.New(rtspNeedsFFmpeg)
	}
	u := rtspURL(ip, p.LANCode).String()

	rtspMu.Lock()
	defer rtspMu.Unlock()
	h := rtspHubs[p.Name]
	if h != nil && (h.url != u || h.bin != bin) {
		h.stopLocked() // the address, access code or ffmpeg changed
		h = nil
	}
	if h == nil {
		ctx, cancel := context.WithCancel(rtspRoot)
		h = &rtspHub{name: p.Name, code: p.LANCode, bin: bin, url: u, cancel: cancel, subs: map[chan []byte]struct{}{}}
		rtspHubs[p.Name] = h
		log.Printf("[camera/%s] starting ffmpeg for %s", p.Name, rtspURL(ip, p.LANCode).Redacted())
		go h.run(ctx)
	}
	return h.add(), nil
}

func (h *rtspHub) add() *rtspSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idle != nil {
		h.idle.Stop()
		h.idle = nil
	}
	ch := make(chan []byte, 2)
	h.subs[ch] = struct{}{}
	if h.last != nil && time.Since(h.lastAt) < rtspFreshFrame {
		ch <- h.last // a viewer joining a running stream does not wait for the next frame
	}
	return &rtspSub{ch: ch, hub: h}
}

func (h *rtspHub) remove(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	delete(h.subs, ch)
	if len(h.subs) == 0 && h.idle == nil {
		h.idle = time.AfterFunc(rtspIdleStop, h.idleStop)
	}
}

// idleStop ends the hub if no one came back in time.
func (h *rtspHub) idleStop() {
	rtspMu.Lock()
	defer rtspMu.Unlock()
	h.mu.Lock()
	idle := !h.stopped && len(h.subs) == 0
	h.mu.Unlock()
	if idle {
		h.stopLocked()
		log.Printf("[camera/%s] no viewers; ffmpeg stopped", h.name)
	}
}

// stopLocked ends the hub: ffmpeg is killed and the viewers' feeds close. The
// caller holds rtspMu.
func (h *rtspHub) stopLocked() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.stopped = true
	if h.idle != nil {
		h.idle.Stop()
	}
	h.cancel()
	for ch := range h.subs {
		close(ch)
	}
	h.subs = nil
	if rtspHubs[h.name] == h {
		delete(rtspHubs, h.name)
	}
}

// publish hands a frame to every viewer. A slow viewer skips frames: its
// oldest waiting one is dropped for the new one.
func (h *rtspHub) publish(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.last, h.lastAt = frame, time.Now()
	for ch := range h.subs {
		select {
		case ch <- frame:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- frame:
			default:
			}
		}
	}
}

// run keeps ffmpeg alive until ctx ends, restarting it with a growing pause
// when it dies.
func (h *rtspHub) run(ctx context.Context) {
	backoff := rtspBackoffMin
	for ctx.Err() == nil {
		got, why := h.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		h.mu.Lock()
		h.failure = why
		h.mu.Unlock()
		if got {
			backoff = rtspBackoffMin
		} else {
			backoff = min(backoff*2, rtspBackoffMax)
		}
		log.Printf("[camera/%s] ffmpeg stopped (%s); restarting in %s", h.name, why, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// runOnce runs ffmpeg until it ends, stalls or ctx is cancelled. got reports
// whether it delivered any picture.
func (h *rtspHub) runOnce(ctx context.Context) (got bool, why string) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args, remove, err := rtspInput(h.url, false)
	if err != nil {
		return false, err.Error()
	}
	defer remove()
	cmd := exec.CommandContext(pctx, h.bin, args...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return false, maskCode(err.Error(), h.code)
	}
	watchdog := time.AfterFunc(rtspStallTimeout, cancel)
	defer watchdog.Stop()

	_ = splitJPEG(out, func(frame []byte) {
		got = true
		watchdog.Reset(rtspStallTimeout)
		h.publish(frame)
	})
	cancel() // the pipe ended or broke: make sure the process is gone
	werr := cmd.Wait()
	switch {
	case stderr.lastLine(h.code) != "":
		why = stderr.lastLine(h.code)
	case werr != nil:
		why = maskCode(werr.Error(), h.code)
	default:
		why = "ffmpeg ended"
	}
	return got, why
}

// rtspLatest returns the running stream's newest frame if it is fresh.
func rtspLatest(name string) []byte {
	rtspMu.Lock()
	h := rtspHubs[name]
	rtspMu.Unlock()
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last != nil && time.Since(h.lastAt) < rtspFreshFrame {
		return h.last
	}
	return nil
}

// rtspCameraStream serves the printer's RTSP camera to the dashboard as MJPEG,
// in the same multipart form as the port-6000 stream.
func rtspCameraStream(w http.ResponseWriter, r *http.Request, p config.Printer, ip string) {
	sub, err := rtspSubscribe(p, ip)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer sub.close()

	wait := time.NewTimer(rtspFirstFrameTimeout)
	defer wait.Stop()
	var frame []byte
	select {
	case f, ok := <-sub.ch:
		if !ok {
			writeErr(w, http.StatusBadGateway, "camera unavailable")
			return
		}
		frame = f
	case <-wait.C:
		msg := rtspNoFrames
		if why := sub.failure(); why != "" {
			msg += " (" + why + ")"
		}
		log.Printf("[camera/%s] %s", p.Name, msg)
		writeErr(w, http.StatusBadGateway, msg)
		return
	case <-r.Context().Done():
		return
	}

	const boundary = "bambu"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	stall := time.NewTimer(rtspViewerStall)
	defer stall.Stop()
	for {
		fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if _, err := w.Write(frame); err != nil {
			return // client disconnected
		}
		fmt.Fprint(w, "\r\n")
		if flusher != nil {
			flusher.Flush()
		}
		stall.Reset(rtspViewerStall)
		select {
		case f, ok := <-sub.ch:
			if !ok {
				return
			}
			frame = f
		case <-stall.C:
			log.Printf("[camera/%s] no picture for %s; viewer dropped", p.Name, rtspViewerStall)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// rtspSnapshot reads one JPEG from the printer's RTSP camera: the running
// stream's latest frame when someone is watching, else a one-shot ffmpeg.
func rtspSnapshot(ctx context.Context, p config.Printer, ip string) ([]byte, error) {
	bin := ffmpegPath()
	if bin == "" {
		return nil, errors.New(rtspNeedsFFmpeg)
	}
	if f := rtspLatest(p.Name); f != nil {
		return f, nil
	}

	// One one-shot at a time per printer: the printer allows few sessions.
	rtspMu.Lock()
	sem := rtspSems[p.Name]
	if sem == nil {
		sem = make(chan struct{}, 1)
		rtspSems[p.Name] = sem
	}
	rtspMu.Unlock()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f := rtspLatest(p.Name); f != nil {
		return f, nil
	}

	ctx, cancel := context.WithTimeout(ctx, rtspSnapshotTimeout)
	defer cancel()
	args, remove, err := rtspInput(rtspURL(ip, p.LANCode).String(), true)
	if err != nil {
		return nil, fmt.Errorf("camera snapshot failed: %w", err)
	}
	defer remove()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("the printer's camera did not answer in time")
		}
		if line := stderr.lastLine(p.LANCode); line != "" {
			return nil, fmt.Errorf("camera snapshot failed: %s", line)
		}
		return nil, fmt.Errorf("camera snapshot failed: %s", maskCode(err.Error(), p.LANCode))
	}
	b := out.Bytes()
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errors.New("the printer's camera sent no picture")
	}
	return b, nil
}
