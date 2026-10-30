package ftps

import (
	"context"
	"testing"
	"time"
)

func TestParseListLine(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e, ok := ParseListLine("-rw-rw-rw-    1 0        0        12345678 Oct 29 10:21 my part.gcode.3mf", now)
	if !ok || e.Name != "my part.gcode.3mf" || e.Size != 12345678 ||
		!e.Modified.Equal(time.Date(2026, 10, 29, 10, 21, 0, 0, time.UTC)) {
		t.Fatalf("got %+v %v", e, ok)
	}
	// A month still ahead of now without a year is last year.
	e, ok = ParseListLine("-rw-r--r-- 1 root root 42 Dec 24 08:00 x.3mf", now)
	if !ok || e.Modified.Year() != 2025 {
		t.Fatalf("got %+v %v", e, ok)
	}
	e, ok = ParseListLine("-rw-rw-rw-    1 0        0           99 Mar  3  2024 old.3mf", now)
	if !ok || e.Size != 99 || !e.Modified.Equal(time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v %v", e, ok)
	}
	for _, bad := range []string{
		"drwxrwxrwx    2 0        0            4096 Oct 29 10:21 cache",
		"lrwxrwxrwx    1 0        0               7 Oct 29 10:21 link -> target",
		"total 12",
		"",
		"-rw-rw-rw-    1 0        0        notanumber Oct 29 10:21 a.3mf",
	} {
		if _, ok := ParseListLine(bad, now); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestList_ReadsDirectoryOverResumedDataChannel(t *testing.T) {
	s := newFakeServer(t)
	s.listing = map[string][]string{
		"/": {
			"drwxrwxrwx    2 0        0            4096 Oct 29 10:21 cache",
			"-rw-rw-rw-    1 0        0        12345678 Oct 29 10:21 my part.gcode.3mf",
			"-rw-rw-rw-    1 0        0             512 Sep 01  2025 notes.txt",
		},
	}
	got, err := List(context.Background(), s.config(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Name != "my part.gcode.3mf" || got[1].Name != "notes.txt" {
		t.Fatalf("entries = %+v", got)
	}
	if _, err := List(context.Background(), s.config(), "/nowhere"); err == nil {
		t.Fatal("a missing directory must be an error")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.verbs) != 2 || s.verbs[0] != "LIST /" || s.verbs[1] != "LIST /nowhere" {
		t.Fatalf("verbs = %v", s.verbs)
	}
}
