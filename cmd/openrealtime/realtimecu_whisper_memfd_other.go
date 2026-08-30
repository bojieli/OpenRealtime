//go:build !linux

package main

import "errors"

func verifyRealtimeCUWhisperServiceHandle(string) error {
	return errors.New("the strict local Whisper deployment requires Linux sealed memfd support")
}
