//go:build !linux

package main

import (
	"errors"
	"os"
)

func realtimeCUFileChangeIdentity(os.FileInfo) (string, error) {
	return "", errors.New("the strict local Realtime-CU deployment verifier requires Linux procfs")
}
