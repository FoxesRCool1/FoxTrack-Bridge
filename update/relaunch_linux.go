//go:build linux

package update

import (
	"os"
	"os/exec"
	"syscall"
)

// relaunchDetached starts the updated binary in a new session so it survives
// this process exiting. It is only used when no service manager is going to
// restart the bridge for us — see supervisorRestarts.
func relaunchDetached(exePath string) error {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(exePath)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
