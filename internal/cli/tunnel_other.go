//go:build !linux

package cli

import "os/exec"

// stopWithParent has no portable equivalent outside Linux; Close stops ssh.
func stopWithParent(*exec.Cmd) {}
