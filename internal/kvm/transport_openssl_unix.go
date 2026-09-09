//go:build !windows

package kvm

import (
	"os/exec"
	"syscall"
)

// configureChildProcess puts the openssl helper in its own process group so a
// terminal-generated SIGINT/SIGQUIT is delivered to this program only. The
// helper then exits through the normal path: our Close closes its stdin, it
// sends TLS close_notify and terminates. If this process dies abruptly instead,
// the kernel closes the pipe and the helper still sees EOF and exits.
func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
