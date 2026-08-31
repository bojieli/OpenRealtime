//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func realtimeCUFileChangeIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", errors.New("Realtime-CU deployment file has no Linux inode identity")
	}
	return fmt.Sprintf("dev=%d;ino=%d;ctime=%d.%09d;nlink=%d",
		stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec, stat.Nlink), nil
}
