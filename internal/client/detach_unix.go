//go:build unix

package client

import (
	"os/exec"
	"syscall"
)

// detach puts the spawned daemon in its own session so it survives the hook
// process exiting.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
