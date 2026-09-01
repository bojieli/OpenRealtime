//go:build windows

package sourcebundle

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockSourceBundleFile(file *os.File) error {
	overlapped := &windows.Overlapped{}
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
}
