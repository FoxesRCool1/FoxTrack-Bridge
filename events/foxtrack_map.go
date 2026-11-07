package events

// foxtrackNames maps the Bridge event kinds that have a FoxTrack twin to the
// event name FoxTrack uses for them. The other kinds have no twin.
var foxtrackNames = map[Kind]string{
	PrintFinished:  "print_finished",
	PrintFailed:    "print_error",
	PrintCancelled: "print_cancelled",
	PrintPaused:    "print_paused",
	PrinterError:   "printer_error",
	PrinterOffline: "printer_silent",
}

// foxtrackToKind is the reverse of foxtrackNames, built once in init.
var foxtrackToKind = map[string]Kind{}

func init() {
	for k, name := range foxtrackNames {
		foxtrackToKind[name] = k
	}
}

// FoxTrackName returns the FoxTrack event name for a Bridge kind. The bool is
// false for kinds that have no FoxTrack twin.
func FoxTrackName(k Kind) (string, bool) {
	name, ok := foxtrackNames[k]
	return name, ok
}

// FromFoxTrackName returns the Bridge kind for a FoxTrack event name. The bool
// is false for names that are not a known FoxTrack event.
func FromFoxTrackName(name string) (Kind, bool) {
	k, ok := foxtrackToKind[name]
	return k, ok
}
