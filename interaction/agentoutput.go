package interaction

import "github.com/bojieli/OpenRealtime/element"

var agentOutputType = element.State(element.Named("interaction.AgentOutput"))

// AgentOutput is the provider-neutral voice-output lifecycle visible to an
// interaction policy. Active begins when a voice generation is admitted and
// ends only when cognition, segmentation, synthesis, and playback have all
// reached their typed terminals. Queued and Audible refine that interval
// without making a policy infer lifecycle from timing.
type AgentOutput struct {
	Revision uint64 `json:"revision"`
	Active   bool   `json:"active"`
	Queued   bool   `json:"queued"`
	Audible  bool   `json:"audible"`
	Saying   string `json:"saying,omitempty"`
	InFlight string `json:"in_flight,omitempty"`
	// Generating counts voice generations admitted and not yet answered by
	// the model. GenerationsStarted and GenerationsFinished count, over the
	// session, the generations that reached the model and the ones it has
	// answered (or that were cancelled or refused). A policy that runs in
	// lockstep - decide, let the model answer, decide again - waits for the
	// generation its last choice admitted, and it needs counts that survive
	// coalescing: this is a sampled state lane, a consumer can miss every
	// intermediate snapshot, and "is anything generating right now" read
	// from the latest one cannot say whether the answer has come.
	Generating          int    `json:"generating,omitempty"`
	GenerationsStarted  uint64 `json:"generations_started,omitempty"`
	GenerationsFinished uint64 `json:"generations_finished,omitempty"`
	// Spoken and Pending split Saying at the audio that has actually reached
	// the user. They are empty when nothing established a boundary, which is
	// different from a boundary at the start: a policy must be able to tell
	// "nothing has been heard" from "nobody measured".
	Spoken  string `json:"spoken,omitempty"`
	Pending string `json:"pending,omitempty"`
	// ProtectedStreams are the transcript streams from which active output was
	// deliberately authorized to interrupt or speak through. The sorted,
	// duplicate-free identities let a later transcript revision say that it is
	// from the same stream without asking an interaction model to infer graph
	// provenance from repeated prose.
	ProtectedStreams []string `json:"protected_streams,omitempty"`
}

// AgentOutputType is shared by the lifecycle controller that produces the
// snapshot and policy elements that sample it. Keeping the protocol type in
// the provider-neutral interaction package avoids either element depending on
// the other's implementation package.
func AgentOutputType() element.Type { return agentOutputType.Clone() }

func (AgentOutput) InspectionCause() element.InspectionCauseKind {
	return element.CauseStateRevision
}

var _ element.InspectionCauseProvider = AgentOutput{}
