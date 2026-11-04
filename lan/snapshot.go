package lan

import (
	"errors"
	"fmt"

	"foxtrack-bridge/capture"
	configpkg "foxtrack-bridge/config"
)

// Snapshot reads one JPEG from a Klipper, OctoPrint or custom webcam: the
// still-image address first, then the same guesses the live view tries (a
// stream answers with its first frame).
func Snapshot(p configpkg.Printer) ([]byte, error) {
	urls := cameraCandidates(p)
	if u := snapshotURL(p); u != "" {
		urls = append([]string{u}, urls...)
	}
	if len(urls) == 0 {
		return nil, errors.New("this printer has no camera the Bridge can read")
	}
	var lastErr error
	for _, u := range urls {
		frame, err := capture.KlipperFrame(u)
		if err == nil {
			return frame, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no camera answered: %w", lastErr)
}
