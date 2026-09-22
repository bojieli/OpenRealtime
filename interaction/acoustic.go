package interaction

import (
	"errors"
	"fmt"
	"time"
)

// AcousticEndpoint is acoustic evidence about whether a pause ends the user's
// turn: a classifier's probability, computed from the recent user audio
// (prosody, final intonation, a breath before more words), not from words.
//
// It is evidence, not a decision. The runtime attaches it to the pause it
// describes and records it on the timeline whether or not anything acts on
// it; only an explicitly selected policy such as AcousticProjection turns it
// into an endpoint. Keeping the two apart is what lets a predictor run in
// observation-only mode first and be promoted later on measured behaviour.
type AcousticEndpoint struct {
	// Probability is P(turn complete) in [0, 1].
	Probability float64 `json:"probability"`
	// Model identifies the classifier that produced it.
	Model string `json:"model,omitempty"`
	// Latency is how long the evidence took to compute.
	Latency time.Duration `json:"latency,omitempty"`
	// WindowMS is how much audio, ending at the pause, the classifier read.
	WindowMS float64 `json:"window_ms,omitempty"`
}

// AcousticProjection is a turn projection that reads acoustic end-of-turn
// evidence at a pause: at or above the threshold the pause ends the turn now,
// below it the floor holds the turn open - but only up to the floor's own
// projection-hold bound, after which silence ends it regardless.
//
// It has no opinion without evidence, so a classifier that failed or was not
// consulted (a revision arriving mid-speech) leaves the ordinary silence rule
// in charge rather than holding or ending on nothing.
type AcousticProjection struct {
	threshold float64
}

// NewAcousticProjection validates the threshold.
func NewAcousticProjection(threshold float64) (AcousticProjection, error) {
	if !(threshold > 0 && threshold < 1) {
		return AcousticProjection{}, errors.New("acoustic end-of-turn threshold must be strictly between 0 and 1")
	}
	return AcousticProjection{threshold: threshold}, nil
}

// Name implements Named.
func (projection AcousticProjection) Name() string {
	return fmt.Sprintf("acoustic-endpoint@%.2f", projection.threshold)
}

// Project implements TurnProjection.
func (projection AcousticProjection) Project(context Context) Projection {
	evidence := context.AcousticEndpoint
	if evidence == nil {
		return Projection{Reason: "no acoustic end-of-turn evidence for this decision"}
	}
	if evidence.Probability >= projection.threshold {
		return Projection{
			Ending: true, Confidence: evidence.Probability,
			Reason: fmt.Sprintf("%s P(complete)=%.2f", evidence.Model, evidence.Probability),
		}
	}
	return Projection{
		Continuing: true, Confidence: 1 - evidence.Probability,
		Reason: fmt.Sprintf("%s P(complete)=%.2f", evidence.Model, evidence.Probability),
	}
}

var _ TurnProjection = AcousticProjection{}
