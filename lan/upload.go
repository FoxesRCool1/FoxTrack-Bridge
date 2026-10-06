package lan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	configpkg "foxtrack-bridge/config"
)

// errBusy and friends are shown to the FoxTrack user as is.
var (
	errPrinterBusy = errors.New("The printer is busy. Wait until it is idle, then try again.")
	errNotStarted  = errors.New("The printer took the file but did not start it. Check that it is idle and ready.")
	errFileInUse   = errors.New("That file is printing on the printer right now.")
	errMoonKey     = errors.New("Moonraker refused Bridge's API key.")
)

// uploadTimeout is its own long timeout: the 6 s command client cannot
// carry a 50 MB file over a slow Wi-Fi link.
var uploadTimeout = 10 * time.Minute

// UploadAndPrint sends the file at localPath to the Klipper printer called name
// (Moonraker /server/files/upload with print=true) and reports whether the
// printer started it. remoteName is cleaned first. The error text is plain
// English, shown to the FoxTrack user.
func (c *Controller) UploadAndPrint(ctx context.Context, name, localPath, remoteName string) error {
	c.mu.RLock()
	p, ok := c.printers[name]
	state := c.states[name]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("printer %q not found", name)
	}
	if state != nil && (state.Status == "printing" || state.Status == "paused") {
		return errPrinterBusy
	}
	target := moonrakerURL(p, "/server/files/upload")
	if target == "" {
		return errors.New("This printer has no Moonraker address in Bridge.")
	}

	if err := moonrakerKeyPreflight(ctx, p); err != nil {
		return err
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind file: %w", err)
	}
	remote := SanitizeRemoteName(remoteName)

	// Multipart by hand around the open file, so 50 MB is never held in memory
	// and the request still has a Content-Length (Moonraker is happier with it
	// than with chunked bodies).
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	for _, kv := range [][2]string{{"root", "gcodes"}, {"checksum", hex.EncodeToString(h.Sum(nil))}, {"print", "true"}} {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			return err
		}
	}
	if _, err := mw.CreateFormFile("file", remote); err != nil {
		return err
	}
	tail := []byte("\r\n--" + mw.Boundary() + "--\r\n")

	req, err := http.NewRequestWithContext(ctx, "POST", target, io.MultiReader(&head, f, bytes.NewReader(tail)))
	if err != nil {
		return err
	}
	req.ContentLength = int64(head.Len()) + st.Size() + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	applyMoonrakerAuth(req, p)

	client := &http.Client{Timeout: uploadTimeout, Transport: insecureTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("The printer could not be reached while sending the file (%v).", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errMoonKey
	case resp.StatusCode == http.StatusForbidden:
		if strings.Contains(strings.ToLower(string(body)), "in use") {
			return errFileInUse
		}
		return errMoonKey
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("The printer refused the file (HTTP %d).", resp.StatusCode)
	}
	// Moonraker's file_manager returns {"item":..., "action":..., "print_started":
	// ..., "print_queued": ...} and application.py's FileUploadHandler writes
	// that dict as is, so the reply is the bare object (read from the source,
	// 2026-10-06). The JSON-RPC style {"result":{...}} wrapper is accepted too in
	// case a proxy or another version adds it.
	var reply struct {
		PrintStarted bool `json:"print_started"`
		PrintQueued  bool `json:"print_queued"`
		Result       *struct {
			PrintStarted bool `json:"print_started"`
			PrintQueued  bool `json:"print_queued"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return errNotStarted
	}
	started, queued := reply.PrintStarted, reply.PrintQueued
	if reply.Result != nil {
		started, queued = started || reply.Result.PrintStarted, queued || reply.Result.PrintQueued
	}
	if !started && !queued {
		return errNotStarted
	}
	return nil
}

// moonrakerKeyPreflight makes one authenticated GET before the upload.
// Moonraker answers a bad API key with 401 before it reads the body, which a
// client that is still sending 50 MB sees as a broken pipe instead of a
// status. A failure that is not 401/403 is left to the upload to report.
func moonrakerKeyPreflight(ctx context.Context, p configpkg.Printer) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", moonrakerURL(p, "/server/info"), nil)
	if err != nil {
		return nil
	}
	applyMoonrakerAuth(req, p)
	resp, err := (&http.Client{Transport: insecureTransport()}).Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errMoonKey
	}
	return nil
}

// SanitizeRemoteName keeps [A-Za-z0-9._-], turns spaces into underscores,
// ends in .gcode and stays within 120 bytes.
func SanitizeRemoteName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == ' ':
			b.WriteByte('_')
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	s := strings.TrimLeft(b.String(), ".")
	if !strings.HasSuffix(strings.ToLower(s), ".gcode") {
		s = strings.TrimRight(s, ".") + ".gcode"
	}
	if len(s) > 120 {
		s = s[:120-len(".gcode")] + ".gcode"
	}
	if s == ".gcode" {
		s = "print.gcode"
	}
	return s
}
