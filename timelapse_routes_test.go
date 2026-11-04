//go:build headless

package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/timelapse"
)

func timelapseTestConfig(t *testing.T) {
	t.Helper()
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{APIKey: "fox", Printers: aiPrinters()}
	configMutex.Unlock()
}

func timelapseGet(t *testing.T) timelapseView {
	t.Helper()
	rec := httptest.NewRecorder()
	handleTimelapse(rec, httptest.NewRequest("GET", "/api/timelapse", nil))
	if rec.Code != 200 {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	var v timelapseView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func timelapsePost(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleTimelapse(rec, jsonPost("/api/timelapse", body))
	return rec
}

func TestTimelapseSettings(t *testing.T) {
	timelapseTestConfig(t)
	v := timelapseGet(t)
	if v.Enabled || v.IntervalSec != 30 || v.KeepDays != 30 || len(v.Printers) != 0 || v.Items == nil || v.Recording == nil {
		t.Fatalf("defaults = %+v, want off, 30 s, 30 days, all printers, empty lists", v)
	}

	if rec := timelapsePost(`{"enabled":true,"interval_sec":45,"keep_days":0,"printers":["p2","nope"]}`); rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	v = timelapseGet(t)
	if !v.Enabled || v.IntervalSec != 45 || v.KeepDays != 0 || strings.Join(v.Printers, ",") != "p2" {
		t.Errorf("after save = %+v; want on, 45 s, keep all, only the printer that exists", v)
	}
	configMutex.RLock()
	stored := *configStore.Timelapse
	nPrinters := len(configStore.Printers)
	configMutex.RUnlock()
	if stored.KeepDays != -1 {
		t.Errorf("keep-all stored as %d; config.json drops 0, so it must be -1", stored.KeepDays)
	}
	if nPrinters != 2 {
		t.Fatalf("printers = %d: a timelapse save changed the printer list", nPrinters)
	}

	// A field left out keeps its stored value.
	if rec := timelapsePost(`{"enabled":false}`); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	v = timelapseGet(t)
	if v.Enabled || v.IntervalSec != 45 || v.KeepDays != 0 || len(v.Printers) != 1 {
		t.Errorf("a partial save changed other settings: %+v", v)
	}
	// A list of printers that are all gone is refused: stored empty it would mean every printer.
	if rec := timelapsePost(`{"printers":["nope","gone"]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "None of those printers exist") {
		t.Errorf("only unknown printers = %d %q, want 400 with a plain message", rec.Code, rec.Body)
	}
	if v = timelapseGet(t); strings.Join(v.Printers, ",") != "p2" {
		t.Errorf("a refused save changed the filter to %v", v.Printers)
	}
	// An empty list means every printer.
	timelapsePost(`{"printers":[]}`)
	if v = timelapseGet(t); len(v.Printers) != 0 {
		t.Errorf("printers = %v, want none (= all)", v.Printers)
	}

	for body, why := range map[string]string{
		`{"interval_sec":2}`:     "interval under 5 s",
		`{"interval_sec":99999}`: "interval over an hour",
		`{"keep_days":-1}`:       "negative keep days",
		`{"enabled":"yes"}`:      "wrong type",
		`not json`:               "not JSON",
	} {
		if rec := timelapsePost(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", why, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/timelapse", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "text/plain")
	handleTimelapse(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST = %d, want 415", rec.Code)
	}
}

func TestTimelapseFileEndpoints(t *testing.T) {
	timelapseTestConfig(t)
	dir := timelapseDir()
	os.MkdirAll(dir, 0o755)
	const id = "1700000000-abc123"
	video := []byte("0123456789video-bytes")
	os.WriteFile(filepath.Join(dir, id+".mp4"), video, 0o644)
	os.WriteFile(filepath.Join(dir, id+".jpg"), []byte("\xFF\xD8poster\xFF\xD9"), 0o644)
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"id":"`+id+`","printer_name":"P","end":1700000600,"format":"mp4","poster":true,"frames":20}`), 0o644)
	// A file next to the folder that a path trick would reach.
	os.WriteFile(filepath.Join(filepath.Dir(dir), "config.json.bak"), []byte("keep me"), 0o644)

	if v := timelapseGet(t); len(v.Items) != 1 || v.Items[0].ID != id {
		t.Fatalf("items = %+v", v.Items)
	}

	do := func(method, target string, hdr ...string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		handleTimelapseFile(rec, req)
		return rec
	}

	rec := do("GET", "/api/timelapse/file/"+id)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" || rec.Body.String() != string(video) || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("GET = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	// Seeking: the player asks for a byte range.
	rec = do("GET", "/api/timelapse/file/"+id, "Range", "bytes=10-14")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "video" {
		t.Errorf("range GET = %d %q, want 206 \"video\"", rec.Code, rec.Body)
	}
	if rec = do("GET", "/api/timelapse/file/"+id+"?poster=1"); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("poster = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, q := range []string{"?poster=0", "?poster=", "?poster=no"} {
		if rec = do("GET", "/api/timelapse/file/"+id+q); rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
			t.Errorf("%s = %d %q, want the video", q, rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	for _, target := range []string{
		"/api/timelapse/file/", "/api/timelapse/file/..", "/api/timelapse/file/../config.json.bak",
		"/api/timelapse/file/..%2fconfig.json.bak", "/api/timelapse/file/%2e%2e%2fconfig.json.bak",
		"/api/timelapse/file/" + id + ".mp4", "/api/timelapse/file/" + id + "/x",
	} {
		for _, method := range []string{"GET", "DELETE"} {
			if rec := do(method, target); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s = %d, want 400", method, target, rec.Code)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "config.json.bak")); err != nil {
		t.Fatal("a path in the ID reached a file outside the timelapse folder")
	}
	if rec = do("GET", "/api/timelapse/file/1700000001-nothing"); rec.Code != http.StatusNotFound {
		t.Errorf("missing video = %d, want 404", rec.Code)
	}
	if rec = do("POST", "/api/timelapse/file/"+id); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}

	if rec = do("DELETE", "/api/timelapse/file/"+id); rec.Code != 200 {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	if v := timelapseGet(t); len(v.Items) != 0 {
		t.Errorf("items after delete = %+v", v.Items)
	}
	for _, ext := range []string{".mp4", ".jpg", ".json"} {
		if _, err := os.Stat(filepath.Join(dir, id+ext)); err == nil {
			t.Errorf("%s was not deleted", ext)
		}
	}
	if rec = do("DELETE", "/api/timelapse/file/"+id); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", rec.Code)
	}
}

// A print starting begins a recording only when the feature is on and the
// printer is wanted; its end stops it even if the feature was switched off.
// A recording closed because its printer sat idle takes its result from the
// status the printer is in: an error is a failed print, not a cancelled one.
func TestTimelapseIdleEndResult(t *testing.T) {
	var pic bytes.Buffer
	jpeg.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil)
	for status, want := range map[string]string{"error": "failed", "finished": "finished", "idle": "cancelled"} {
		t.Run(status, func(t *testing.T) {
			timelapseTestConfig(t)
			t.Setenv("FOXTRACK_BRIDGE_FFMPEG", "")
			t.Setenv("PATH", t.TempDir()) // no ffmpeg: the video is a GIF
			rec := timelapseRecorder()
			rec.Start(timelapse.Printer{ID: "p1", Name: "Monsieur"}, "a.gcode", false)
			for i := 0; i < timelapse.MinFrames; i++ {
				if err := rec.Capture("p1", 0, func() ([]byte, error) { return pic.Bytes(), nil }); err != nil {
					t.Fatal(err)
				}
			}
			mqttpkg.UpdatePrinterState("Monsieur", mqttpkg.TelemetryData{Status: status})
			t.Cleanup(func() { mqttpkg.RemovePrinterState("Monsieur"); delete(tlIdle, "p1") })

			now := time.Now()
			timelapseTick(now)
			if !rec.Active("p1") {
				t.Fatal("the recording ended before the printer had been idle long enough")
			}
			timelapseTick(now.Add(timelapseIdleEnd))
			waitFor(t, "the video", func() bool { return !rec.Active("p1") && rec.Building() == 0 && len(rec.List()) == 1 })
			if got := rec.List()[0].Result; got != want {
				t.Errorf("printer %s: result %q, want %q", status, got, want)
			}
		})
	}
}

func TestTimelapseEvents(t *testing.T) {
	timelapseTestConfig(t)
	rec := timelapseRecorder()
	start := events.Event{Kind: events.PrintStarted, PrinterID: "p1", Printer: "Monsieur", File: "a.gcode"}

	timelapseEvent(start)
	if rec.Active("p1") {
		t.Fatal("recorded with the feature off")
	}
	timelapsePost(`{"enabled":true,"printers":["p2"]}`)
	timelapseEvent(start)
	if rec.Active("p1") {
		t.Fatal("recorded a printer that is not in the filter")
	}
	timelapsePost(`{"printers":[]}`)
	timelapseEvent(start)
	if !rec.Active("p1") {
		t.Fatal("did not start recording")
	}
	timelapsePost(`{"enabled":false}`)
	timelapseEvent(events.Event{Kind: events.PrintFinished, PrinterID: "p1"})
	if rec.Active("p1") {
		t.Error("the end of a print did not stop the recording after the feature was switched off")
	}

	configMutex.Lock()
	configStore.Printers[0].CameraHidden = true
	configMutex.Unlock()
	timelapsePost(`{"enabled":true}`)
	timelapseEvent(start)
	if rec.Active("p1") {
		t.Error("recorded a printer whose camera is hidden")
	}
}
