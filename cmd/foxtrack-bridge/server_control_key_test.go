package main

import (
	"testing"
	"time"
)

func TestControlKeys(t *testing.T) {
	now := time.Now()
	k := controlKey("Fox", "pause", "k-1")
	if controlKey("Fox", "pause", "") != "" || seenControlKey("", now) {
		t.Fatal("no key is never a repeat")
	}
	if seenControlKey(k, now) {
		t.Fatal("a key that never ran is not a repeat (a failed command may be retried)")
	}
	rememberControlKey(k, now)
	if !seenControlKey(k, now.Add(time.Second)) {
		t.Fatal("the same click again must be dropped")
	}
	if seenControlKey(controlKey("Fox", "stop", "k-1"), now) || seenControlKey(controlKey("Owl", "pause", "k-1"), now) {
		t.Fatal("the key is per printer and command")
	}
	if seenControlKey(k, now.Add(11*time.Minute)) {
		t.Fatal("a key is forgotten after 10 minutes")
	}
}
