//go:build linux

package meeting

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockMeetingSourceFileExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func renameMeetingSourceMarkerNoReplace(
	directory, source, destination string, expectedRoot os.FileInfo,
) error {
	parent, err := os.Open(directory)
	if err != nil {
		return errors.New("open meeting source publication directory")
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || !os.SameFile(info, expectedRoot) {
		return errors.New("meeting source publication directory changed")
	}
	return unix.Renameat2(
		int(parent.Fd()), source, int(parent.Fd()), destination, unix.RENAME_NOREPLACE,
	)
}
