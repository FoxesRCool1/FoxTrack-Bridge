package timelapse

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Errors the file endpoints turn into HTTP statuses.
var (
	ErrBadID    = errors.New("that is not a timelapse ID")
	ErrNotFound = errors.New("that timelapse does not exist")
)

// List returns the finished videos, newest first.
func (r *Recorder) List() []Meta {
	entries, _ := os.ReadDir(r.dir)
	out := []Meta{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidID(id) {
			continue
		}
		if m, err := r.meta(id); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].End != out[j].End {
			return out[i].End > out[j].End
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// meta reads a video's details and checks that its video file is there.
func (r *Recorder) meta(id string) (Meta, error) {
	if !ValidID(id) {
		return Meta{}, ErrBadID
	}
	data, err := os.ReadFile(filepath.Join(r.dir, id+".json"))
	if err != nil {
		return Meta{}, ErrNotFound
	}
	var m Meta
	if json.Unmarshal(data, &m) != nil || (m.Format != "mp4" && m.Format != "gif") {
		return Meta{}, ErrNotFound
	}
	m.ID = id
	if _, err := os.Stat(filepath.Join(r.dir, id+"."+m.Format)); err != nil {
		return Meta{}, ErrNotFound
	}
	if m.Poster {
		_, err := os.Stat(filepath.Join(r.dir, id+".jpg"))
		m.Poster = err == nil
	}
	return m, nil
}

// Open returns the path of a video's file, or of its poster picture, and its
// details. id is checked here, so a caller may pass it straight from a URL.
func (r *Recorder) Open(id string, poster bool) (string, Meta, error) {
	m, err := r.meta(id)
	if err != nil {
		return "", Meta{}, err
	}
	if poster {
		if !m.Poster {
			return "", Meta{}, ErrNotFound
		}
		return filepath.Join(r.dir, id+".jpg"), m, nil
	}
	return filepath.Join(r.dir, id+"."+m.Format), m, nil
}

// Delete removes a video with its details and poster.
func (r *Recorder) Delete(id string) error {
	m, err := r.meta(id)
	if err != nil {
		return err
	}
	base := filepath.Join(r.dir, id)
	os.Remove(base + "." + m.Format)
	os.Remove(base + ".jpg")
	return os.Remove(base + ".json")
}

// Cleanup deletes the videos that ended more than keepDays ago (0 keeps them
// all) and half-written files left by a crash. It returns how many videos it
// deleted.
func (r *Recorder) Cleanup(keepDays int) int {
	now := r.now()
	entries, _ := os.ReadDir(r.dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".part") {
			if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > partTTL {
				os.Remove(filepath.Join(r.dir, e.Name()))
			}
		}
	}
	if keepDays <= 0 {
		return 0
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
	removed := 0
	for _, m := range r.List() {
		if m.End < cutoff && r.Delete(m.ID) == nil {
			removed++
		}
	}
	return removed
}
