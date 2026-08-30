//go:build !linux

package main

import (
	"context"
	"errors"
)

func secureReadServeProfileFile(
	context.Context, string, int, func() error,
) ([]byte, error) {
	return nil, errors.New(
		"secure launch-profile file opening is unsupported on this platform; pass a profile through a verified launcher",
	)
}
