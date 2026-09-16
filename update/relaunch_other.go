//go:build !linux

package update

import "fmt"

// relaunchDetached is only reachable from the Linux apply path; Windows and
// macOS relaunch from their helper scripts instead.
func relaunchDetached(exePath string) error {
	return fmt.Errorf("detached relaunch is not supported on this platform")
}
