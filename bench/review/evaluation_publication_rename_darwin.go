//go:build darwin

package review

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameDirectoryNoReplace(
	sourceParent, sourceName string, expectedSource os.FileInfo,
	destinationParent, destinationName string, expectedDestination os.FileInfo,
) error {
	source, err := os.Open(sourceParent)
	if err != nil {
		return errors.New("open exclusive-rename source parent")
	}
	defer source.Close()
	destination := source
	if destinationParent != sourceParent {
		destination, err = os.Open(destinationParent)
		if err != nil {
			return errors.New("open exclusive-rename destination parent")
		}
		defer destination.Close()
	}
	sourceInfo, sourceErr := source.Stat()
	destinationInfo, destinationErr := destination.Stat()
	if sourceErr != nil || destinationErr != nil ||
		!os.SameFile(sourceInfo, expectedSource) ||
		!os.SameFile(destinationInfo, expectedDestination) {
		return errors.New("exclusive-rename parent identity changed")
	}
	return unix.RenameatxNp(
		int(source.Fd()), sourceName, int(destination.Fd()), destinationName,
		unix.RENAME_EXCL,
	)
}

func lockFileExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
