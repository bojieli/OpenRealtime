//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

func renameRealtimeCUProfileNoReplace(
	_ *os.Root, _ os.FileInfo, _, _ string,
) error {
	return errors.New("Realtime-CU profile exclusive directory rename is unsupported")
}
