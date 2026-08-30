//go:build linux

package campaign

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameAggregateNoReplace(
	sourceParent, sourceName string, expectedSource os.FileInfo,
	destinationParent, destinationName string, expectedDestination os.FileInfo,
) error {
	source, err := os.Open(sourceParent)
	if err != nil {
		return errors.New("open candidate campaign rename source parent")
	}
	defer source.Close()
	destination := source
	if destinationParent != sourceParent {
		destination, err = os.Open(destinationParent)
		if err != nil {
			return errors.New("open candidate campaign rename destination parent")
		}
		defer destination.Close()
	}
	sourceInfo, sourceErr := source.Stat()
	destinationInfo, destinationErr := destination.Stat()
	if sourceErr != nil || destinationErr != nil ||
		!os.SameFile(sourceInfo, expectedSource) ||
		!os.SameFile(destinationInfo, expectedDestination) {
		return errors.New("candidate campaign rename parent identity changed")
	}
	return unix.Renameat2(
		int(source.Fd()), sourceName, int(destination.Fd()), destinationName,
		unix.RENAME_NOREPLACE,
	)
}
