//go:build !windows && !linux

package launcher

import "os/exec"

// No parent-death mechanism on this platform.
//
// macOS has no Pdeathsig and no Job Objects; the usual substitute is a watcher
// goroutine in the child, which is not available here — engram is a binary we
// consume, not code we control. So on darwin and the BSDs an abrupt kill of
// engram-ui still leaves the subprocess behind. Stop() covers every orderly
// exit, which after the run()/os.Exit fix is the overwhelming majority.
func prepareReap(cmd *exec.Cmd) {}
func adoptReap(cmd *exec.Cmd)   {}
