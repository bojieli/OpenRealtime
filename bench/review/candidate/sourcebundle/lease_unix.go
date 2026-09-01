//go:build linux || darwin

package sourcebundle

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockSourceBundleFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
