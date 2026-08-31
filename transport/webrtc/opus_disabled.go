//go:build !opus

package webrtc

import "errors"

// OpusEncoderAvailable reports whether this build can encode Opus. The
// reproducible, statically linked release binaries are built without cgo, so
// they cannot, and say so rather than downgrading a configured codec.
const OpusEncoderAvailable = false

func newOpusEncoder(int, int) (opusEncoder, error) {
	return nil, errors.New("this build has no Opus encoder; rebuild with -tags opus")
}
