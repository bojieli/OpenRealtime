//go:build !linux && !darwin

package management

import (
	"errors"
	"os"
)

func atomicSourcePublicationSupported() bool { return false }

func publishSourceCreate(*os.Root, os.FileInfo, string, string) error {
	return errors.New("atomic source creation is unsupported")
}

func publishSourceUpdate(*os.Root, os.FileInfo, string, string) error {
	return errors.New("atomic source replacement is unsupported")
}
