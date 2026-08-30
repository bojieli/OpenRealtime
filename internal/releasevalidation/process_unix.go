//go:build linux || darwin

package releasevalidation

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Gate commands commonly launch browsers and test servers. Kill the whole
// process group on cancellation so a timed-out release gate cannot leave a
// child serving or holding a browser profile after its report says failed.
func configureCommandCancellation(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
}
