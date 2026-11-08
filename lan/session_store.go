package lan

import (
	"log"
	"slices"

	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
)

// savedSession is a print in progress as it is kept on disk
// (history.SaveSession), so a Bridge restart in the middle of a print keeps the
// print's real start time.
type savedSession struct {
	FileName       string               `json:"file_name"`
	StartTime      int64                `json:"start_time"`
	NozzleTemp     float64              `json:"nozzle_temp"`
	BedTemp        float64              `json:"bed_temp"`
	TempSamples    []history.TempSample `json:"temp_samples,omitempty"`
	LastSampleT    int64                `json:"last_sample_t"`
	LastSeen       int64                `json:"last_seen"`
	LastProgress   int                  `json:"last_progress"`
	LastFilamentMM float64              `json:"last_filament_mm"`
}

// sessionKey is the printer's stable config ID, which survives a rename and a
// restart; a printer with none (never saved yet) goes by its name.
func (c *Controller) sessionKey(name string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.printers[name]; ok && p.ID != "" {
		return "id:" + p.ID
	}
	return "name:" + name
}

func (s *printSession) saved() savedSession {
	return savedSession{
		FileName: s.FileName, StartTime: s.StartTime,
		NozzleTemp: s.NozzleTemp, BedTemp: s.BedTemp,
		TempSamples: slices.Clone(s.TempSamples),
		LastSampleT: s.lastSampleT, LastSeen: s.lastSeen,
		LastProgress: s.lastProgress, LastFilamentMM: s.lastFilamentMM,
	}
}

// restoreIfSame takes the session saved under key back into s when the printer
// is still printing the file it was saved for (history.Resumable).
func (s *printSession) restoreIfSame(name, key string, t *mqttpkg.TelemetryData, now int64) {
	var sv savedSession
	if !history.LoadSession(key, &sv) ||
		!history.Resumable(sv.FileName, sv.LastSeen, sv.LastProgress, t.FileName, t.Progress, now) {
		return
	}
	log.Printf("[%s] print %q carries on after a restart; started %ds ago", name, t.FileName, now-sv.StartTime)
	s.StartTime, s.NozzleTemp, s.BedTemp = sv.StartTime, sv.NozzleTemp, sv.BedTemp
	s.TempSamples, s.lastSampleT = sv.TempSamples, sv.LastSampleT
	s.lastFilamentMM = max(sv.LastFilamentMM, s.lastFilamentMM)
}

func saveSession(name, key string, s *printSession) {
	if err := history.SaveSession(key, s.saved()); err != nil {
		log.Printf("[%s] could not save the print session: %v", name, err)
	}
}

func forgetSession(name, key string) {
	if err := history.DeleteSession(key); err != nil {
		log.Printf("[%s] could not remove the saved print session: %v", name, err)
	}
}
