//go:build linux || darwin

package fileidentity

import (
	"errors"
	"os"
	"syscall"
)

func platformLinkCount(file *os.File) (uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, errors.New("file stat does not expose a link count")
	}
	return uint64(stat.Nlink), nil
}
