//go:build !linux && !darwin

package meeting

import (
	"errors"
	"os"
)

func lockMeetingSourceFileExclusive(*os.File) error {
	return errors.New("meeting source publication leases are unsupported on this platform")
}

func renameMeetingSourceMarkerNoReplace(
	string, string, string, os.FileInfo,
) error {
	return errors.New("exclusive meeting source publication is unsupported on this platform")
}
