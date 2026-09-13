package voices

import (
	"context"
	"errors"
	"io"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/perception"
)

// OtherSpeakerSource is the canonical source label for a voice that differs
// from the person who opened the session. It is deliberately human-readable:
// interaction situations and model context render observation sources as
// evidence, not as an opaque classifier enum.
const OtherSpeakerSource = "someone else in the room"

// FinalAttributionWait is the maximum extra latency added at a terminal
// transcript. Partial revisions never wait. Finalization only joins a speaker
// comparison that Hear already started; it never starts new embedding work.
const FinalAttributionWait = 250 * time.Millisecond

// Provider adds per-utterance speaker attribution to an existing ASR provider.
// A single Recogniser is shared by all Provider instances in one session so
// the first utterance can enrol the session's user and later utterances can be
// compared with it.
type Provider struct {
	inner      v1.PerceptionProvider
	recogniser *Recogniser
	session    context.Context
}

// WrapProvider starts one attribution utterance around an exact ASR provider.
func WrapProvider(
	session context.Context, inner v1.PerceptionProvider, recogniser *Recogniser, utterance string,
) (*Provider, error) {
	if session == nil {
		return nil, errors.New("speaker-attributing provider requires a session context")
	}
	if inner == nil {
		return nil, errors.New("speaker-attributing provider requires an ASR provider")
	}
	if recogniser == nil {
		return nil, errors.New("speaker-attributing provider requires a recogniser")
	}
	if utterance == "" {
		return nil, errors.New("speaker-attributing provider requires an utterance identity")
	}
	recogniser.Begin(utterance)
	return &Provider{inner: inner, recogniser: recogniser, session: session}, nil
}

func (provider *Provider) Descriptor() v1.Descriptor { return provider.inner.Descriptor() }

func (provider *Provider) PushFrame(
	ctx context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	provider.recogniser.Hear(provider.session, []perception.Frame{{
		Kind: perception.FrameAudio, Source: "microphone",
		PCM16LE: frame.PCM16LE, SampleRateHz: frame.SampleRateHz,
	}})
	revisions, err := provider.inner.PushFrame(ctx, frame)
	for index := range revisions {
		revisions[index] = provider.attributed(revisions[index])
	}
	return revisions, err
}

func (provider *Provider) Finalize(
	ctx context.Context, sourceSample uint64,
) (v1.PerceptionRevision, error) {
	revision, err := provider.inner.Finalize(ctx, sourceSample)
	if err != nil {
		return revision, err
	}
	wait, cancel := context.WithTimeout(ctx, FinalAttributionWait)
	provider.recogniser.Await(wait)
	cancel()
	revision = provider.attributed(revision)
	return revision, nil
}

func (provider *Provider) attributed(revision v1.PerceptionRevision) v1.PerceptionRevision {
	if provider.recogniser.Verdict() != Different {
		return revision
	}
	revision.Source = OtherSpeakerSource
	return revision
}

// SpeechEndpointed preserves the optional streaming-ASR endpoint contract.
func (provider *Provider) SpeechEndpointed() bool {
	endpointed, ok := provider.inner.(interface{ SpeechEndpointed() bool })
	return ok && endpointed.SpeechEndpointed()
}

// EagerEndOfTurn preserves the optional eager end-of-turn contract.
func (provider *Provider) EagerEndOfTurn() bool {
	eager, ok := provider.inner.(interface{ EagerEndOfTurn() bool })
	return ok && eager.EagerEndOfTurn()
}

// Close releases the wrapped ASR provider when it owns resources.
func (provider *Provider) Close() error {
	if closer, ok := provider.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

var _ v1.PerceptionProvider = (*Provider)(nil)
var _ io.Closer = (*Provider)(nil)
