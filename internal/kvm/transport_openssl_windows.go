//go:build windows

package kvm

import (
	"os/exec"
	"syscall"
)

// configureChildProcess starts the openssl helper in a new process group, which
// on Windows also disables CTRL+C delivery to it. A console Ctrl-C therefore
// reaches only this program, whose shutdown path closes the helper's stdin so
// it can send TLS close_notify before exiting.
func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
