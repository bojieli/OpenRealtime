package webrtc

import (
	"fmt"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// AudioCodec selects what the adapter sends to the peer.
//
// The two are not equivalent and the choice is not cosmetic. PCMU is G.711 at
// 8 kHz: it carries a fraction of the band the session actually produces, and
// it is the default only because encoding it costs nothing and needs no
// library. Opus carries the session's real 24 kHz output, and needs libopus,
// which means cgo — so it lives behind a build tag rather than in the
// reproducible static release binaries. A build without that tag refuses the
// setting instead of quietly downgrading it.
type AudioCodec string

const (
	// AudioCodecPCMU is G.711 mu-law at 8 kHz. Always available.
	AudioCodecPCMU AudioCodec = "pcmu"
	// AudioCodecOpus is Opus, available in a build tagged `opus`.
	AudioCodecOpus AudioCodec = "opus"
)

// ParseAudioCodec resolves a configured name. Empty selects PCMU.
func ParseAudioCodec(value string) (AudioCodec, error) {
	switch AudioCodec(strings.ToLower(strings.TrimSpace(value))) {
	case "", AudioCodecPCMU:
		return AudioCodecPCMU, nil
	case AudioCodecOpus:
		if !OpusEncoderAvailable {
			return "", fmt.Errorf(
				"this build has no Opus encoder: rebuild with -tags opus and libopus available, or select %q",
				AudioCodecPCMU,
			)
		}
		return AudioCodecOpus, nil
	default:
		return "", fmt.Errorf("audio codec must be %q or %q, got %q",
			AudioCodecPCMU, AudioCodecOpus, value)
	}
}

// opusFrameDuration is the packetisation Opus is encoded at. 20 ms is what
// every WebRTC implementation in the field expects, and libopus requires a
// frame size it recognises rather than an arbitrary one.
const opusFrameDuration = 20 * time.Millisecond

// opusSamplesPerFrame is one 20 ms frame at the session rate.
const opusSamplesPerFrame = inboundRate / 50

// trackCapability is the RTP codec the adapter offers for its own audio.
//
// Opus is always advertised at 48 kHz stereo in SDP regardless of the rate it
// is encoded at; the clock rate is the RTP timebase, not the encoder's input.
func (codec AudioCodec) trackCapability() webrtc.RTPCodecCapability {
	if codec == AudioCodecOpus {
		return webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		}
	}
	return webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}
}

// sessionOutputFormat is what the adapter asks the protocol session to send
// it. Opus is encoded here from the session's own PCM, so nothing downstream
// has to know a second audio format exists.
func (codec AudioCodec) sessionOutputFormat() map[string]any {
	if codec == AudioCodecOpus {
		return map[string]any{"type": "audio/pcm", "rate": inboundRate}
	}
	return map[string]any{"type": "audio/pcmu"}
}

// opusEncoder encodes 24 kHz mono PCM16 into Opus packets.
type opusEncoder interface {
	// EncodeFrame encodes exactly opusSamplesPerFrame samples.
	EncodeFrame(pcm []int16) ([]byte, error)
	Close()
}

// decodePCM16 reads little-endian PCM16 into samples. It is the inverse of
// encodePCM16 and drops a trailing odd byte, which a truncated delta can
// produce.
func decodePCM16(payload []byte) []int16 {
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(uint16(payload[index*2]) | uint16(payload[index*2+1])<<8)
	}
	return samples
}
