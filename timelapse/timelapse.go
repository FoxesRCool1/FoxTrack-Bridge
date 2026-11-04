// Package timelapse records a picture every few seconds while a printer
// prints, and turns the pictures into a video at the end (an H.264 MP4 with
// ffmpeg, an animated GIF without). It knows nothing about printers or
// cameras: the caller says when a print starts and ends and hands in each
// JPEG.
//
// On disk, under the directory given to New:
//
//	<printer id>/<start unix>/00001.jpg ...   pictures of a print in progress
//	<printer id>/<start unix>/session.json    where that print stands
//	<start unix>-<printer id>.mp4|gif         a finished video
//	<start unix>-<printer id>.json            its details
//	<start unix>-<printer id>.jpg             its poster picture (mp4 only)
package timelapse

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxFrames       = 3000 // per print; at the cap every other picture is dropped
	MinFrames       = 10   // fewer than this is not worth a video
	DefaultInterval = 30   // seconds between pictures
	MinInterval     = 5
	MaxInterval     = 3600
	DefaultKeepDays = 30
	MaxKeepDays     = 3650

	resumeWithin = 3 * time.Hour  // a restart resumes a print whose last picture is newer than this
	staleAfter   = 6 * time.Hour  // an open print with no picture for this long is over
	partTTL      = 24 * time.Hour // leftover half-written videos are removed after this
)

// Interval is the picture spacing in seconds for a stored setting.
func Interval(stored int) int {
	switch {
	case stored <= 0:
		return DefaultInterval
	case stored < MinInterval:
		return MinInterval
	case stored > MaxInterval:
		return MaxInterval
	}
	return stored
}

// KeepDays is how many days videos are kept for a stored setting, 0 meaning
// forever. The stored value is -1 for forever because config.json leaves out
// a zero, and an unset setting has to mean the default of 30.
func KeepDays(stored int) int {
	switch {
	case stored == 0:
		return DefaultKeepDays
	case stored < 0:
		return 0
	}
	return min(stored, MaxKeepDays)
}

// StoredKeepDays is KeepDays turned around: what to store for "days, 0 =
// forever".
func StoredKeepDays(days int) int {
	if days <= 0 {
		return -1
	}
	return days
}

// Printer names one printer for the recorder.
type Printer struct{ ID, Name string }

// Meta is the .json next to a finished video.
type Meta struct {
	ID          string  `json:"id"`
	PrinterID   string  `json:"printer_id"`
	PrinterName string  `json:"printer_name"`
	File        string  `json:"file,omitempty"`
	Start       int64   `json:"start"` // Unix seconds
	End         int64   `json:"end"`
	Result      string  `json:"result"` // finished, failed or cancelled
	Frames      int     `json:"frames"`
	Duration    float64 `json:"duration_sec"` // length of the video
	Size        int64   `json:"size"`         // bytes
	Format      string  `json:"format"`       // mp4 or gif
	Poster      bool    `json:"poster,omitempty"`
}

// state is session.json: the part of a print in progress that must survive a
// restart.
type state struct {
	PrinterID   string `json:"printer_id"`
	PrinterName string `json:"printer_name"`
	File        string `json:"file"`
	Start       int64  `json:"start"`
	Stride      int    `json:"stride"` // pictures are every Stride x interval; doubles when pictures are thinned
	Ended       bool   `json:"ended,omitempty"`
	End         int64  `json:"end,omitempty"`
	Result      string `json:"result,omitempty"`
}

type session struct {
	st    state
	dir   string
	made  bool // the folder exists; it is made with the first picture
	count int
	next  time.Time
	busy  bool
}

// Recorder keeps the pictures of the prints in progress and builds videos.
type Recorder struct {
	dir       string
	ffmpeg    func() string
	now       func() time.Time
	maxFrames int

	mu       sync.Mutex
	sessions map[string]*session // by printer ID

	qmu        sync.Mutex
	queued     map[string]bool // folders waiting for or in a build
	queue      chan string
	workerOnce sync.Once
}

// New returns a recorder storing under dir. ffmpeg returns the path of
// ffmpeg, or "" for none, and is asked at each build.
func New(dir string, ffmpeg func() string) *Recorder {
	return &Recorder{
		dir: dir, ffmpeg: ffmpeg, now: time.Now, maxFrames: MaxFrames,
		sessions: map[string]*session{}, queued: map[string]bool{}, queue: make(chan string, 1024),
	}
}

// Dir is where the recorder stores.
func (r *Recorder) Dir() string { return r.dir }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// safeName makes a printer ID fit for a file name.
func safeName(id string) string {
	s := unsafeName.ReplaceAllString(id, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = "printer"
	}
	return s
}

var idPattern = regexp.MustCompile(`^[0-9]{1,12}-[A-Za-z0-9_-]{1,64}$`)

// ValidID reports whether id has the shape of a video ID. Anything else,
// notably a path, is refused before it reaches the file system.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func videoID(st state) string { return fmt.Sprintf("%d-%s", st.Start, safeName(st.PrinterID)) }

func frameName(i int) string { return fmt.Sprintf("%05d.jpg", i) }

// Start begins recording a print of file on p. A print already being recorded
// on that printer is ended first, unless resume is set: then the call does
// nothing if one is, and otherwise picks up the printer's open folder when it
// is the same file with recent pictures (the Bridge restarted mid-print).
func (r *Recorder) Start(p Printer, file string, resume bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.sessions[p.ID]; old != nil {
		if resume {
			return
		}
		delete(r.sessions, p.ID)
		r.finish(old, "cancelled")
	}
	if resume {
		if s := r.resumable(p, file); s != nil {
			r.sessions[p.ID] = s
			return
		}
	}
	start := r.now().Unix()
	r.sessions[p.ID] = &session{
		st:  state{PrinterID: p.ID, PrinterName: p.Name, File: file, Start: start, Stride: 1},
		dir: filepath.Join(r.dir, safeName(p.ID), strconv.FormatInt(start, 10)),
	}
}

// resumable returns the printer's newest folder when it is an open print of
// the same file with recent pictures.
func (r *Recorder) resumable(p Printer, file string) *session {
	pdir := filepath.Join(r.dir, safeName(p.ID))
	entries, err := os.ReadDir(pdir)
	if err != nil || file == "" {
		return nil
	}
	newest := "" // folder names are Unix times of equal length, so they sort by age
	for _, e := range entries {
		if e.IsDir() && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		return nil
	}
	dir := filepath.Join(pdir, newest)
	st, ok := readState(dir)
	if !ok || st.Ended || st.File != file {
		return nil
	}
	count, mtime := frameStats(dir)
	if count == 0 || r.now().Sub(mtime) > resumeWithin {
		return nil
	}
	if st.Stride < 1 {
		st.Stride = 1
	}
	return &session{st: st, dir: dir, made: true, count: count}
}

// Active reports whether a print is being recorded on the printer.
func (r *Recorder) Active(printerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[printerID] != nil
}

// ActiveIDs lists the printers being recorded.
func (r *Recorder) ActiveIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	return ids
}

// Recording is a print being recorded now.
type Recording struct {
	PrinterID   string `json:"printer_id"`
	PrinterName string `json:"printer_name"`
	File        string `json:"file,omitempty"`
	Frames      int    `json:"frames"`
}

// Recording lists the prints being recorded, with the pictures so far.
func (r *Recorder) Recording() []Recording {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Recording, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, Recording{PrinterID: s.st.PrinterID, PrinterName: s.st.PrinterName, File: s.st.File, Frames: s.count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PrinterName < out[j].PrinterName })
	return out
}

// Building is how many videos are queued or being built.
func (r *Recorder) Building() int {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	return len(r.queued)
}

// Capture takes a picture of the printer's print if one is due: snap is
// called (without any lock held) and its JPEG stored. The spacing is the
// interval times the session's stride, kept to a schedule so a slow camera
// does not stretch it. A session with a picture already being taken is
// skipped.
func (r *Recorder) Capture(printerID string, interval time.Duration, snap func() ([]byte, error)) error {
	r.mu.Lock()
	s := r.sessions[printerID]
	now := r.now()
	if s == nil || s.busy || now.Before(s.next) {
		r.mu.Unlock()
		return nil
	}
	s.busy = true
	step := interval * time.Duration(s.st.Stride)
	if s.next.IsZero() || s.next.Add(step).Before(now) {
		s.next = now.Add(step)
	} else {
		s.next = s.next.Add(step)
	}
	r.mu.Unlock()

	jpg, err := snap()

	r.mu.Lock()
	defer r.mu.Unlock()
	s.busy = false
	if err != nil {
		return err
	}
	if r.sessions[printerID] != s {
		return nil // the print ended while the camera answered
	}
	return r.add(s, jpg)
}

// add stores one picture. The caller holds r.mu.
func (r *Recorder) add(s *session, jpg []byte) error {
	if len(jpg) < 4 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		return fmt.Errorf("the camera did not return a JPEG picture")
	}
	if !s.made {
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return err
		}
		if err := writeState(s.dir, s.st); err != nil {
			return err
		}
		s.made = true
	}
	if err := writeFileAtomic(filepath.Join(s.dir, frameName(s.count+1)), jpg); err != nil {
		return err
	}
	s.count++
	if s.count >= r.maxFrames {
		// Drop every other picture and take them half as often from now on,
		// so the video stays evenly paced and the print fits the cap.
		n, err := thin(s.dir, s.count)
		if err != nil {
			return err
		}
		s.count = n
		s.st.Stride *= 2
		return writeState(s.dir, s.st)
	}
	return nil
}

// thin deletes the even-numbered pictures and renumbers the rest.
func thin(dir string, count int) (int, error) {
	for i := 2; i <= count; i += 2 {
		os.Remove(filepath.Join(dir, frameName(i)))
	}
	return compact(dir)
}

// compact renumbers the pictures to 00001.jpg, 00002.jpg ... without gaps.
func compact(dir string) (int, error) {
	names := frameFiles(dir)
	for i, n := range names {
		if want := frameName(i + 1); n != want {
			if err := os.Rename(filepath.Join(dir, n), filepath.Join(dir, want)); err != nil {
				return 0, err
			}
		}
	}
	return len(names), nil
}

func frameFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// frameStats counts the pictures in dir and finds the newest one's time.
func frameStats(dir string) (int, time.Time) {
	var newest time.Time
	names := frameFiles(dir)
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return len(names), newest
}

// End finishes the printer's print. With enough pictures a video is queued;
// with too few the pictures are deleted. result is "finished", "failed" or
// "cancelled".
func (r *Recorder) End(printerID, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[printerID]; s != nil {
		delete(r.sessions, printerID)
		r.finish(s, result)
	}
}

// finish closes a session that is no longer in r.sessions.
func (r *Recorder) finish(s *session, result string) {
	if s.count < MinFrames {
		removeSession(s.dir)
		return
	}
	s.st.Ended, s.st.End, s.st.Result = true, r.now().Unix(), result
	if err := writeState(s.dir, s.st); err != nil {
		log.Printf("[timelapse] %s: %v", s.st.PrinterName, err)
		return
	}
	r.enqueue(s.dir)
}

func removeSession(dir string) {
	os.RemoveAll(dir)
	os.Remove(filepath.Dir(dir)) // the printer's folder, if now empty
}

// Recover queues the videos of prints that ended before a restart, and ends
// open prints that have had no picture for hours (the print finished while
// the Bridge was off). Prints that are open and recent are left for Start to
// resume. Call it at startup and now and then.
func (r *Recorder) Recover() {
	r.mu.Lock()
	active := map[string]bool{}
	for _, s := range r.sessions {
		active[s.dir] = true
	}
	r.mu.Unlock()

	printers, _ := os.ReadDir(r.dir)
	for _, pe := range printers {
		if !pe.IsDir() {
			continue
		}
		pdir := filepath.Join(r.dir, pe.Name())
		sessions, _ := os.ReadDir(pdir)
		for _, se := range sessions {
			dir := filepath.Join(pdir, se.Name())
			if !se.IsDir() || active[dir] {
				continue
			}
			st, ok := readState(dir)
			if !ok {
				start, _ := strconv.ParseInt(se.Name(), 10, 64)
				st = state{PrinterID: pe.Name(), PrinterName: pe.Name(), Start: start, Stride: 1}
			}
			count, mtime := frameStats(dir)
			if !st.Ended {
				if count > 0 && r.now().Sub(mtime) < staleAfter {
					continue // may still be printing
				}
				st.Ended, st.Result = true, "cancelled"
				if st.End = mtime.Unix(); count == 0 {
					st.End = r.now().Unix()
				}
				if count >= MinFrames && writeState(dir, st) != nil {
					continue
				}
			}
			if count < MinFrames {
				removeSession(dir)
				continue
			}
			r.enqueue(dir)
		}
		os.Remove(pdir) // gone if empty
	}
}

func readState(dir string) (state, bool) {
	var st state
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || json.Unmarshal(data, &st) != nil {
		return state{}, false
	}
	return st, true
}

func writeState(dir string, st state) error {
	data, _ := json.Marshal(st)
	return writeFileAtomic(filepath.Join(dir, "session.json"), data)
}

// writeFileAtomic writes beside the target and renames, so a crash leaves
// either the old file or the new one, never half of one.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
