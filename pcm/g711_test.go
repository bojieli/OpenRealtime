package pcm_test

import (
	"math"
	"testing"

	"github.com/bojieli/OpenRealtime/pcm"
)

// The two silence bytes every telephone engineer knows: µ-law 0xFF is zero,
// and A-law 0xD5 is the smallest positive step.
func TestG711SilenceBytes(t *testing.T) {
	if got := pcm.EncodeMuLawSample(0); got != 0xFF {
		t.Errorf("µ-law of silence is 0x%02X, want 0xFF", got)
	}
	if got := pcm.DecodeMuLawSample(0xFF); got != 0 {
		t.Errorf("µ-law 0xFF decodes to %d, want 0", got)
	}
	if got := pcm.EncodeALawSample(0); got != 0xD5 {
		t.Errorf("A-law of silence is 0x%02X, want 0xD5", got)
	}
	if got := pcm.DecodeALawSample(0xD5); got != 8 {
		t.Errorf("A-law 0xD5 decodes to %d, want 8", got)
	}
}

// Companding is lossy by design, with error proportional to amplitude. What
// must hold is that decoding what was encoded lands within one quantisation
// step of the original, at every amplitude, in both laws.
func TestG711RoundTripStaysWithinAStep(t *testing.T) {
	for _, law := range []struct {
		name   string
		encode func(int16) byte
		decode func(byte) int16
	}{
		{"µ-law", pcm.EncodeMuLawSample, pcm.DecodeMuLawSample},
		{"A-law", pcm.EncodeALawSample, pcm.DecodeALawSample},
	} {
		for sample := int32(-32768); sample <= 32767; sample += 7 {
			decoded := int32(law.decode(law.encode(int16(sample))))
			magnitude := math.Abs(float64(sample))
			// One step is roughly 1/16 of the segment, and the top segment
			// steps by 1024; small values step by 16 (A) or 8 (µ) plus bias.
			tolerance := math.Max(magnitude/16+16, 40)
			if math.Abs(float64(decoded-sample)) > tolerance {
				t.Fatalf("%s: %d encoded and decoded to %d, off by more than %.0f",
					law.name, sample, decoded, tolerance)
			}
			if (sample > 40 && decoded < 0) || (sample < -40 && decoded > 0) {
				t.Fatalf("%s: %d lost its sign, decoded to %d", law.name, sample, decoded)
			}
		}
	}
}

// Every byte value decodes, and encoding the result gives the byte back: the
// decoder is the inverse of the encoder on the codec's own alphabet. µ-law has
// one exception the standard itself carries: 0x7F is negative zero, decodes
// to 0, and re-encodes as positive zero, 0xFF. It is the only byte that does.
func TestG711EveryByteIsAFixedPoint(t *testing.T) {
	for value := 0; value < 256; value++ {
		if value == 0x7F {
			if got := pcm.DecodeMuLawSample(0x7F); got != 0 {
				t.Errorf("µ-law negative zero decodes to %d", got)
			}
			continue
		}
		if got := pcm.EncodeMuLawSample(pcm.DecodeMuLawSample(byte(value))); got != byte(value) {
			t.Errorf("µ-law byte 0x%02X decodes to %d and re-encodes to 0x%02X",
				value, pcm.DecodeMuLawSample(byte(value)), got)
		}
		if got := pcm.EncodeALawSample(pcm.DecodeALawSample(byte(value))); got != byte(value) {
			t.Errorf("A-law byte 0x%02X decodes to %d and re-encodes to 0x%02X",
				value, pcm.DecodeALawSample(byte(value)), got)
		}
	}
}

// Buffers: little-endian in, one byte per sample out, and an odd trailing
// byte is not a sample.
func TestG711BuffersCarryWholeSamples(t *testing.T) {
	tone := make([]byte, 2*160+1) // 20 ms at 8 kHz plus a stray byte
	for index := 0; index < 160; index++ {
		sample := int16(8000 * math.Sin(2*math.Pi*float64(index)*440/8000))
		tone[index*2] = byte(uint16(sample))
		tone[index*2+1] = byte(uint16(sample) >> 8)
	}
	mu := pcm.EncodeMuLaw(tone)
	if len(mu) != 160 {
		t.Fatalf("µ-law of 160 samples is %d bytes", len(mu))
	}
	back := pcm.DecodeMuLaw(mu)
	if len(back) != 320 {
		t.Fatalf("decoded µ-law is %d bytes, want 320", len(back))
	}
	a := pcm.EncodeALaw(tone)
	if len(pcm.DecodeALaw(a)) != 320 {
		t.Fatalf("decoded A-law is %d bytes, want 320", len(pcm.DecodeALaw(a)))
	}
	// The tone survives: correlation with the original stays high.
	var dot, norm float64
	for index := 0; index < 160; index++ {
		original := float64(int16(uint16(tone[index*2]) | uint16(tone[index*2+1])<<8))
		decoded := float64(int16(uint16(back[index*2]) | uint16(back[index*2+1])<<8))
		dot += original * decoded
		norm += original * original
	}
	if dot/norm < 0.98 {
		t.Fatalf("the tone was distorted: correlation %.3f", dot/norm)
	}
}
