//go:build !windows

package selfupdate

import "errors"

// RunElevated exists for Windows only, where the releases are.
func RunElevated(exe string, args []string) (uint32, error) {
	return 0, errors.New("run it again as root")
}
