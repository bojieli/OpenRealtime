//go:build opus

package webrtc

import (
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// OpusEncoderAvailable reports whether this build can encode Opus.
const OpusEncoderAvailable = true

// libopusEncoder wraps libopus for the one shape this adapter needs: mono
// 24 kHz PCM16 in, one Opus packet per 20 ms frame out.
type libopusEncoder struct {
	encoder *opus.Encoder
	packet  []byte
}

func newOpusEncoder(sampleRate, channels int) (opusEncoder, error) {
	// AppVoIP tells libopus this is speech under a latency budget, which is
	// what changes how it spends bits.
	encoder, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("create the Opus encoder: %w", err)
	}
	// One frame never approaches this; the buffer only has to be large enough
	// that libopus never has to refuse a write.
	return &libopusEncoder{encoder: encoder, packet: make([]byte, 4_000)}, nil
}

func (encoder *libopusEncoder) EncodeFrame(pcm []int16) ([]byte, error) {
	written, err := encoder.encoder.Encode(pcm, encoder.packet)
	if err != nil {
		return nil, fmt.Errorf("encode an Opus frame: %w", err)
	}
	if written <= 0 {
		return nil, nil
	}
	frame := make([]byte, written)
	copy(frame, encoder.packet[:written])
	return frame, nil
}

func (encoder *libopusEncoder) Close() {}
