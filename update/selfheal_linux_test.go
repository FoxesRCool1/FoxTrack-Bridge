//go:build linux

package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitFromCgroup(t *testing.T) {
	cases := []struct {
		name   string
		cgroup string
		want   string
	}{
		{
			name:   "systemd user unit",
			cgroup: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/foxtrack-bridge.service\n",
			want:   "foxtrack-bridge.service",
		},
		{
			name:   "system template unit",
			cgroup: "0::/system.slice/system-foxtrack\\x2dbridge.slice/foxtrack-bridge@caleb.service\n",
			want:   "foxtrack-bridge@caleb.service",
		},
		{
			name:   "trailing slash",
			cgroup: "0::/user.slice/app.slice/foxtrack-bridge.service/\n",
			want:   "foxtrack-bridge.service",
		},
		{
			name:   "not a service (a container or a plain shell)",
			cgroup: "0::/user.slice/user-1000.slice/session-2.scope\n",
			want:   "",
		},
		{
			name:   "empty",
			cgroup: "",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitFromCgroup(tc.cgroup); got != tc.want {
				t.Errorf("unitFromCgroup = %q, want %q", got, tc.want)
			}
		})
	}
}

// Off systemd there is no unit to repair, and the bridge must start anyway.
func TestCurrentSystemdUnitEmptyWithoutInvocationID(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	if got := currentSystemdUnit(); got != "" {
		t.Errorf("currentSystemdUnit = %q, want empty when not run by systemd", got)
	}
}

func TestEnsureRestartAlwaysNoOpWithoutSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	msg, err := EnsureRestartAlways()
	if err != nil {
		t.Fatalf("unexpected error off systemd: %v", err)
	}
	if msg != "" {
		t.Errorf("message = %q, want empty off systemd", msg)
	}
}

// The drop-in must be whole or absent — systemd refuses to parse a truncated
// one, which would take the unit down instead of repairing it.
func TestWriteFileAtomicReplacesWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "10-restart-always.conf")

	if err := os.WriteFile(path, []byte("[Service]\nRestart=on-failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte(restartDropInBody), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != restartDropInBody {
		t.Errorf("drop-in was not replaced whole:\n%s", got)
	}
	if !strings.Contains(string(got), "Restart=always") {
		t.Error("drop-in does not set Restart=always")
	}

	// No temp files may be left behind for systemd to trip over.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the drop-in", names)
	}
}

// The override is a systemd drop-in, so it must carry a [Service] section.
func TestRestartDropInBodyIsValidOverride(t *testing.T) {
	if !strings.Contains(restartDropInBody, "[Service]") {
		t.Error("drop-in is missing the [Service] section header")
	}
	if !strings.Contains(restartDropInBody, "Restart=always") {
		t.Error("drop-in does not set Restart=always")
	}
}
