package virtualprinter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// The virtual printer looks like an OctoPrint server to OrcaSlicer and
// PrusaSlicer ("Physical printer" / "Send to print host", host type
// OctoPrint). The slicer's upload lands in FoxTrack, never on a printer. Only
// the calls the two slicers make are answered; everything else is 404.

// MaxFileBytes is FoxTrack's cap on a sliced file.
const MaxFileBytes = 50 << 20

// Forwarder sends one file to FoxTrack and returns FoxTrack's HTTP status
// and body. It is nil while the Bridge is not connected to FoxTrack.
type Forwarder func(ctx context.Context, fileName string, body io.Reader, size int64) (int, []byte, error)

// Server is the one virtual printer. Apply starts, moves or stops it.
type Server struct {
	mu      sync.Mutex
	port    int
	key     string
	forward Forwarder
	srv     *http.Server
	ln      net.Listener
}

// Apply makes the server match the settings: off, or on at port with key.
// The forwarder may change without a restart (connecting FoxTrack later).
func (s *Server) Apply(enabled bool, port int, key string, forward Forwarder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forward = forward
	if !enabled || key == "" {
		s.stopLocked()
		return
	}
	if s.srv != nil && s.port == port && s.key == key {
		return
	}
	s.stopLocked()
	s.port, s.key = port, key
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("[virtual-printer] cannot listen on port %d: %v", port, err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.ln = ln
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[virtual-printer] server stopped: %v", err)
		}
	}(s.srv, ln)
	log.Printf("[virtual-printer] listening on port %d (OctoPrint API for slicers)", port)
}

// Running reports whether the port is open.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv != nil
}

func (s *Server) stopLocked() {
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.srv.Shutdown(ctx)
	cancel()
	s.srv, s.ln = nil, nil
	log.Printf("[virtual-printer] stopped")
}

// Handler returns the HTTP handler, for tests.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key, forward := s.key, s.forward
	s.mu.Unlock()

	switch {
	case r.URL.Path == "/api/version" && r.Method == "GET":
		if !s.authorized(r, key) {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"api":"0.1","server":"1.9.0","text":"OctoPrint 1.9.0 (FoxTrack Bridge)"}`)
	case (r.URL.Path == "/api/files/local" || r.URL.Path == "/api/files/sdcard") && r.Method == "POST":
		if !s.authorized(r, key) {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}
		s.upload(w, r, forward)
	default:
		http.NotFound(w, r)
	}
}

// authorized checks X-Api-Key, or ?apikey= as OctoPrint also allows.
func (s *Server) authorized(r *http.Request, key string) bool {
	if key == "" {
		return false
	}
	got := r.Header.Get("X-Api-Key")
	if got == "" {
		got = r.URL.Query().Get("apikey")
	}
	return got == key
}

// upload reads the multipart "file" part into a temp file (so the size is
// known and checked before FoxTrack sees a byte), forwards it, and maps
// FoxTrack's answer onto what the slicer shows.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, forward Forwarder) {
	if forward == nil {
		http.Error(w, "The Bridge is not connected to FoxTrack. Paste a Bridge token in the Bridge dashboard first.", http.StatusBadGateway)
		return
	}
	// The whole request is at least the file, so an oversized Content-Length is
	// refused without reading it.
	if r.ContentLength > MaxFileBytes+(64<<10) {
		http.Error(w, "The file is larger than 50 MB.", http.StatusRequestEntityTooLarge)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected a multipart upload with a file part.", http.StatusBadRequest)
		return
	}
	var name string
	var tmp *os.File
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "Bad multipart upload.", http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		name = FileName(part.FileName())
		tmp, err = os.CreateTemp("", "foxtrack-slicer-*.upload")
		if err != nil {
			http.Error(w, "The Bridge could not store the file.", http.StatusInternalServerError)
			return
		}
		n, err := io.Copy(tmp, io.LimitReader(part, MaxFileBytes+1))
		if err != nil {
			cleanup(tmp)
			http.Error(w, "The upload was cut short.", http.StatusBadRequest)
			return
		}
		if n > MaxFileBytes {
			cleanup(tmp)
			http.Error(w, "The file is larger than 50 MB.", http.StatusRequestEntityTooLarge)
			return
		}
		break
	}
	if tmp == nil {
		http.Error(w, "No file part in the upload.", http.StatusBadRequest)
		return
	}
	defer cleanup(tmp)
	if name == "" {
		http.Error(w, "The file has no name.", http.StatusBadRequest)
		return
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil || size == 0 {
		http.Error(w, "The file is empty.", http.StatusBadRequest)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "The Bridge could not read the file back.", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	status, body, err := forward(ctx, name, tmp, size)
	if err != nil {
		log.Printf("[virtual-printer] %s: FoxTrack unreachable: %v", name, err)
		http.Error(w, "FoxTrack did not take the file. Try again.", http.StatusBadGateway)
		return
	}
	code, msg := Answer(status, body)
	if code != http.StatusCreated {
		log.Printf("[virtual-printer] %s: FoxTrack answered %d %.200s", name, status, body)
		http.Error(w, msg, code)
		return
	}
	log.Printf("[virtual-printer] %s (%d bytes) sent to FoxTrack", name, size)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"done":true,"files":{"local":{"name":%q,"origin":"local","refs":{}}}}`, name)
}

func cleanup(f *os.File) {
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
}

// FileName keeps only the last path part of what the slicer sent (it may
// include folders) and trims it. The name is sent to FoxTrack URI-encoded.
func FileName(raw string) string {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	return strings.TrimSpace(path.Base(raw))
}

// Answer maps FoxTrack's status onto the code and text the slicer shows.
func Answer(status int, body []byte) (int, string) {
	b := strings.ToLower(string(body))
	switch {
	case status >= 200 && status < 300:
		return http.StatusCreated, ""
	case status == http.StatusUnsupportedMediaType:
		return http.StatusUnsupportedMediaType, "FoxTrack takes .gcode and .gcode.3mf files."
	case status == http.StatusRequestEntityTooLarge:
		return http.StatusRequestEntityTooLarge, "The file is larger than 50 MB."
	case status == http.StatusInsufficientStorage || strings.Contains(b, "storage_full"):
		return http.StatusInsufficientStorage, "Your FoxTrack print file storage is full."
	case status == http.StatusTooManyRequests || strings.Contains(b, "too_many_uploads"):
		return http.StatusTooManyRequests, "50 files are waiting in FoxTrack. Add or delete some first."
	case status == http.StatusForbidden || strings.Contains(b, "plan_limit"):
		return http.StatusForbidden, "Your FoxTrack plan does not include FoxTrack Bridge."
	case status == http.StatusUnauthorized:
		return http.StatusBadGateway, "FoxTrack refused the Bridge token. Check it in the Bridge dashboard."
	}
	return http.StatusBadGateway, "FoxTrack did not take the file. Try again."
}
