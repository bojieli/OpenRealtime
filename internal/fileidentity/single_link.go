// Package fileidentity provides fail-closed identity checks for retained files.
package fileidentity

import (
	"errors"
	"os"
)

// RequireSingleLink rejects a file that has another hard-link name. A sealed
// artifact with an external hard link could otherwise be modified through a
// path outside the verified tree without changing its inode identity.
func RequireSingleLink(file *os.File) error {
	if file == nil {
		return errors.New("file identity handle is nil")
	}
	links, err := platformLinkCount(file)
	if err != nil {
		return errors.New("inspect file link count")
	}
	if links != 1 {
		return errors.New("file has an external hard link")
	}
	return nil
}
