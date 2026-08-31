//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCompanionProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptCompanionProcess(process *os.Process) error {
	return syscall.Kill(-process.Pid, syscall.SIGINT)
}

func killCompanionProcess(process *os.Process) error {
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}

func companionProcessMissing(err error) bool { return err == syscall.ESRCH }
