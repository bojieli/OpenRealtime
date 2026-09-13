package asrbuffer

import (
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// ReusableBuffer is a Buffer around a recogniser that keeps its connection
// across utterances (api/v1.UtteranceReusable).
//
// A plain Buffer owns one utterance and closes the recogniser when it ends,
// which is right for a batch endpoint and wrong for a streaming one: the
// served room wrapped every recogniser in a plain Buffer, so each utterance
// reconnected to Deepgram before its first partial - measured at 0.3 to 2.3 s
// for Flux, with the recogniser element blocked for all of it, a later
// utterance's audio piling up behind, and a turn end that could no longer
// arrive before the gate's silence.
//
// It is a separate type rather than a method on Buffer because the runtime
// decides reuse from the methods a provider has, and a batch recogniser
// wrapped in a Buffer must go on being closed.
type ReusableBuffer struct {
	*Buffer
	reusable v1.UtteranceReusable
}

// EndUtterance ends the utterance in the recogniser and readies the buffer for
// the next one. Counters stay cumulative and fold into the accumulator once,
// when the session closes the buffer, so a snapshot never loses an utterance
// between a fold and a reset.
func (buffer *ReusableBuffer) EndUtterance() error {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	err := buffer.reusable.EndUtterance()
	buffer.pending = nil
	buffer.pendingStartSample = 0
	buffer.nextInputIndex, buffer.nextInputSample, buffer.nextProviderIndex = 0, 0, 0
	buffer.haveInput, buffer.finalized = false, false
	buffer.currentChunkSamples = buffer.minimumChunkSamples
	buffer.stats.CurrentChunkSamples = buffer.minimumChunkSamples
	buffer.stats.Finalized = false
	// A failed update left the recogniser's state unknown for that utterance;
	// the recogniser has now discarded it, so the next utterance starts clean.
	// A recogniser that cannot is still refused by its own EndUtterance error.
	if err == nil {
		buffer.terminalErr = nil
	}
	return err
}

// Wrap buffers a recogniser, reusing it across utterances when it supports
// that and owning one utterance when it does not.
func (accumulator *Accumulator) Wrap(config Config) (v1.PerceptionProvider, error) {
	buffer, err := accumulator.New(config)
	if err != nil {
		return nil, err
	}
	reusable, ok := config.Provider.(v1.UtteranceReusable)
	if !ok {
		return buffer, nil
	}
	return &ReusableBuffer{Buffer: buffer, reusable: reusable}, nil
}

var _ v1.UtteranceReusable = (*ReusableBuffer)(nil)
