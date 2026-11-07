// Package notify sends print events to chat and push services. It knows the
// services' wire formats and the Bridge's wording, and nothing about where
// events come from: the main package decides who gets what.
package notify

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/costs"
	"foxtrack-bridge/events"
)

// Message is one event in words. Every service gets the same text; only the
// markup around it differs.
type Message struct {
	Event    events.Event
	Title    string
	Lines    []string // one to three short lines of plain text
	Link     string   // optional, for a Bambu alarm code
	Snapshot []byte   // JPEG, set by the caller; Send drops it unless the channel wants one
}

var hmsCode = regexp.MustCompile(`^[0-9A-Fa-f]{4}(_[0-9A-Fa-f]{4}){3}$`)

// Labels are the dashboard's names for the event kinds.
var Labels = map[events.Kind]string{
	events.PrintStarted:          "Print started",
	events.PrintPaused:           "Print paused",
	events.PrintResumed:          "Print resumed",
	events.PrintFinished:         "Print finished",
	events.PrintFailed:           "Print failed",
	events.PrintCancelled:        "Print cancelled",
	events.PrinterError:          "Printer alarm",
	events.PrinterOffline:        "Printer offline",
	events.PrinterOnline:         "Printer back online",
	events.MaintenanceDue:        "Maintenance due",
	events.PrintAlmostDone:       "Almost done",
	events.PrintFailureSuspected: "Failure spotted by camera watch",
}

// Duration writes seconds the way the messages do: "2h 14m", "14m", "40s".
func Duration(sec int64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh %02dm", sec/3600, sec%3600/60)
	case sec >= 60:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%ds", sec)
}

// Compose turns an event into its words.
func Compose(e events.Event) Message {
	m := Message{Event: e}
	who := e.Printer
	if e.File != "" {
		if who != "" {
			who += " · "
		}
		who += e.File
	}
	add := func(s string) {
		if s != "" {
			m.Lines = append(m.Lines, s)
		}
	}
	// took describes how far a print got: "after 2h 14m at 45%".
	took := func() string {
		var parts []string
		if e.Record != nil && e.Record.Duration > 0 {
			parts = append(parts, "after "+Duration(e.Record.Duration))
		}
		if e.Progress > 0 {
			parts = append(parts, fmt.Sprintf("at %d%%", e.Progress))
		}
		return strings.Join(parts, " ")
	}
	grams := func() string {
		if e.Record != nil {
			g := e.Record.TotalGrams()
			if g == 0 && e.Record.FilamentMM > 0 {
				g = costs.GramsFromMM(e.Record.FilamentMM, "") // Klipper and OctoPrint count length
			}
			if g >= 0.5 {
				return fmt.Sprintf("%.0f g of filament", g)
			}
		}
		return ""
	}
	switch e.Kind {
	case events.PrintStarted:
		m.Title = "Print started"
		add(who)
		if e.Remaining > 0 {
			add("About " + Duration(int64(e.Remaining)*60) + " to go")
		}
	case events.PrintPaused:
		m.Title = "Print paused"
		add(who)
		add(e.Message)
	case events.PrintResumed:
		m.Title = "Print resumed"
		add(who)
	case events.PrintFinished:
		m.Title = "Print finished"
		add(who)
		line := ""
		if e.Record != nil && e.Record.Duration > 0 {
			line = "Took " + Duration(e.Record.Duration)
		}
		if g := grams(); g != "" {
			if line != "" {
				line += ", "
			}
			line += g
		}
		add(line)
	case events.PrintFailed, events.PrintCancelled:
		m.Title = "Print failed"
		verb := "Failed "
		if e.Kind == events.PrintCancelled {
			m.Title, verb = "Print cancelled", "Stopped "
		}
		add(who)
		if t := took(); t != "" {
			add(verb + t)
		}
		if g := grams(); g != "" {
			add(g + " used")
		}
	case events.PrinterError, events.PrinterAlarmCleared:
		as := eventAlarms(e)
		cleared := e.Kind == events.PrinterAlarmCleared
		switch {
		case cleared && len(as) == 1:
			m.Title = "Alarm cleared"
		case cleared:
			m.Title = fmt.Sprintf("%d alarms cleared", len(as))
		case len(as) > 1:
			m.Title = fmt.Sprintf("%d printer alarms", len(as))
		default:
			m.Title = "Printer alarm"
		}
		if e.Printer != "" {
			m.Title += ": " + e.Printer
		}
		for i, a := range as {
			if i == maxAlarmLines {
				add(fmt.Sprintf("And %d more. The Bridge dashboard lists them all.", len(as)-i))
				break
			}
			add(alarmLine(a))
		}
		if cleared {
			if len(as) == 1 && as[0].Lasted >= time.Minute {
				add("It lasted " + Duration(int64(as[0].Lasted.Seconds())) + ".")
			}
			break
		}
		for _, a := range as {
			if hmsCode.MatchString(a.Code) {
				m.Link = "https://wiki.bambulab.com/en/hms/error-code?code=" + a.Code
				break
			}
		}
	case events.PrinterOffline:
		m.Title = "Printer offline"
		add(e.Printer + " stopped reporting")
		add(e.Message)
	case events.PrinterOnline:
		m.Title = "Printer back online"
		add(e.Printer + " is reporting again")
	case events.MaintenanceDue:
		m.Title = "Maintenance due"
		add(e.Printer)
		add(e.Message)
	case events.PrintAlmostDone:
		m.Title = "Almost done"
		what := e.File
		if what == "" {
			what = "the print"
		}
		if e.Remaining > 0 {
			add(fmt.Sprintf("%s: about %d min left on %s.", e.Printer, e.Remaining, what))
		} else {
			add(e.Printer + ": the end is near.")
		}
	case events.PrintFailureSuspected:
		m.Title = "Print may have failed"
		what := e.File
		if what == "" {
			what = "the print"
		}
		add(fmt.Sprintf("The camera watch thinks %s on %s has failed. Check the printer: the Bridge has not paused it.", what, e.Printer))
		add(e.Message)
	case events.Test:
		m.Title = "Test from FoxTrack Bridge"
		add("If you can read this, this channel works.")
	default:
		m.Title = string(e.Kind)
		add(who)
		add(e.Message)
	}
	return m
}

// maxAlarmLines is how many alarms one message lists.
const maxAlarmLines = 5

// eventAlarms is the event's alarm list; an event made without one (an older
// caller, a test) counts as one alarm from its Code and Message.
func eventAlarms(e events.Event) []events.Alarm {
	if len(e.Alarms) > 0 {
		return e.Alarms
	}
	if e.Code == "" && e.Message == "" {
		return nil
	}
	return []events.Alarm{{Code: e.Code, Message: e.Message, Severity: e.Severity}}
}

// alarmLine is one alarm in words: "Filament ran out (code 0700_...)".
func alarmLine(a events.Alarm) string {
	switch {
	case a.Message == "":
		return "Alarm code " + a.Code + ". Check the printer."
	case a.Code == "":
		return a.Message
	}
	return a.Message + " (code " + a.Code + ")"
}

// linkText is what the alarm link says.
const linkText = "What this code means"

// Plain is the message as plain text.
func (m Message) Plain() string {
	return m.Title + "\n" + m.Body()
}

// Body is the message without its title, as plain text.
func (m Message) Body() string {
	s := strings.Join(m.Lines, "\n")
	if m.Link != "" {
		s += "\n" + linkText + ": " + m.Link
	}
	return s
}

var (
	// Discord reads Markdown anywhere in a line, so a file called
	// my_part_v2.gcode would turn italic.
	discordEsc = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "#", `\#`, "-", `\-`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`)
	slackEsc   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	tgEsc      = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// Markdown is the message for Discord: bold title, escaped lines, and the
// link in angle brackets (no preview card).
func (m Message) Markdown() string {
	s := "**" + discordEsc.Replace(m.Title) + "**"
	for _, l := range m.Lines {
		s += "\n" + discordEsc.Replace(l)
	}
	if m.Link != "" {
		s += "\n" + linkText + ": <" + m.Link + ">"
	}
	return s
}

// Slack is the message in Slack's mrkdwn.
func (m Message) Slack() string {
	s := "*" + slackEsc.Replace(m.Title) + "*"
	for _, l := range m.Lines {
		s += "\n" + slackEsc.Replace(l)
	}
	if m.Link != "" {
		s += "\n<" + m.Link + "|" + linkText + ">"
	}
	return s
}

// Telegram is the message in Telegram's HTML mode, which needs & < > escaped.
func (m Message) Telegram() string {
	s := "<b>" + tgEsc.Replace(m.Title) + "</b>"
	for _, l := range m.Lines {
		s += "\n" + tgEsc.Replace(l)
	}
	if m.Link != "" {
		s += "\n<a href=\"" + html.EscapeString(m.Link) + "\">" + linkText + "</a>"
	}
	return s
}

// HTML is the message as a small HTML fragment, for email.
func (m Message) HTML() string {
	s := "<p><strong>" + html.EscapeString(m.Title) + "</strong></p>"
	for _, l := range m.Lines {
		s += "<p style=\"margin:0\">" + html.EscapeString(l) + "</p>"
	}
	if m.Link != "" {
		s += "<p><a href=\"" + html.EscapeString(m.Link) + "\">" + linkText + "</a></p>"
	}
	return s
}

// Wants reports whether the channel is on and listens to this event for this
// printer. Quiet hours are the caller's business.
func Wants(ch config.NotifyChannel, e events.Event) bool {
	if !ch.Enabled {
		return false
	}
	kinds := ch.Events
	if len(kinds) == 0 {
		for _, k := range events.Default {
			kinds = append(kinds, string(k))
		}
	}
	ok := false
	for _, k := range kinds {
		if k == string(e.Kind) {
			ok = true
			break
		}
	}
	if !ok && e.Kind == events.PrinterAlarmCleared {
		// The all-clear goes wherever the alarm went.
		return Wants(ch, events.Event{Kind: events.PrinterError, PrinterID: e.PrinterID})
	}
	if !ok {
		return false
	}
	if len(ch.Printers) == 0 {
		return true
	}
	for _, id := range ch.Printers {
		if id == e.PrinterID {
			return true
		}
	}
	return false
}

// Quiet reports whether now falls inside the quiet hours (local time). A
// window that ends before it starts runs past midnight.
func Quiet(n *config.Notifications, now time.Time) bool {
	if n == nil {
		return false
	}
	s, ok1 := clock(n.QuietStart)
	e, ok2 := clock(n.QuietEnd)
	if !ok1 || !ok2 || s == e {
		return false
	}
	t := now.Hour()*60 + now.Minute()
	if s < e {
		return t >= s && t < e
	}
	return t >= s || t < e
}

// clock reads "HH:MM" as minutes since midnight.
func clock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// ValidClock reports whether s is empty or an "HH:MM" time.
func ValidClock(s string) bool {
	_, ok := clock(s)
	return s == "" || ok
}
