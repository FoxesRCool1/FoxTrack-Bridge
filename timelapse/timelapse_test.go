package timelapse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testPrinter = Printer{ID: "abc123", Name: "Voron"}

// testJPEG is a real, small picture: a gradient w x h, shifted by n so
// pictures differ.
func testJPEG(t testing.TB, w, h, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x + n*7), uint8(y * 2), uint8(n * 20), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// marker is a minimal picture that carries n, for tests that only count.
func marker(n int) []byte { return []byte(fmt.Sprintf("\xFF\xD8%06d\xFF\xD9", n)) }

// newTestRecorder returns a recorder on a temp dir with a clock the test moves.
func newTestRecorder(t *testing.T, ffmpeg func() string) (*Recorder, *time.Time) {
	t.Helper()
	r := New(t.TempDir(), ffmpeg)
	clock := time.Now()
	r.now = func() time.Time { return clock }
	return r, &clock
}

func waitBuilt(t *testing.T, r *Recorder) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r.Building() == 0 {
			return
		}
	}
	t.Fatal("the build did not finish")
}

func capture(t *testing.T, r *Recorder, clock *time.Time, n int, interval time.Duration, pic func(i int) []byte) {
	t.Helper()
	for i := 1; i <= n; i++ {
		*clock = clock.Add(interval)
		if err := r.Capture(testPrinter.ID, interval, func() ([]byte, error) { return pic(i), nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func readFrame(t *testing.T, dir string, i int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, frameName(i)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data[2 : len(data)-2])
}

func TestDefaults(t *testing.T) {
	if MaxFrames != 3000 {
		t.Errorf("MaxFrames = %d, want 3000", MaxFrames)
	}
	if r := New(t.TempDir(), nil); r.maxFrames != MaxFrames {
		t.Errorf("a new recorder caps at %d, want %d", r.maxFrames, MaxFrames)
	}
	for stored, want := range map[int]int{0: 30, -3: 30, 1: 5, 4: 5, 5: 5, 45: 45, 3600: 3600, 99999: 3600} {
		if got := Interval(stored); got != want {
			t.Errorf("Interval(%d) = %d, want %d", stored, got, want)
		}
	}
	for stored, want := range map[int]int{0: 30, -1: 0, 7: 7, 99999: 3650} {
		if got := KeepDays(stored); got != want {
			t.Errorf("KeepDays(%d) = %d, want %d", stored, got, want)
		}
	}
	// "Keep them all" must survive omitempty: it is stored as -1, never 0.
	if StoredKeepDays(0) != -1 || StoredKeepDays(30) != 30 || KeepDays(StoredKeepDays(0)) != 0 {
		t.Error("keep-forever does not round-trip through the stored value")
	}
}

// At the cap every other picture goes and the spacing doubles, so the video
// stays evenly paced.
func TestFrameCapThinsAndKeepsSpacingEven(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	r.maxFrames = 20
	r.Start(testPrinter, "cube.gcode", false)
	interval := 30 * time.Second

	capture(t, r, clock, 20, interval, func(i int) []byte { return marker(i) })

	dir := r.sessions[testPrinter.ID].dir
	if got := frameFiles(dir); len(got) != 10 {
		t.Fatalf("%d pictures after reaching the cap of 20, want 10: %v", len(got), got)
	}
	for k := 1; k <= 10; k++ {
		if got, want := readFrame(t, dir, k), fmt.Sprintf("%06d", 2*k-1); got != want {
			t.Errorf("picture %d holds %s, want the original %s (every other one kept, no gaps)", k, got, want)
		}
	}
	if s := r.sessions[testPrinter.ID]; s.st.Stride != 2 || s.count != 10 {
		t.Fatalf("stride %d, count %d; want 2 and 10", s.st.Stride, s.count)
	}
	if st, ok := readState(dir); !ok || st.Stride != 2 {
		t.Errorf("the stride was not saved: %+v", st)
	}

	// From here a picture is taken every second tick of the interval.
	capture(t, r, clock, 10, interval, func(i int) []byte { return marker(100 + i) })
	if got := len(frameFiles(dir)); got != 15 {
		t.Errorf("%d pictures after 10 more ticks at double spacing, want 15", got)
	}
}

func TestCaptureIsDueOnceAndOnlyWhileRecording(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	calls := 0
	snap := func() ([]byte, error) { calls++; return marker(calls), nil }
	if err := r.Capture(testPrinter.ID, 30*time.Second, snap); err != nil || calls != 0 {
		t.Fatalf("captured with no print recording (calls=%d, err=%v)", calls, err)
	}
	r.Start(testPrinter, "a.gcode", false)
	r.Capture(testPrinter.ID, 30*time.Second, snap) // the first is due at once
	r.Capture(testPrinter.ID, 30*time.Second, snap) // not yet
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 within one interval", calls)
	}
	*clock = clock.Add(31 * time.Second)
	r.Capture(testPrinter.ID, 30*time.Second, snap)
	if calls != 2 {
		t.Errorf("calls = %d, want 2 after the interval", calls)
	}
}

func TestCaptureRefusesNonJPEGAndDropsPicturesAfterTheEnd(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	r.Start(testPrinter, "a.gcode", false)
	*clock = clock.Add(time.Minute)
	err := r.Capture(testPrinter.ID, 30*time.Second, func() ([]byte, error) { return []byte("<html>no camera</html>"), nil })
	if err == nil {
		t.Error("stored something that is not a JPEG")
	}
	*clock = clock.Add(time.Minute)
	err = r.Capture(testPrinter.ID, 30*time.Second, func() ([]byte, error) {
		r.End(testPrinter.ID, "cancelled") // the print ends while the camera answers
		return marker(1), nil
	})
	if err != nil || r.Active(testPrinter.ID) {
		t.Errorf("err=%v active=%v", err, r.Active(testPrinter.ID))
	}
	if _, statErr := os.Stat(filepath.Join(r.dir, safeName(testPrinter.ID))); statErr == nil {
		t.Error("a picture arriving after the end left a folder behind")
	}
}

func TestSample(t *testing.T) {
	if got := sample(10, 300); len(got) != 10 || got[0] != 1 || got[9] != 10 {
		t.Errorf("sample(10, 300) = %v", got)
	}
	got := sample(1000, 300)
	if len(got) != 300 || got[0] != 1 || got[299] != 1000 {
		t.Fatalf("sample(1000, 300): len %d, first %d, last %d", len(got), got[0], got[len(got)-1])
	}
	for i := 1; i < len(got); i++ {
		if d := got[i] - got[i-1]; d < 3 || d > 4 {
			t.Fatalf("uneven step %d at %d (1000/300 is between 3 and 4)", d, i)
		}
	}
}

func TestEndWithTooFewPicturesDeletesThem(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	r.Start(testPrinter, "a.gcode", false)
	capture(t, r, clock, MinFrames-1, 5*time.Second, marker)
	r.End(testPrinter.ID, "cancelled")
	if entries, _ := os.ReadDir(r.dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if r.Building() != 0 {
		t.Error("queued a video for a handful of pictures")
	}
}

func buildOne(t *testing.T, r *Recorder, clock *time.Time, n, w, h int) Meta {
	t.Helper()
	r.Start(testPrinter, "benchy.gcode", false)
	capture(t, r, clock, n, 5*time.Second, func(i int) []byte { return testJPEG(t, w, h, i) })
	dir := r.sessions[testPrinter.ID].dir
	r.End(testPrinter.ID, "finished")
	waitBuilt(t, r)
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("%d videos listed, want 1", len(list))
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the pictures were not deleted after the build")
	}
	return list[0]
}

// Without ffmpeg the video is an animated GIF made with the standard library.
func TestGIFFallbackWithoutFFmpeg(t *testing.T) {
	r, clock := newTestRecorder(t, func() string { return "" })
	m := buildOne(t, r, clock, 10, 660, 120)

	if m.Format != "gif" || m.Frames != 10 || m.Result != "finished" || m.PrinterName != "Voron" || m.File != "benchy.gcode" {
		t.Fatalf("meta = %+v", m)
	}
	if m.Poster {
		t.Error("a GIF needs no poster")
	}
	path, _, err := r.Open(m.ID, false)
	if err != nil || filepath.Ext(path) != ".gif" {
		t.Fatalf("Open = %q, %v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		t.Fatalf("not a GIF: %v", err)
	}
	if len(g.Image) != 10 {
		t.Errorf("%d GIF frames, want 10", len(g.Image))
	}
	if b := g.Image[0].Bounds(); b.Dx() != 640 || b.Dy() != 116 {
		t.Errorf("frame size %dx%d, want 640x116 (660x120 scaled to 640 wide)", b.Dx(), b.Dy())
	}
	if fi, _ := os.Stat(path); m.Size != fi.Size() || m.Size == 0 {
		t.Errorf("size = %d, file is %d", m.Size, fi.Size())
	}
	if want := 10 * 4.0 / 100; m.Duration != want {
		t.Errorf("duration = %v, want %v", m.Duration, want)
	}
	// The details are saved next to the video for the gallery.
	var onDisk Meta
	data, _ := os.ReadFile(filepath.Join(r.dir, m.ID+".json"))
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.ID != m.ID || onDisk.Frames != 10 {
		t.Errorf("saved details: %+v, %v", onDisk, err)
	}
}

func TestSmallPicturesAreNotScaledUp(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	if scaleDown(img, 640) != image.Image(img) {
		t.Error("a picture narrower than the limit was copied or resized")
	}
}

func TestFFmpegFailureFallsBackToGIF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/false")
	}
	r, clock := newTestRecorder(t, func() string { return "/bin/false" })
	if m := buildOne(t, r, clock, 10, 64, 48); m.Format != "gif" {
		t.Errorf("format = %q, want gif when ffmpeg fails", m.Format)
	}
}

// With a real ffmpeg: H.264, even dimensions from odd ones, faststart, poster.
func TestMP4WithFFmpeg(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	r, clock := newTestRecorder(t, func() string { return bin })
	m := buildOne(t, r, clock, 12, 321, 201)
	if m.Format != "mp4" || !m.Poster || m.Frames != 12 || m.Duration != 12.0/30 {
		t.Fatalf("meta = %+v", m)
	}
	path, _, _ := r.Open(m.ID, false)
	data, _ := os.ReadFile(path)
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		t.Fatalf("not an MP4: % x", data[:min(12, len(data))])
	}
	if i, j := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat")); i < 0 || j < 0 || i > j {
		t.Errorf("moov at %d, mdat at %d: not faststart", i, j)
	}
	if poster, _, err := r.Open(m.ID, true); err != nil || filepath.Ext(poster) != ".jpg" {
		t.Errorf("poster = %q, %v", poster, err)
	}
	if probe, err := exec.LookPath("ffprobe"); err == nil {
		out, err := exec.Command(probe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name,width,height,pix_fmt", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		// 321x201 is odd; yuv420p H.264 needs even sides.
		if got := strings.TrimSpace(string(out)); got != "h264,320,200,yuv420p" {
			t.Errorf("ffprobe says %q, want h264,320,200,yuv420p", got)
		}
	}
}

func TestIDValidation(t *testing.T) {
	good := []string{"1700000000-abc123", "1-a", "1700000000-0123456789abcdef0123456789abcdef", "1700000000-fallback-ab_c"}
	bad := []string{"", ".", "..", "../x", "../../etc/passwd", "1700000000-../x", "1700000000-a/b", `1700000000-a\b`,
		"1700000000-", "-abc", "abc-def", "1700000000-a.json", "1700000000-a.mp4", "1700000000-a b", "%2e%2e%2f", "1700000000-a%2fb",
		"1700000000-a\x00", " 1700000000-a", "1700000000-" + strings.Repeat("a", 65), strings.Repeat("1", 13) + "-a"}
	for _, id := range good {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
	for _, id := range bad {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	// A bad ID never reaches the disk: the file outside the dir survives.
	r, _ := newTestRecorder(t, nil)
	outside := filepath.Join(filepath.Dir(r.dir), "secret.json")
	os.WriteFile(outside, []byte(`{"format":"gif"}`), 0o644)
	for _, id := range []string{"../secret", "..%2fsecret", "../" + filepath.Base(outside)} {
		if _, _, err := r.Open(id, false); err != ErrBadID {
			t.Errorf("Open(%q) = %v, want ErrBadID", id, err)
		}
		if err := r.Delete(id); err != ErrBadID {
			t.Errorf("Delete(%q) = %v, want ErrBadID", id, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a path in the ID reached a file outside the timelapse folder")
	}
	if _, _, err := r.Open("1700000000-nothing", false); err != ErrNotFound {
		t.Errorf("a valid ID with no video: %v, want ErrNotFound", err)
	}
}

// fakeVideo writes the three files of a finished video that ended at end.
func fakeVideo(t *testing.T, r *Recorder, id string, end time.Time) {
	t.Helper()
	os.MkdirAll(r.dir, 0o755)
	meta, _ := json.Marshal(Meta{ID: id, PrinterName: "P", Start: end.Unix() - 600, End: end.Unix(), Result: "finished", Frames: 20, Format: "mp4", Poster: true})
	for ext, data := range map[string][]byte{".json": meta, ".mp4": []byte("video"), ".jpg": []byte("poster")} {
		if err := os.WriteFile(filepath.Join(r.dir, id+ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetention(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	day := 24 * time.Hour
	fakeVideo(t, r, "100-old", clock.Add(-40*day))
	fakeVideo(t, r, "200-edge", clock.Add(-29*day))
	fakeVideo(t, r, "300-new", clock.Add(-1*time.Hour))
	oldPart := filepath.Join(r.dir, "1-x.mp4.part")
	newPart := filepath.Join(r.dir, "2-y.mp4.part")
	os.WriteFile(oldPart, []byte("half"), 0o644)
	os.WriteFile(newPart, []byte("half"), 0o644)
	os.Chtimes(oldPart, clock.Add(-3*day), clock.Add(-3*day))

	if n := r.Cleanup(0); n != 0 || len(r.List()) != 3 {
		t.Fatalf("keep days 0 removed %d videos, left %d; want it to keep all 3", n, len(r.List()))
	}
	if _, err := os.Stat(oldPart); err == nil {
		t.Error("a day-old half-written file was kept")
	}
	if _, err := os.Stat(newPart); err != nil {
		t.Error("a fresh half-written file (a build in progress) was removed")
	}
	if n := r.Cleanup(30); n != 1 {
		t.Fatalf("removed %d, want 1 (only the 40-day-old one)", n)
	}
	var ids []string
	for _, m := range r.List() {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "300-new,200-edge" { // newest first
		t.Errorf("left %v", ids)
	}
	for _, ext := range []string{".json", ".mp4", ".jpg"} {
		if _, err := os.Stat(filepath.Join(r.dir, "100-old"+ext)); err == nil {
			t.Errorf("100-old%s was not deleted", ext)
		}
	}
	if err := r.Delete("200-edge"); err != nil || len(r.List()) != 1 {
		t.Errorf("Delete: %v, %d left", err, len(r.List()))
	}
	if err := r.Delete("200-edge"); err != ErrNotFound {
		t.Errorf("deleting twice: %v, want ErrNotFound", err)
	}
}

// A restart mid-print picks up the same folder if it is the same file.
func TestResumeAfterRestart(t *testing.T) {
	r, clock := newTestRecorder(t, nil)
	r.Start(testPrinter, "cube.gcode", false)
	capture(t, r, clock, 12, 30*time.Second, marker)
	first := r.sessions[testPrinter.ID].dir

	r2 := New(r.dir, nil) // the Bridge restarted
	r2.now = r.now
	*clock = clock.Add(time.Minute)
	r2.Start(testPrinter, "cube.gcode", true)
	if s := r2.sessions[testPrinter.ID]; s == nil || s.dir != first || s.count != 12 {
		t.Fatalf("resumed %+v, want the same folder with 12 pictures", s)
	}
	r2.Start(testPrinter, "ignored.gcode", true) // already recording: no change
	if r2.sessions[testPrinter.ID].dir != first {
		t.Error("resume replaced a recording in progress")
	}

	// A different file, or an empty name, starts afresh.
	for _, file := range []string{"other.gcode", ""} {
		r3 := New(r.dir, nil)
		*clock = clock.Add(time.Minute)
		r3.now = r.now
		r3.Start(testPrinter, file, true)
		if s := r3.sessions[testPrinter.ID]; s.dir == first || s.count != 0 {
			t.Errorf("file %q resumed the old folder", file)
		}
	}
	// Too long ago: the print is over.
	r4 := New(r.dir, nil)
	*clock = clock.Add(resumeWithin + time.Hour)
	r4.now = r.now
	r4.Start(testPrinter, "cube.gcode", true)
	if s := r4.sessions[testPrinter.ID]; s.dir == first {
		t.Error("resumed a print whose last picture is hours old")
	}
}

func TestStartWithoutResumeEndsThePreviousPrint(t *testing.T) {
	r, clock := newTestRecorder(t, func() string { return "" })
	r.Start(testPrinter, "a.gcode", false)
	capture(t, r, clock, 12, 5*time.Second, func(i int) []byte { return testJPEG(t, 64, 48, i) })
	*clock = clock.Add(time.Minute)
	r.Start(testPrinter, "b.gcode", false) // a.gcode's end event never came
	waitBuilt(t, r)
	if l := r.List(); len(l) != 1 || l[0].File != "a.gcode" || l[0].Result != "cancelled" {
		t.Errorf("list = %+v, want the first print as a cancelled video", l)
	}
}

func TestRecover(t *testing.T) {
	r, clock := newTestRecorder(t, func() string { return "" })
	mk := func(printer string, start time.Time, n int, st *state, age time.Duration) string {
		dir := filepath.Join(r.dir, printer, fmt.Sprint(start.Unix()))
		os.MkdirAll(dir, 0o755)
		for i := 1; i <= n; i++ {
			os.WriteFile(filepath.Join(dir, frameName(i)), testJPEG(t, 64, 48, i), 0o644)
			os.Chtimes(filepath.Join(dir, frameName(i)), clock.Add(-age), clock.Add(-age))
		}
		if st != nil {
			writeState(dir, *st)
		}
		return dir
	}
	ended := mk("aaa", clock.Add(-time.Hour), 12, &state{PrinterID: "aaa", PrinterName: "A", Start: clock.Add(-time.Hour).Unix(), Stride: 1, Ended: true, End: clock.Unix(), Result: "finished"}, time.Minute)
	stale := mk("bbb", clock.Add(-9*time.Hour), 15, &state{PrinterID: "bbb", PrinterName: "B", Start: clock.Add(-9 * time.Hour).Unix(), Stride: 1}, 8*time.Hour)
	recent := mk("ccc", clock.Add(-time.Hour), 15, &state{PrinterID: "ccc", PrinterName: "C", Start: clock.Add(-time.Hour).Unix(), Stride: 1}, time.Minute)
	tiny := mk("ddd", clock.Add(-9*time.Hour), 3, &state{PrinterID: "ddd", PrinterName: "D", Start: clock.Add(-9 * time.Hour).Unix(), Stride: 1}, 8*time.Hour)
	nostate := mk("eee", clock.Add(-9*time.Hour), 11, nil, 8*time.Hour)

	r.Recover()
	waitBuilt(t, r)

	byPrinter := map[string]Meta{}
	for _, m := range r.List() {
		byPrinter[m.PrinterID] = m
	}
	if len(byPrinter) != 3 || byPrinter["aaa"].Result != "finished" || byPrinter["bbb"].Result != "cancelled" || byPrinter["eee"].Frames != 11 {
		t.Errorf("videos = %+v, want aaa (finished), bbb (cancelled) and eee", byPrinter)
	}
	for name, dir := range map[string]string{"ended": ended, "stale": stale, "tiny": tiny, "no state": nostate} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("the %s folder was not cleaned up", name)
		}
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("a recent open print was closed; it may still be printing")
	}
}
