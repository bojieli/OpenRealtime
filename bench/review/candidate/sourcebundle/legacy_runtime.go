package sourcebundle

import (
	"bytes"
	"encoding/json"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
)

// legacyRuntimeStatus freezes the status encoding used before 997a4e0.
// That revision made graph-native status sparse, including omitting empty
// observers. Old sealed completions and review contexts still contain those
// fields. Verification recognizes this exact former encoding without changing
// the live wire format, rewriting evidence, or accepting arbitrary JSON shapes.
type legacyRuntimeStatus struct {
	Architecture            binding.ArchitectureIdentity `json:"architecture,omitempty"`
	Graph                   binding.ArchitectureIdentity `json:"graph,omitempty"`
	Binding                 string                       `json:"binding"`
	Profile                 string                       `json:"profile,omitempty"`
	Ownership               binding.Ownership            `json:"ownership"`
	Stack                   binding.StackCapabilities    `json:"stack"`
	Policies                interaction.Report           `json:"policies"`
	Interaction             binding.InteractionStatus    `json:"interaction"`
	Tools                   binding.ToolStatus           `json:"tools"`
	Observers               []string                     `json:"observers"`
	Fast                    string                       `json:"fast,omitempty"`
	Reflex                  string                       `json:"reflex,omitempty"`
	Slow                    string                       `json:"slow,omitempty"`
	Perception              string                       `json:"perception,omitempty"`
	PerceptionRevision      string                       `json:"perception_revision,omitempty"`
	SpeakerIdentity         string                       `json:"speaker_identity,omitempty"`
	SpeakerIdentityRevision string                       `json:"speaker_identity_revision,omitempty"`
	VisualNarrator          string                       `json:"visual_narrator,omitempty"`
	Speech                  string                       `json:"speech,omitempty"`
	SpeechRevision          string                       `json:"speech_revision,omitempty"`
}

type legacyRuntimeTranscript struct {
	Moments              []bench.Moment           `json:"moments"`
	PlaybackMS           float64                  `json:"playback_ms"`
	Failure              string                   `json:"failure,omitempty"`
	NegotiatedObservers  []string                 `json:"negotiated_observers,omitempty"`
	Runtime              *legacyRuntimeStatus     `json:"runtime,omitempty"`
	Execution            *bench.ExecutionEvidence `json:"execution_evidence,omitempty"`
	ExecutionError       string                   `json:"execution_evidence_error,omitempty"`
	OutstandingResponses int                      `json:"outstanding_responses,omitempty"`
	OutstandingTools     int                      `json:"outstanding_tools,omitempty"`
}

func legacyTranscript(transcript bench.Transcript) legacyRuntimeTranscript {
	return legacyRuntimeTranscript{
		Moments: transcript.Moments, PlaybackMS: transcript.PlaybackMS, Failure: transcript.Failure,
		NegotiatedObservers: transcript.NegotiatedObservers,
		Runtime:             (*legacyRuntimeStatus)(transcript.Runtime), Execution: transcript.Execution,
		ExecutionError: transcript.ExecutionError, OutstandingResponses: transcript.OutstandingResponses,
		OutstandingTools: transcript.OutstandingTools,
	}
}

// matchesLegacyRuntimeJSON is called only after bounded strict decoding into
// the current typed schema. It allows exactly the old status representation
// in the two archive records that embedded it. Other fields, key order,
// whitespace, and explicit-empty choices must still match their encoder.
// Receipt and manifest checks independently bind the original file bytes.
func matchesLegacyRuntimeJSON(payload []byte, destination any, indented bool) bool {
	var legacy any
	switch value := destination.(type) {
	case *candidate.Completion:
		if value.Transcript.Runtime == nil {
			return false
		}
		legacy = struct {
			Attempt    candidate.Attempt       `json:"attempt"`
			Outcome    bench.TaskOutcome       `json:"outcome"`
			Transcript legacyRuntimeTranscript `json:"transcript"`
		}{value.Attempt, value.Outcome, legacyTranscript(value.Transcript)}
	case *reviewContext:
		if value.Transcript.Runtime == nil {
			return false
		}
		legacy = struct {
			Attempt                 candidate.Attempt       `json:"attempt"`
			DeterministicOutcome    bench.TaskOutcome       `json:"deterministic_outcome"`
			Transcript              legacyRuntimeTranscript `json:"transcript"`
			DeterministicAuthority  string                  `json:"deterministic_authority"`
			AdvisoryReviewAuthority string                  `json:"advisory_review_authority"`
		}{value.Attempt, value.DeterministicOutcome, legacyTranscript(value.Transcript),
			value.DeterministicAuthority, value.AdvisoryReviewAuthority}
	default:
		return false
	}
	var canonical []byte
	var err error
	if indented {
		canonical, err = json.MarshalIndent(legacy, "", "  ")
		canonical = append(canonical, '\n')
	} else {
		canonical, err = json.Marshal(legacy)
	}
	return err == nil && bytes.Equal(payload, canonical)
}
