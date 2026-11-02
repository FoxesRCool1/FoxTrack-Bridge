package mqtt

import (
	"encoding/json"
	"fmt"
	"strings"

	"foxtrack-bridge/webhook"
)

// Bambu alarms come from two fields of the print report: the hms list
// ({attr, code} pairs) and print_error (one number). Both arrive only when
// they change, so the merged list lives in TelemetryData.Alarms and each
// report replaces only the part it carries.

// hmsEntry is one item of print.hms. attr and code are 32-bit values.
type hmsEntry struct {
	Attr uint32
	Code uint32
}

func (h *hmsEntry) UnmarshalJSON(b []byte) error {
	var raw struct {
		Attr any `json:"attr"`
		Code any `json:"code"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	a, _ := number(raw.Attr)
	c, _ := number(raw.Code)
	h.Attr, h.Code = uint32(int64(a)), uint32(int64(c))
	return nil
}

// HMSCode formats an HMS entry the way Bambu Studio and the Bambu wiki do:
// the 8 hex digits of attr, then the 8 of code, in groups of 4 joined by "_",
// e.g. 0300_1200_0002_0001.
func HMSCode(attr, code uint32) string {
	s := fmt.Sprintf("%08X%08X", attr, code)
	return s[0:4] + "_" + s[4:8] + "_" + s[8:12] + "_" + s[12:16]
}

// HMSSeverity reads the level Bambu Studio reads: bits 16-31 of code.
func HMSSeverity(code uint32) string {
	switch (code >> 16) & 0xFFFF {
	case 1:
		return webhook.AlarmFatal
	case 2:
		return webhook.AlarmSerious
	case 3:
		return webhook.AlarmCommon
	case 4:
		return webhook.AlarmInfo
	}
	return webhook.AlarmCommon
}

// PrintErrorCode formats print_error as Bambu shows it: XXXX_XXXX.
func PrintErrorCode(v int64) string {
	s := fmt.Sprintf("%08X", uint32(v))
	return s[0:4] + "_" + s[4:8]
}

// hmsAlarm turns one HMS entry into the alarm FoxTrack stores. Bambu's own
// text for a code is fetched by Bambu Studio from Bambu's servers, which the
// Bridge does not do, so message stays null and FoxTrack shows the code with
// a link to the Bambu wiki page for it.
func hmsAlarm(h hmsEntry) webhook.RelayAlarm {
	return webhook.CodeAlarm(webhook.AlarmSourceHMS, HMSCode(h.Attr, h.Code), HMSSeverity(h.Code), "")
}

// payloadHasAlarmKeys is the cheap check that decides whether a report can
// change the alarm list at all.
func payloadHasAlarmKeys(payload []byte) bool {
	s := string(payload)
	return strings.Contains(s, `"hms"`) || strings.Contains(s, `"print_error"`)
}

// mergeAlarms applies one report to the previous alarm list. A report with an
// hms key replaces every hms alarm (an empty list clears them); one with a
// print_error key replaces the print_error alarm (0 clears it). A report with
// neither keeps prev as it is.
func mergeAlarms(prev []webhook.RelayAlarm, payload []byte) []webhook.RelayAlarm {
	if !payloadHasAlarmKeys(payload) {
		return prev
	}
	var probe struct {
		Print struct {
			HMS        *[]hmsEntry `json:"hms"`
			PrintError *flexInt    `json:"print_error"`
		} `json:"print"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return prev
	}
	out := make([]webhook.RelayAlarm, 0, len(prev)+1)
	for _, a := range prev {
		if a.Source == webhook.AlarmSourceHMS && probe.Print.HMS != nil {
			continue
		}
		if a.Source == webhook.AlarmSourcePrintError && probe.Print.PrintError != nil {
			continue
		}
		out = append(out, a)
	}
	if probe.Print.PrintError != nil {
		if v := int64(*probe.Print.PrintError); v != 0 {
			out = append(out, webhook.CodeAlarm(webhook.AlarmSourcePrintError, PrintErrorCode(v), webhook.AlarmSerious, ""))
		}
	}
	if probe.Print.HMS != nil {
		for _, h := range *probe.Print.HMS {
			if h.Attr == 0 && h.Code == 0 {
				continue
			}
			out = append(out, hmsAlarm(h))
		}
	}
	return out
}

// alarmsChanged reports whether two alarm lists differ in any item.
func alarmsChanged(a, b []webhook.RelayAlarm) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].Source != b[i].Source || a[i].Severity != b[i].Severity ||
			strPtr(a[i].Code) != strPtr(b[i].Code) || strPtr(a[i].Message) != strPtr(b[i].Message) {
			return true
		}
	}
	return false
}

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// plugChanged reports a change FoxTrack must see at once: the plug came,
// went, switched, or stopped answering.
func plugChanged(a, b *webhook.RelayPlug) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	return a != nil && (a.On != b.On || a.Reachable != b.Reachable)
}
