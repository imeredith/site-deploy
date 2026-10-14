package cli

import (
	"os/exec"
	"syscall"
)

// stopWithParent ends ssh if the CLI dies without closing the tunnel, for
// example when a CI job is cancelled.
func stopWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
