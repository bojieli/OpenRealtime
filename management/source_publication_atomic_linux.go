//go:build linux

package management

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func atomicSourcePublicationSupported() bool { return true }

func publishSourceCreate(
	parent *os.Root, expected os.FileInfo, source, destination string,
) error {
	return sourceRenameAt(parent, expected, source, destination, unix.RENAME_NOREPLACE)
}

func publishSourceUpdate(
	parent *os.Root, expected os.FileInfo, source, destination string,
) error {
	return sourceRenameAt(parent, expected, source, destination, unix.RENAME_EXCHANGE)
}

func sourceRenameAt(
	parent *os.Root, expected os.FileInfo, source, destination string, flags uint,
) error {
	directory, err := parent.Open(".")
	if err != nil {
		return errors.New("open source-publication parent")
	}
	defer func() { _ = directory.Close() }()
	opened, err := directory.Stat()
	if err != nil || expected == nil || !os.SameFile(opened, expected) {
		return errors.New("source-publication parent identity changed")
	}
	return unix.Renameat2(
		int(directory.Fd()), source, int(directory.Fd()), destination, flags,
	)
}
