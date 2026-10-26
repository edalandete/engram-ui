//go:build linux

package launcher

import (
	"os/exec"
	"syscall"
)

// Pdeathsig tells the kernel to signal this child when its parent dies, for
// any reason including SIGKILL. Set before Start, so it applies from the
// moment the process exists.
//
// See reap_windows.go for what this is protecting against and why it matters:
// an orphaned `engram serve` holds the same SQLite file, listens on a port
// nobody recorded, and the next launch simply spawns another one.
func prepareReap(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// adoptReap has nothing to do: Pdeathsig is already in effect.
func adoptReap(cmd *exec.Cmd) {}
