package timelapse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	videoFPS     = 30
	gifMaxWidth  = 640
	gifMaxFrames = 300
	gifDelay     = 4 // hundredths of a second; browsers slow anything under 2
	buildTimeout = 30 * time.Minute
)

// enqueue schedules a finished print's folder for a build. Builds run one at
// a time, in the background.
func (r *Recorder) enqueue(dir string) {
	r.qmu.Lock()
	if r.queued[dir] {
		r.qmu.Unlock()
		return
	}
	r.queued[dir] = true
	r.qmu.Unlock()
	r.workerOnce.Do(func() { go r.worker() })
	select {
	case r.queue <- dir:
	default:
		// Full: the folder stays and the next Recover queues it again.
		r.qmu.Lock()
		delete(r.queued, dir)
		r.qmu.Unlock()
	}
}

func (r *Recorder) worker() {
	for dir := range r.queue {
		if err := r.Build(dir); err != nil {
			log.Printf("[timelapse] building a video from %s failed: %v", dir, err)
		}
		r.qmu.Lock()
		delete(r.queued, dir)
		r.qmu.Unlock()
	}
}

// Build turns an ended print's pictures into a video, writes its details and
// deletes the pictures. With ffmpeg it makes an H.264 MP4 (MPEG-4 part 2 if
// there is no libx264); if that fails, or there is no ffmpeg, an animated GIF.
// A folder whose pictures cannot be read at all is deleted; any other failure
// keeps the pictures for a later try.
func (r *Recorder) Build(dir string) error {
	st, ok := readState(dir)
	if !ok || !st.Ended {
		return errors.New("the print is not marked as ended")
	}
	n, err := compact(dir)
	if err != nil {
		return err
	}
	if n < MinFrames {
		removeSession(dir)
		return nil
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	id := videoID(st)
	base := filepath.Join(r.dir, id)
	m := Meta{
		ID: id, PrinterID: st.PrinterID, PrinterName: st.PrinterName, File: st.File,
		Start: st.Start, End: st.End, Result: st.Result, Frames: n,
	}

	if bin := r.ffmpegPath(); bin != "" {
		if err := buildMP4(bin, dir, base+".mp4.part"); err == nil {
			m.Format, m.Duration = "mp4", float64(n)/videoFPS
		} else {
			log.Printf("[timelapse] ffmpeg could not build %s (%v); making a GIF instead", id, err)
		}
	}
	if m.Format == "" {
		used, err := buildGIF(dir, n, base+".gif.part")
		if errors.Is(err, errNoFrames) {
			removeSession(dir)
			return err
		}
		if err != nil {
			return err
		}
		m.Format, m.Duration = "gif", float64(used*gifDelay)/100
	}
	video := base + "." + m.Format
	if err := os.Rename(video+".part", video); err != nil {
		return err
	}
	if fi, err := os.Stat(video); err == nil {
		m.Size = fi.Size()
	}
	if m.Format == "mp4" { // the last picture, the finished print, is the poster
		if data, err := os.ReadFile(filepath.Join(dir, frameName(n))); err == nil && writeFileAtomic(base+".jpg", data) == nil {
			m.Poster = true
		}
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(base+".json", data); err != nil {
		return err
	}
	removeSession(dir)
	return nil
}

func (r *Recorder) ffmpegPath() string {
	if r.ffmpeg == nil {
		return ""
	}
	return r.ffmpeg()
}

// buildMP4 encodes the numbered pictures in dir to an MP4 at out. It tries
// libx264 and then MPEG-4 part 2, for ffmpeg builds without libx264.
func buildMP4(bin, dir, out string) error {
	codecs := [][]string{
		{"-c:v", "libx264", "-preset", "veryfast", "-crf", "23"},
		{"-c:v", "mpeg4", "-q:v", "4"},
	}
	var last error
	for _, codec := range codecs {
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-framerate", fmt.Sprint(videoFPS), "-start_number", "1", "-i", filepath.Join(dir, "%05d.jpg"),
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2:out_range=tv"}
		args = append(args, codec...)
		args = append(args, "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-f", "mp4", out)

		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		cmd := exec.CommandContext(ctx, bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.WaitDelay = 2 * time.Second
		err := cmd.Run()
		cancel()
		if err == nil {
			return nil
		}
		os.Remove(out)
		last = fmt.Errorf("%s: %v: %s", codec[1], err, lastLine(stderr.String()))
	}
	return last
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

var errNoFrames = errors.New("none of the pictures could be read")

// sample picks at most max of the numbers 1..n, spread evenly and including
// the first and the last.
func sample(n, max int) []int {
	if n <= max {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i + 1
		}
		return idx
	}
	idx := make([]int, max)
	for k := range idx {
		idx[k] = 1 + k*(n-1)/(max-1)
	}
	return idx
}

// buildGIF writes an animated GIF of up to gifMaxFrames of the n pictures in
// dir, each at most gifMaxWidth wide, and returns how many it used. The
// standard library only, so it works without ffmpeg.
func buildGIF(dir string, n int, out string) (int, error) {
	g := &gif.GIF{}
	for _, i := range sample(n, gifMaxFrames) {
		f, err := os.Open(filepath.Join(dir, frameName(i)))
		if err != nil {
			continue
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue // a picture cut short by a crash
		}
		small := scaleDown(img, gifMaxWidth)
		p := image.NewPaletted(small.Bounds(), palette.Plan9)
		draw.FloydSteinberg.Draw(p, p.Bounds(), small, small.Bounds().Min)
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, gifDelay)
	}
	if len(g.Image) == 0 {
		return 0, errNoFrames
	}
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	err = gif.EncodeAll(f, g)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
		return 0, err
	}
	return len(g.Image), nil
}

// scaleDown shrinks src to at most maxW wide, averaging 2 x 2 samples per
// new pixel. A smaller picture is returned as it is.
func scaleDown(src image.Image, maxW int) image.Image {
	sb := src.Bounds()
	w, h := sb.Dx(), sb.Dy()
	if w <= maxW {
		return src
	}
	dw, dh := maxW, max(1, h*maxW/w)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var r, g, b uint32
			for _, oy := range [2]int{1, 3} {
				for _, ox := range [2]int{1, 3} {
					sx := sb.Min.X + (x*4+ox)*w/(dw*4)
					sy := sb.Min.Y + (y*4+oy)*h/(dh*4)
					pr, pg, pb, _ := src.At(sx, sy).RGBA()
					r, g, b = r+pr, g+pg, b+pb
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r>>10), uint8(g>>10), uint8(b>>10), 0xFF
		}
	}
	return dst
}
