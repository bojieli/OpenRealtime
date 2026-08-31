//go:build !linux && !darwin

package ffmpeg

import (
	"errors"
	"os"
)

// The production FFmpeg plug-in requires Linux/Darwin ownership, /usr, ldd,
// and bubblewrap semantics. Other release targets still compile the clean API,
// but fail closed if an operator tries to instantiate this local toolchain.
func trustedFileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errors.New("ffmpeg toolchain identity is unsupported on this platform")
}

func validateTrustedOwner(os.FileInfo) error {
	return errors.New("ffmpeg toolchain ownership is unsupported on this platform")
}
