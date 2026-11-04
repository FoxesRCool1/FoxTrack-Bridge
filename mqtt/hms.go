package mqtt

import (
	_ "embed"
	"encoding/json"
	"log"
	"strings"
	"sync"
)

// hms_en.json is the English text of Bambu's HMS and print error codes,
// built by hms_gen.go (sources and licence: THIRD_PARTY_LICENSES.md). "hms"
// is keyed on the 16 hex digits of attr+code, "err" on the 8 of print_error;
// the table has no wildcard or short forms, so a lookup is exact.
//
//go:embed hms_en.json
var hmsTableJSON []byte

var (
	hmsTableOnce sync.Once
	hmsTable     struct {
		HMS map[string]string `json:"hms"`
		Err map[string]string `json:"err"`
	}
)

func loadHMSTable() {
	hmsTableOnce.Do(func() {
		if err := json.Unmarshal(hmsTableJSON, &hmsTable); err != nil {
			log.Printf("HMS table unreadable, alarms show codes only: %v", err) // the table is embedded, so a test catches this
		}
	})
}

// alarmTextMax is the longest alarm message FoxTrack accepts.
const alarmTextMax = 300

// clipAlarmText fits a table text into alarmTextMax runes, cutting at the last
// full sentence, or else at a word with "..." added.
func clipAlarmText(s string) string {
	r := []rune(s)
	if len(r) <= alarmTextMax {
		return s
	}
	cut := string(r[:alarmTextMax-3])
	if i := strings.LastIndex(cut, ". "); i > alarmTextMax/2 {
		return cut[:i+1]
	}
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return cut + "..."
}

// hmsText is Bambu's description of an HMS entry, "" when the table lacks it.
func hmsText(attr, code uint32) string {
	loadHMSTable()
	return clipAlarmText(hmsTable.HMS[strings.ReplaceAll(HMSCode(attr, code), "_", "")])
}

// printErrorText is Bambu's description of a print_error value, "" when unknown.
func printErrorText(v int64) string {
	loadHMSTable()
	return clipAlarmText(hmsTable.Err[strings.ReplaceAll(PrintErrorCode(v), "_", "")])
}
