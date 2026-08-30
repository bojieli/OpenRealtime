//go:build linux

package realtimecu

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockReviewSourceFileExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func renameReviewSourceNoReplace(
	directory, source, destination string, expectedRoot os.FileInfo,
) error {
	parent, err := os.Open(directory)
	if err != nil {
		return errors.New("open realtime computer-use source publication directory")
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || !os.SameFile(info, expectedRoot) {
		return errors.New("realtime computer-use source publication directory changed")
	}
	return unix.Renameat2(
		int(parent.Fd()), source, int(parent.Fd()), destination, unix.RENAME_NOREPLACE,
	)
}
