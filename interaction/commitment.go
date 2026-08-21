package interaction

import (
	"fmt"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// CommitmentDecision says how much of a candidate may be emitted now.
//
// Emit is what crosses the boundary; Hold is what is kept back. A policy that
// emits everything has no wrong-start rate and maximum latency exposure; one
// that emits nothing until certainty has the opposite. This is the axis, and
// it is the reason the decision is a named policy rather than an if statement
// in the speech path.
type CommitmentDecision struct {
	Emit   string `json:"emit,omitempty"`
	Hold   string `json:"hold,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Committed reports whether anything crosses the boundary.
func (decision CommitmentDecision) Committed() bool { return decision.Emit != "" }

// CommitmentInput is one candidate for emission.
type CommitmentInput struct {
	Context
	// Text is the candidate assistant content.
	Text string `json:"text"`
	// Complete marks output that reached its provider's terminal safe point.
	Complete bool `json:"complete"`
	// SpeechAuthority is the producing provider's authority, taken from the
	// committed log rather than from configuration. Silent content never
	// crosses this boundary whatever the policy says.
	SpeechAuthority string `json:"speech_authority,omitempty"`
}

// Commitment decides how much to emit before certainty, and is therefore what
// sets the wrong-start rate.
type Commitment interface {
	Named
	Decide(CommitmentInput) CommitmentDecision
}

type completeCommitment struct{}

// NewCompleteCommitment emits only output that reached a terminal safe point.
// It is the shipped default: a wrong start is audible and a repair is damage
// limitation rather than reversal.
func NewCompleteCommitment() Commitment { return completeCommitment{} }

func (completeCommitment) Name() string { return "complete" }

func (completeCommitment) Decide(input CommitmentInput) CommitmentDecision {
	if silent(input.SpeechAuthority) {
		return CommitmentDecision{Hold: input.Text, Reason: "producer has no voice authority"}
	}
	if !input.Complete {
		return CommitmentDecision{Hold: input.Text, Reason: "output has not reached a safe point"}
	}
	return CommitmentDecision{Emit: input.Text, Reason: "complete at a safe point"}
}

type sentenceCommitment struct {
	minimum int
}

// NewSentenceCommitment emits completed sentences as they appear, holding the
// unfinished tail. It trades a lower first-audio latency for the possibility
// of having said a sentence the rest of the answer contradicts.
func NewSentenceCommitment(minimumCharacters int) Commitment {
	if minimumCharacters <= 0 {
		minimumCharacters = 12
	}
	return sentenceCommitment{minimum: minimumCharacters}
}

func (policy sentenceCommitment) Name() string {
	return fmt.Sprintf("sentence-%d", policy.minimum)
}

func (policy sentenceCommitment) Decide(input CommitmentInput) CommitmentDecision {
	if silent(input.SpeechAuthority) {
		return CommitmentDecision{Hold: input.Text, Reason: "producer has no voice authority"}
	}
	if input.Complete {
		return CommitmentDecision{Emit: input.Text, Reason: "complete at a safe point"}
	}
	boundary := lastSentenceBoundary(input.Text)
	if boundary < policy.minimum {
		return CommitmentDecision{Hold: input.Text, Reason: "no sentence boundary yet"}
	}
	return CommitmentDecision{
		Emit: input.Text[:boundary], Hold: input.Text[boundary:],
		Reason: "completed sentence",
	}
}

func lastSentenceBoundary(text string) int {
	boundary := 0
	for index, symbol := range text {
		switch symbol {
		case '.', '!', '?', '\n', '。', '！', '？':
			boundary = index + len(string(symbol))
		}
	}
	return boundary
}

func silent(authority string) bool {
	return authority == "silent"
}

// RepairAction says what to do about a commitment that later evidence
// invalidated.
type RepairAction string

const (
	// RepairSpeak requires an explicit audible correction. Audio that was
	// heard cannot be unheard, so the only honest response is to say so.
	RepairSpeak RepairAction = "speak"
	// RepairSilent records the obligation without voicing a correction.
	RepairSilent RepairAction = "silent"
	// RepairNone applies when nothing audible was affected.
	RepairNone RepairAction = "none"
)

// RepairInput describes what was invalidated.
type RepairInput struct {
	Context
	// PlayedAudioMS is how much of the invalidated content the user heard.
	// Zero means the commitment was cancelled before anyone heard it, which is
	// not a repair situation at all.
	PlayedAudioMS uint64 `json:"played_audio_ms"`
	// InvalidatedByRevision is the later canonical evidence that invalidated
	// the commitment.
	InvalidatedByRevision uint64 `json:"invalidated_by_revision"`
	// TargetPhase is the phase that produced the invalidated content.
	TargetPhase trajectory.Phase `json:"target_phase,omitempty"`
}

// RepairDecision is what the runtime will do about it.
type RepairDecision struct {
	Action RepairAction `json:"action"`
	Reason string       `json:"reason,omitempty"`
}

// Repair decides what to do when a commitment proves wrong.
type Repair interface {
	Named
	Decide(RepairInput) RepairDecision
}

type audibleRepair struct {
	threshold time.Duration
}

// NewAudibleRepair requires an explicit correction for anything the user
// actually heard.
func NewAudibleRepair() Repair { return audibleRepair{} }

// NewAudibleRepairAfter requires a correction only once more than threshold of
// audio was heard, on the argument that a fragment too short to carry a claim
// cannot have committed the agent to one.
func NewAudibleRepairAfter(threshold time.Duration) Repair {
	if threshold < 0 {
		threshold = 0
	}
	return audibleRepair{threshold: threshold}
}

func (policy audibleRepair) Name() string {
	if policy.threshold <= 0 {
		return "audible"
	}
	return fmt.Sprintf("audible-after-%dms", policy.threshold.Milliseconds())
}

func (policy audibleRepair) Decide(input RepairInput) RepairDecision {
	if input.PlayedAudioMS == 0 {
		return RepairDecision{Action: RepairNone, Reason: "nothing was heard"}
	}
	if input.PlayedAudioMS < uint64(policy.threshold.Milliseconds()) {
		return RepairDecision{Action: RepairSilent, Reason: "heard fragment is below the repair threshold"}
	}
	return RepairDecision{Action: RepairSpeak, Reason: "invalidated content was already heard"}
}

type silentRepair struct{}

// NewSilentRepair records obligations without voicing corrections. It exists
// as a control condition: it is what most systems do, and the comparison is
// the point.
func NewSilentRepair() Repair { return silentRepair{} }

func (silentRepair) Name() string { return "silent" }

func (silentRepair) Decide(input RepairInput) RepairDecision {
	if input.PlayedAudioMS == 0 {
		return RepairDecision{Action: RepairNone, Reason: "nothing was heard"}
	}
	return RepairDecision{Action: RepairSilent, Reason: "audible repair disabled"}
}
