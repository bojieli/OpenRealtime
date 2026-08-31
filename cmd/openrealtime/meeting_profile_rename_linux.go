//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameMeetingProfileNoReplace(
	parent *os.Root, expected os.FileInfo, source, destination string,
) (resultErr error) {
	directory, err := parent.Open(".")
	if err != nil {
		return errors.New("open Meeting profile rename parent")
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	opened, err := directory.Stat()
	if err != nil || expected == nil || !os.SameFile(opened, expected) {
		return errors.New("Meeting profile rename parent identity changed")
	}
	return unix.Renameat2(
		int(directory.Fd()), source, int(directory.Fd()), destination,
		unix.RENAME_NOREPLACE,
	)
}
