package mqtt

import (
	"log"
	"slices"

	"foxtrack-bridge/history"
)

// savedSession is a print in progress as it is kept on disk
// (history.SaveSession), so a Bridge restart in the middle of a print keeps the
// print's real start time and its spool snapshot.
type savedSession struct {
	FileName    string               `json:"file_name"`
	StartTime   int64                `json:"start_time"`
	NozzleTemp  float64              `json:"nozzle_temp"`
	BedTemp     float64              `json:"bed_temp"`
	TempSamples []history.TempSample `json:"temp_samples,omitempty"`
	LastSampleT int64                `json:"last_sample_t"`
	LastSeen    int64                `json:"last_seen"`
	LastPercent int                  `json:"last_percent"`
	Spools      []AmsSlot            `json:"spools,omitempty"`
}

// sessionKey is the printer's serial: it survives a rename and a restart.
func sessionKey(p Printer) string { return "bambu:" + p.Serial }

func (s *printSession) saved() savedSession {
	return savedSession{
		FileName: s.FileName, StartTime: s.StartTime,
		NozzleTemp: s.NozzleTemp, BedTemp: s.BedTemp,
		TempSamples: slices.Clone(s.TempSamples),
		LastSampleT: s.lastSampleT, LastSeen: s.lastSeen, LastPercent: s.lastPercent,
		Spools: slices.Clone(s.Spools),
	}
}

// restoreIfSame takes the saved session back into s when the printer is still
// printing the file it was saved for (history.Resumable). The caller holds
// sessionMu.
func (s *printSession) restoreIfSame(p Printer, fileName string, progress int, now int64) {
	var sv savedSession
	if !history.LoadSession(sessionKey(p), &sv) ||
		!history.Resumable(sv.FileName, sv.LastSeen, sv.LastPercent, fileName, progress, now) {
		return
	}
	log.Printf("[%s] print %q carries on after a restart; started %ds ago", p.Name, fileName, now-sv.StartTime)
	s.StartTime, s.NozzleTemp, s.BedTemp = sv.StartTime, sv.NozzleTemp, sv.BedTemp
	// The spools at the start of the print are what pair with the end for filament use.
	s.TempSamples, s.Spools = sv.TempSamples, sv.Spools
	if len(s.TempSamples) > 0 {
		s.lastSampleT = sv.LastSampleT
	}
}

func saveSession(p Printer, s *printSession) {
	if err := history.SaveSession(sessionKey(p), s.saved()); err != nil {
		log.Printf("[%s] could not save the print session: %v", p.Name, err)
	}
}

func forgetSession(p Printer) {
	if err := history.DeleteSession(sessionKey(p)); err != nil {
		log.Printf("[%s] could not remove the saved print session: %v", p.Name, err)
	}
}
