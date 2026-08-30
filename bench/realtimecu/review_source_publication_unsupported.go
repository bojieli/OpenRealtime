//go:build !linux && !darwin

package realtimecu

import (
	"errors"
	"os"
)

func lockReviewSourceFileExclusive(_ *os.File) error {
	return errors.New("realtime computer-use source publication leases are unsupported on this platform")
}

func renameReviewSourceNoReplace(
	_ string, _ string, _ string, _ os.FileInfo,
) error {
	return errors.New("exclusive realtime computer-use source publication is unsupported on this platform")
}
