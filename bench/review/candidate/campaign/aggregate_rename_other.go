//go:build !linux && !darwin

package campaign

import (
	"errors"
	"os"
)

func renameAggregateNoReplace(
	_ string, _ string, _ os.FileInfo, _ string, _ string, _ os.FileInfo,
) error {
	return errors.New("candidate campaign exclusive directory rename is unsupported on this platform")
}
