package mqtt

import (
	"testing"

	"foxtrack-bridge/webhook"
)

func TestHMSCode_FormatsFourGroups(t *testing.T) {
	// attr 0x03001200, code 0x00020001: "0300_1200_0002_0001" (Bambu wiki form).
	if got := HMSCode(0x03001200, 0x00020001); got != "0300_1200_0002_0001" {
		t.Fatalf("HMSCode = %q", got)
	}
	if got := HMSCode(0x0C000100, 0x00010001); got != "0C00_0100_0001_0001" {
		t.Fatalf("HMSCode = %q", got)
	}
}

func TestHMSSeverity_ReadsBits16To31(t *testing.T) {
	cases := map[uint32]string{
		0x00010001: webhook.AlarmFatal,
		0x00020001: webhook.AlarmSerious,
		0x00030001: webhook.AlarmCommon,
		0x00040001: webhook.AlarmInfo,
		0x00090001: webhook.AlarmCommon, // unknown level: common
	}
	for code, want := range cases {
		if got := HMSSeverity(code); got != want {
			t.Errorf("HMSSeverity(%08X) = %q, want %q", code, got, want)
		}
	}
}

func TestPrintErrorCode_Format(t *testing.T) {
	if got := PrintErrorCode(0x03008003); got != "0300_8003" {
		t.Fatalf("PrintErrorCode = %q", got)
	}
}

func codes(list []webhook.RelayAlarm) []string {
	var out []string
	for _, a := range list {
		out = append(out, a.Source+":"+strPtr(a.Code)+":"+a.Severity)
	}
	return out
}

func TestMergeAlarms_KeepsLastFullListAcrossPartialPushes(t *testing.T) {
	full := []byte(`{"print":{"hms":[{"attr":50336256,"code":131073},{"attr":201326848,"code":65537}],"print_error":0}}`)
	got := mergeAlarms(nil, full)
	want := []string{"hms:0300_1200_0002_0001:serious", "hms:0C00_0100_0001_0001:fatal"}
	if g := codes(got); len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
		t.Fatalf("after full = %v, want %v", g, want)
	}

	// A partial push without hms or print_error changes nothing.
	partial := []byte(`{"print":{"nozzle_temper":210.5,"mc_percent":42}}`)
	if g := codes(mergeAlarms(got, partial)); len(g) != 2 {
		t.Fatalf("after partial = %v, want the 2 kept", g)
	}

	// print_error alone adds its item and keeps the hms ones.
	withErr := mergeAlarms(got, []byte(`{"print":{"print_error":50364419}}`))
	if g := codes(withErr); len(g) != 3 || g[2] != "print_error:0300_8003:serious" {
		t.Fatalf("after print_error = %v", g)
	}

	// print_error back to 0 drops only that item.
	cleared := mergeAlarms(withErr, []byte(`{"print":{"print_error":0}}`))
	if g := codes(cleared); len(g) != 2 || g[0] != want[0] {
		t.Fatalf("after print_error 0 = %v", g)
	}

	// An empty hms list clears the hms items.
	none := mergeAlarms(cleared, []byte(`{"print":{"hms":[]}}`))
	if len(none) != 0 {
		t.Fatalf("after empty hms = %v, want none", codes(none))
	}
}

func TestMergeAlarms_StringNumbersAndUnchanged(t *testing.T) {
	got := mergeAlarms(nil, []byte(`{"print":{"hms":[{"attr":"50336256","code":"131073"}]}}`))
	if g := codes(got); len(g) != 1 || g[0] != "hms:0300_1200_0002_0001:serious" {
		t.Fatalf("string numbers = %v", g)
	}
	if alarmsChanged(got, mergeAlarms(got, []byte(`{"print":{"hms":[{"attr":50336256,"code":131073}]}}`))) {
		t.Fatal("same list reported as changed")
	}
	if !alarmsChanged(got, nil) {
		t.Fatal("cleared list not reported as changed")
	}
}
