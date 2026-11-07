package events

import "testing"

func TestFoxTrackName(t *testing.T) {
	pairs := map[Kind]string{
		PrintFinished:  "print_finished",
		PrintFailed:    "print_error",
		PrintCancelled: "print_cancelled",
		PrintPaused:    "print_paused",
		PrinterError:   "printer_error",
		PrinterOffline: "printer_silent",
	}
	for k, want := range pairs {
		got, ok := FoxTrackName(k)
		if !ok {
			t.Errorf("FoxTrackName(%s) = ok=false, want a name", k)
			continue
		}
		if got != want {
			t.Errorf("FoxTrackName(%s) = %q, want %q", k, got, want)
		}
	}

	unmapped := []Kind{PrintStarted, PrintResumed, PrinterOnline, MaintenanceDue, PrintAlmostDone, PrintFailureSuspected, Test}
	for _, k := range unmapped {
		if _, ok := FoxTrackName(k); ok {
			t.Errorf("FoxTrackName(%s) = ok=true, want false", k)
		}
	}
}

func TestFromFoxTrackName(t *testing.T) {
	pairs := map[string]Kind{
		"print_finished":  PrintFinished,
		"print_error":     PrintFailed,
		"print_cancelled": PrintCancelled,
		"print_paused":    PrintPaused,
		"printer_error":   PrinterError,
		"printer_silent":  PrinterOffline,
	}
	for name, want := range pairs {
		got, ok := FromFoxTrackName(name)
		if !ok {
			t.Errorf("FromFoxTrackName(%q) = ok=false, want a kind", name)
			continue
		}
		if got != want {
			t.Errorf("FromFoxTrackName(%q) = %s, want %s", name, got, want)
		}
	}

	if _, ok := FromFoxTrackName("print_started"); ok {
		t.Errorf("FromFoxTrackName(%q) = ok=true, want false", "print_started")
	}
	if _, ok := FromFoxTrackName("no_such_event"); ok {
		t.Errorf("FromFoxTrackName(%q) = ok=true, want false", "no_such_event")
	}
}
