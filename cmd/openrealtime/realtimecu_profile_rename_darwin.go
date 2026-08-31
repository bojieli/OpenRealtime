//go:build darwin

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameRealtimeCUProfileNoReplace(
	parent *os.Root, expected os.FileInfo, source, destination string,
) (resultErr error) {
	directory, err := parent.Open(".")
	if err != nil {
		return errors.New("open Realtime-CU profile rename parent")
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	opened, err := directory.Stat()
	if err != nil || expected == nil || !os.SameFile(opened, expected) {
		return errors.New("Realtime-CU profile rename parent identity changed")
	}
	return unix.RenameatxNp(
		int(directory.Fd()), source, int(directory.Fd()), destination,
		unix.RENAME_EXCL,
	)
}
