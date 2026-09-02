//go:build linux || darwin

package campaign

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockAggregatePublicationFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
