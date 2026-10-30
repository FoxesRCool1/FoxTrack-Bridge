package virtualprinter

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type forwarded struct {
	name string
	body string
	size int64
}

func fakeForward(status int, reply string, got *forwarded) Forwarder {
	return func(_ context.Context, name string, body io.Reader, size int64) (int, []byte, error) {
		b, _ := io.ReadAll(body)
		*got = forwarded{name: name, body: string(b), size: size}
		return status, []byte(reply), nil
	}
}

func newServer(forward Forwarder) *Server {
	s := &Server{}
	s.mu.Lock()
	s.key, s.forward = "secret-key", forward
	s.mu.Unlock()
	return s
}

func multipartUpload(t *testing.T, fileName, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("select", "true")
	_ = mw.WriteField("print", "true")
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(fw, content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestVersion_NeedsKey(t *testing.T) {
	s := newServer(nil)
	req := httptest.NewRequest("GET", "/api/version", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", rec.Code)
	}
	req.Header.Set("X-Api-Key", "secret-key")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"OctoPrint 1.9.0 (FoxTrack Bridge)"`) {
		t.Fatalf("version: %d %s", rec.Code, rec.Body.String())
	}
	// ?apikey= works too, as in OctoPrint.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/version?apikey=secret-key", nil))
	if rec.Code != 200 {
		t.Fatalf("query key: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/settings?apikey=secret-key", nil))
	if rec.Code != 404 {
		t.Fatalf("other path: %d", rec.Code)
	}
}

func TestUpload_ForwardsFileAndAnswers201(t *testing.T) {
	var got forwarded
	s := newServer(fakeForward(200, `{"ok":true,"id":"abc"}`, &got))
	body, ct := multipartUpload(t, "prints/Bracket v3 (PLA).gcode", "G28\nG1 X5\n")
	req := httptest.NewRequest("POST", "/api/files/local", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Api-Key", "secret-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"name":"Bracket v3 (PLA).gcode"`) {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if got.name != "Bracket v3 (PLA).gcode" || got.body != "G28\nG1 X5\n" || got.size != 10 {
		t.Fatalf("forwarded = %+v", got)
	}
}

func TestUpload_MapsFoxTrackAnswers(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		text   string
	}{
		{415, `{"error":"Only .gcode"}`, 415, ".gcode and .gcode.3mf"},
		{413, ``, 413, "larger than 50 MB"},
		{507, `{"error":"storage_full"}`, 507, "storage is full"},
		{429, `{"error":"too_many_uploads"}`, 429, "50 files are waiting"},
		{403, `{"error":"plan_limit"}`, 403, "plan does not include"},
		{500, `Internal error`, 502, "did not take the file"},
		{404, ``, 502, "did not take the file"},
	}
	for _, tc := range cases {
		var got forwarded
		s := newServer(fakeForward(tc.status, tc.body, &got))
		body, ct := multipartUpload(t, "a.gcode", "G28")
		req := httptest.NewRequest("POST", "/api/files/sdcard", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Api-Key", "secret-key")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.text) {
			t.Errorf("FoxTrack %d: slicer got %d %q", tc.status, rec.Code, rec.Body.String())
		}
	}
}

func TestUpload_NotConnectedAndTooBig(t *testing.T) {
	s := newServer(nil)
	body, ct := multipartUpload(t, "a.gcode", "G28")
	req := httptest.NewRequest("POST", "/api/files/local", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Api-Key", "secret-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not connected to FoxTrack") {
		t.Fatalf("standalone: %d %s", rec.Code, rec.Body.String())
	}

	var got forwarded
	s = newServer(fakeForward(200, `{"ok":true}`, &got))
	req = httptest.NewRequest("POST", "/api/files/local", strings.NewReader(""))
	req.ContentLength = MaxFileBytes + (1 << 20)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Api-Key", "secret-key")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || got.size != 0 {
		t.Fatalf("too big: %d, forwarded %+v", rec.Code, got)
	}
}

func TestFileName_LastPartOnly(t *testing.T) {
	for in, want := range map[string]string{"prints/sub/part.gcode": "part.gcode", `C:\Users\me\part.gcode`: "part.gcode", "  Ünïcode ✓.gcode.3mf ": "Ünïcode ✓.gcode.3mf"} {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q", in, got)
		}
	}
}
