//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

func renameMeetingProfileNoReplace(
	_ *os.Root, _ os.FileInfo, _, _ string,
) error {
	return errors.New("Meeting profile exclusive directory rename is unsupported")
}
