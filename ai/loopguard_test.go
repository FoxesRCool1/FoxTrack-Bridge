package ai

import (
	"strings"
	"testing"
)

// A good answer is left completely alone. This is the case that matters most:
// a guard that eats real content is worse than no guard.
func TestTrimRunaway_LeavesAGoodAnswerAlone(t *testing.T) {
	answer := strings.Join([]string{
		"Here are the steps to add a Bambu Lab printer in LAN mode:",
		"",
		"1. Turn on LAN Only Mode under Network settings on the printer's touchscreen.",
		"2. Turn on Developer Mode under About on the same screen.",
		"3. Write down the IP address, the serial number and the LAN access code.",
		"4. In the dashboard, go to Printers and click Add Printer.",
		"5. Choose the type Bambu Lab, enter those three values, and click Connect.",
		"",
		"The serial number and the access code are case sensitive and must be typed exactly.",
		"The Bridge reaches the printer over MQTT on TCP port 8883, so nothing may block that port.",
	}, "\n")

	if got := TrimRunaway(answer); got != answer {
		t.Errorf("a clean answer was modified:\n%s", got)
	}
}

// The observed failure: a correct answer followed by sign-offs that never stop.
func TestTrimRunaway_CutsTheSignOffLoop(t *testing.T) {
	good := strings.Join([]string{
		"Your printer connects over MQTT on port 8883 once LAN Only Mode is on.",
		"Check the serial number and the LAN access code are typed exactly as shown.",
	}, "\n")
	loop := strings.Join([]string{
		"If you run into trouble, I am here to help you with it. Just ask me.",
		"If you run into any trouble at all, I am here to help. Just ask me.",
		"If you run into trouble later, I am still here to help you. Just ask.",
		"If you run into any trouble, I am here to help you out. Just ask me.",
		"If you run into trouble again, I am here to help you with it. Just ask.",
	}, "\n")

	got := TrimRunaway(good + "\n" + loop)
	if !strings.Contains(got, "port 8883") {
		t.Fatal("the real answer was thrown away")
	}
	if strings.Count(got, "Just ask") > 2 {
		t.Errorf("the loop survived:\n%s", got)
	}
	if !strings.Contains(got, "repeating itself") {
		t.Error("the user was not told the reply was cut")
	}
}

// Short lines cannot be used to smuggle a loop past the word threshold.
func TestTrimRunaway_ShortLinesStillFeedTheSeenSet(t *testing.T) {
	text := strings.Join([]string{
		"The nozzle is at 210 degrees and the bed is at 60 degrees right now.",
		"Check it.",
		"Check it.",
		"The nozzle is at 210 degrees and the bed is at 60 degrees right now again.",
		"The nozzle is at 210 degrees and the bed is at 60 degrees right now once more.",
		"The nozzle is at 210 degrees and the bed is at 60 degrees right now, again.",
	}, "\n")
	got := TrimRunaway(text)
	if strings.Count(got, "210 degrees") > 2 {
		t.Errorf("repeated substantial lines survived:\n%s", got)
	}
}

// A line that brings new content clears the counter, so two restatements
// scattered through a long answer never trigger a cut.
func TestTrimRunaway_NewContentResetsTheCount(t *testing.T) {
	text := strings.Join([]string{
		"The printer is offline and the Bridge has no telemetry for it at all.",
		"The printer is offline and the Bridge has no telemetry for it right now.",
		"Its IP address has probably changed since you last wrote it down somewhere.",
		"Check the address on the printer screen and correct it in the dashboard now.",
		"A DHCP reservation on your router stops the address changing again later on.",
	}, "\n")
	if got := TrimRunaway(text); got != text {
		t.Errorf("a normal answer with one restatement was cut:\n%s", got)
	}
}

func TestTrimRunaway_EmptyAndShortInputs(t *testing.T) {
	for _, in := range []string{"", "Yes.", "The printer is printing."} {
		if got := TrimRunaway(in); got != in {
			t.Errorf("TrimRunaway(%q) = %q, want it unchanged", in, got)
		}
	}
}
