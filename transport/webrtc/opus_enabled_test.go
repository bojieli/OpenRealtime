//go:build opus

package webrtc

import (
	"math"
	"testing"
)

// The encoder has to accept exactly the frame the adapter feeds it and return
// a packet a decoder recognises. Encoding is the half of Opus this repository
// did not have, so this checks it produces something pion/opus can read back
// at the session's own rate.
func TestOpusEncoderProducesDecodablePackets(t *testing.T) {
	if !OpusEncoderAvailable {
		t.Fatal("this build was tagged opus but reports no encoder")
	}
	encoder, err := newOpusEncoder(inboundRate, 1)
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	defer encoder.Close()

	// A 440 Hz tone rather than silence: silence compresses to a handful of
	// bytes and would pass even if the encoder were dropping the signal.
	pcm := make([]int16, opusSamplesPerFrame)
	for index := range pcm {
		pcm[index] = int16(12_000 * math.Sin(2*math.Pi*440*float64(index)/inboundRate))
	}
	frame, err := encoder.EncodeFrame(pcm)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if len(frame) < 8 {
		t.Fatalf("encoded frame is %d bytes, which is too small to carry a tone", len(frame))
	}

	decoder, err := newRoundTripDecoder()
	if err != nil {
		t.Fatalf("new decoder: %v", err)
	}
	samples := make([]int16, inboundRate/10)
	count, err := decoder.DecodeToInt16(frame, samples)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if count != opusSamplesPerFrame {
		t.Fatalf("decoded %d samples, want %d", count, opusSamplesPerFrame)
	}
	var peak int16
	for _, sample := range samples[:count] {
		if sample > peak {
			peak = sample
		}
	}
	if peak < 2_000 {
		t.Fatalf("decoded peak amplitude %d: the signal did not survive the round trip", peak)
	}
}
