//go:build headless

package main

import (
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func TestParseFailWatchReply(t *testing.T) {
	cases := []struct {
		in     string
		failed bool
		reason string
	}{
		{`{"failed": true, "reason": "spaghetti"}`, true, "spaghetti"},
		{"```json\n{\"failed\": false, \"reason\": \"looks fine\"}\n```", false, "looks fine"},
		{`Sure! {"failed":true,"reason":"part moved"} Hope that helps.`, true, "part moved"},
		{`I think it failed`, false, ""},
		{`{"failed": "yes"}`, false, ""},
		{``, false, ""},
	}
	for _, c := range cases {
		f, r := parseFailWatchReply(c.in)
		if f != c.failed || r != c.reason {
			t.Errorf("%q: got (%v, %q), want (%v, %q)", c.in, f, r, c.failed, c.reason)
		}
	}
}

func TestFailWatchRecord_TwoStrikesOneAlert(t *testing.T) {
	st := &failWatchState{}
	steps := []struct {
		failed, alert bool
	}{
		{true, false}, {false, false}, // a single bad picture is forgiven
		{true, false}, {true, true}, // two in a row: alert
		{true, false}, {true, false}, // once per print
	}
	for i, s := range steps {
		if got := failWatchRecord(st, s.failed); got != s.alert {
			t.Fatalf("step %d: alert = %v, want %v", i, got, s.alert)
		}
	}
}

func TestFailWatchEnabledAndInterval(t *testing.T) {
	a := config.AI{Enabled: true, Preset: "openai", Model: "m", APIKey: "k", WatchFailures: true}
	if failWatchEnabled(a) {
		t.Fatal("needs the camera permission too")
	}
	a.AllowCamera = true
	if !failWatchEnabled(a) {
		t.Fatal("should be on")
	}
	a.Enabled = false
	if failWatchEnabled(a) {
		t.Fatal("assistant off means watch off")
	}
	for in, want := range map[int]time.Duration{0: 5 * time.Minute, 1: 2 * time.Minute, 10: 10 * time.Minute, 500: time.Hour} {
		if got := failWatchInterval(config.AI{WatchEveryMin: in}); got != want {
			t.Errorf("interval(%d) = %v, want %v", in, got, want)
		}
	}
}
