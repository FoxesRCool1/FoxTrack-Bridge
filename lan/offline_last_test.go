package lan

import (
	"testing"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// The OFFLINE report keeps the last known print, so FoxTrack (which stores
// each report as sent) does not show 0% and empty spools for an unreachable
// printer. Temperatures are unknown and go out as 0.
func TestOfflinePayload_KeepsLastPrint(t *testing.T) {
	last := &webhook.RelayPayload{Print: webhook.RelayPrint{
		GcodeState: "RUNNING", SubTaskName: "benchy.gcode", McPercent: 47, McRemainingTime: 12,
		NozzleTemper: 210, BedTemper: 60, Alarms: []webhook.RelayAlarm{},
		Ams:                []webhook.RelayAmsSlot{{Slot: 1, Material: "PLA"}},
		BridgeCapabilities: []string{"print_file"},
	}}
	p := mqttpkg.OfflinePayload(last, configpkg.KindKlipper, "Voron", nil).Print
	if p.GcodeState != "OFFLINE" || p.McRemainingTime != -1 || p.Alarms != nil {
		t.Errorf("state = %+v", p)
	}
	if p.McPercent != 47 || p.SubTaskName != "benchy.gcode" || len(p.Ams) != 1 {
		t.Errorf("the last print was not kept: %+v", p)
	}
	if p.NozzleTemper != 0 || p.BedTemper != 0 {
		t.Errorf("temperatures are unknown while offline: %+v", p)
	}
	if len(p.BridgeCapabilities) != 0 || p.PrinterModel != "Voron" {
		t.Errorf("capabilities must be plug-only and the model kept: %+v", p)
	}
	if last.Print.GcodeState != "RUNNING" {
		t.Error("the last report was changed")
	}
}
