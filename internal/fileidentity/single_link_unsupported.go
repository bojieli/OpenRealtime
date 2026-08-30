//go:build !linux && !darwin && !windows

package fileidentity

import (
	"errors"
	"os"
)

func platformLinkCount(*os.File) (uint64, error) {
	return 0, errors.New("file link counts are unsupported on this platform")
}
