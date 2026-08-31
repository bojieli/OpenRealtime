//go:build opus

package webrtc

import "github.com/pion/opus"

// newRoundTripDecoder builds the decoder the adapter already uses inbound, so
// the encoder is checked against the exact implementation in the session path.
func newRoundTripDecoder() (*opus.Decoder, error) {
	decoder, err := opus.NewDecoderWithOutput(inboundRate, 1)
	if err != nil {
		return nil, err
	}
	return &decoder, nil
}
