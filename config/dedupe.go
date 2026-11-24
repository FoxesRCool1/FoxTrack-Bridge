package config

import (
	"fmt"
	"log"
	"strings"
)

// maxPrinterNameRunes is the longest printer name the Bridge accepts (the
// server refuses longer ones on add, edit and import).
const maxPrinterNameRunes = 128

func foldName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// dedupePrinterNames renames printers that share a name (compared without
// case or outer spaces) and reports whether it changed anything. Control,
// history and live state are keyed by name, so two printers with one name
// collide; configs written before the uniqueness check can still hold such a
// pair. The first printer keeps the name, each later one gets " (2)", " (3)"
// and so on, skipping any name or previous name already in use. Only the name
// changes: the ID and the serial stay, so FoxTrack sees the same printer
// (RelayIdentity). Blank names are left alone.
func dedupePrinterNames(printers []Printer) bool {
	taken := make(map[string]bool, len(printers))
	for _, p := range printers {
		taken[foldName(p.Name)] = true
		for _, prev := range p.PreviousNames {
			taken[foldName(prev)] = true
		}
	}
	seen := make(map[string]bool, len(printers))
	changed := false
	for i := range printers {
		key := foldName(printers[i].Name)
		if key == "" || !seen[key] {
			seen[key] = true
			continue
		}
		base := strings.TrimSpace(printers[i].Name)
		for n := 2; ; n++ {
			suffix := fmt.Sprintf(" (%d)", n)
			b := []rune(base)
			if over := len(b) + len(suffix) - maxPrinterNameRunes; over > 0 {
				b = b[:len(b)-over]
			}
			name := strings.TrimSpace(string(b)) + suffix
			if taken[foldName(name)] {
				continue
			}
			log.Printf("[config] two printers were named %q; one is now %q", printers[i].Name, name)
			printers[i].Name = name
			taken[foldName(name)] = true
			seen[foldName(name)] = true
			changed = true
			break
		}
	}
	return changed
}
