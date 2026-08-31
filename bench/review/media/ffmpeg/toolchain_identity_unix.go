//go:build linux || darwin

package ffmpeg

import (
	"errors"
	"os"
	"syscall"
)

func trustedFileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, errors.New("file stat does not expose a stable identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func validateTrustedOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || stat.Uid != 0 {
		return errors.New("file stat does not expose root ownership")
	}
	return nil
}
