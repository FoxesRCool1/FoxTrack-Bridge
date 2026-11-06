package backup

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func listCopies(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > len(prefix) && e.Name()[:len(prefix)] == prefix {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestFirstRunCreatesBothCopies(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.json"), `{"a":1}`)
	write(t, filepath.Join(dir, "history.json"), `[]`)

	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	if err := Run(dir, now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	backups := filepath.Join(dir, "backups")
	date := now.Format("2006-01-02")
	for _, name := range []string{"config-" + date + ".json", "history-" + date + ".json"} {
		p := filepath.Join(backups, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("missing copy %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}
}

func TestSecondRunSameDayChangesNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.json"), `{"a":1}`)
	write(t, filepath.Join(dir, "history.json"), `[]`)

	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	if err := Run(dir, now); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	date := now.Format("2006-01-02")
	cfgCopy := filepath.Join(dir, "backups", "config-"+date+".json")
	info, err := os.Stat(cfgCopy)
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	firstMtime := info.ModTime()

	// Change the source after the first run: the skip must keep the old copy.
	write(t, filepath.Join(dir, "config.json"), `{"a":2}`)

	again := now.Add(2 * time.Hour)
	if err := Run(dir, again); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	info, err = os.Stat(cfgCopy)
	if err != nil {
		t.Fatalf("stat copy after second run: %v", err)
	}
	if !info.ModTime().Equal(firstMtime) {
		t.Errorf("copy was rewritten on the second run")
	}
	data, err := os.ReadFile(cfgCopy)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("copy content = %q, want the original %q", data, `{"a":1}`)
	}
}

func TestPruneKeepsNewestSeven(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0700); err != nil {
		t.Fatal(err)
	}
	// 9 older dated copies of each kind, plus a current source file.
	for i := 0; i < 9; i++ {
		d := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.Local).Format("2006-01-02")
		write(t, filepath.Join(backups, "config-"+d+".json"), "c")
		write(t, filepath.Join(backups, "history-"+d+".json"), "h")
	}
	write(t, filepath.Join(dir, "config.json"), `{"a":1}`)
	write(t, filepath.Join(dir, "history.json"), `[]`)

	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	if err := Run(dir, now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	date := now.Format("2006-01-02")
	for _, prefix := range []string{"config-", "history-"} {
		names := listCopies(t, backups, prefix)
		if len(names) != 7 {
			t.Errorf("%s copies = %d (%v), want 7", prefix, len(names), names)
		}
		if names[len(names)-1] != prefix+date+".json" {
			t.Errorf("newest %s copy missing: %v", prefix, names)
		}
		// The three oldest dates must be gone.
		for _, gone := range []string{"2026-10-01", "2026-10-02", "2026-10-03"} {
			for _, n := range names {
				if n == prefix+gone+".json" {
					t.Errorf("oldest copy %s still present", n)
				}
			}
		}
	}
}

func TestMissingHistoryIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.json"), `{"a":1}`)

	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	if err := Run(dir, now); err != nil {
		t.Fatalf("Run with missing history.json: %v", err)
	}
	date := now.Format("2006-01-02")
	if _, err := os.Stat(filepath.Join(dir, "backups", "config-"+date+".json")); err != nil {
		t.Errorf("config copy not made: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", "history-"+date+".json")); !os.IsNotExist(err) {
		t.Errorf("history copy should not exist, stat err = %v", err)
	}
}

func TestUnrelatedFilesAreNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(backups, "notes.txt"), "keep me")
	// 9 old config copies to trigger pruning.
	for i := 0; i < 9; i++ {
		d := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.Local).Format("2006-01-02")
		write(t, filepath.Join(backups, "config-"+d+".json"), "c")
	}
	write(t, filepath.Join(dir, "config.json"), `{"a":1}`)

	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	if err := Run(dir, now); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backups, "notes.txt")); err != nil {
		t.Errorf("notes.txt was deleted: %v", err)
	}
}
