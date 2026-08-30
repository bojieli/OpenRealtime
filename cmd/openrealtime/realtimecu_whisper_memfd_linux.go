//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func verifyRealtimeCUWhisperServiceHandle(path string) error {
	target, err := os.Readlink(path)
	if err != nil || target != "/memfd:openrealtime-whisper-service-v2 (deleted)" {
		return errors.New("Whisper listener service is not the reviewed sealed memfd")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("open Whisper sealed service handle")
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	closeErr := file.Close()
	want := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if err != nil || closeErr != nil || seals&want != want {
		return errors.New("Whisper listener service handle is not write-sealed")
	}
	return nil
}
