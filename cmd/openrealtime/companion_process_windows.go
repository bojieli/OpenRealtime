//go:build windows

package main

import (
	"os"
	"os/exec"
)

func configureCompanionProcess(_ *exec.Cmd) {}

func interruptCompanionProcess(process *os.Process) error { return process.Kill() }

func killCompanionProcess(process *os.Process) error { return process.Kill() }

func companionProcessMissing(_ error) bool { return false }
