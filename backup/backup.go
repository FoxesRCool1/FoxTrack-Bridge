// Package backup keeps daily copies of the Bridge's config.json and
// history.json in <config dir>/backups/, retaining the newest 7 of each.
package backup

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"foxtrack-bridge/config"
)

const (
	// maxCopies is how many dated copies of each file are kept.
	maxCopies = 7
	// interval is how often Loop runs a backup.
	interval = 6 * time.Hour
)

var (
	configCopyRe  = regexp.MustCompile(`^config-\d{4}-\d{2}-\d{2}\.json$`)
	historyCopyRe = regexp.MustCompile(`^history-\d{4}-\d{2}-\d{2}\.json$`)
)

// Run copies config.json and history.json from dir (if present) to
// dir/backups/<name>-YYYY-MM-DD.json, skipping a copy for today that already
// exists, and prunes each kind down to the newest maxCopies. A missing source
// file is not an error; the sources themselves are never touched.
func Run(dir string, now time.Time) error {
	dir = filepath.Clean(dir)
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0700); err != nil {
		return err
	}
	date := now.Format("2006-01-02")

	for _, name := range []string{"config.json", "history.json"} {
		src := filepath.Join(dir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		dst := filepath.Join(backups, name[:len(name)-len(".json")]+"-"+date+".json")
		if _, err := os.Stat(dst); err == nil {
			// Today's copy already exists; skip it.
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := config.WriteFileAtomic(dst, data, 0600); err != nil {
			return err
		}
	}

	if err := prune(backups, configCopyRe); err != nil {
		return err
	}
	return prune(backups, historyCopyRe)
}

// prune deletes the oldest copies matching re so that at most maxCopies
// remain. Only files matching the pattern are touched.
func prune(dir string, re *regexp.Regexp) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if re.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := 0; i < len(names)-maxCopies; i++ {
		if err := os.Remove(filepath.Join(dir, names[i])); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Loop runs a backup once at startup and then every 6 hours. It never
// returns or panics.
func Loop() {
	runAndLog()
	for range time.Tick(interval) {
		runAndLog()
	}
}

func runAndLog() {
	if err := Run(config.ConfigDir(), time.Now()); err != nil {
		log.Printf("[backup] daily backup failed: %v", err)
	}
}
