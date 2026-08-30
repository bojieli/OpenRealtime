//go:build !linux && !darwin

package review

import (
	"errors"
	"os"
)

func renameDirectoryNoReplace(
	_ string, _ string, _ os.FileInfo, _ string, _ string, _ os.FileInfo,
) error {
	return errors.New("exclusive directory rename is unsupported on this platform")
}

func lockFileExclusive(_ *os.File) error {
	return errors.New("exclusive publication leases are unsupported on this platform")
}
