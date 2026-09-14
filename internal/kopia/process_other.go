//go:build !linux

package kopia

import "os/exec"

func prepareCommand(cmd *exec.Cmd) {}
