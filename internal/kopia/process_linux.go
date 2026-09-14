//go:build linux

package kopia

import (
	"os/exec"
	"syscall"
)

// Do not leave an upload process behind if the manager is killed abruptly.
func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
