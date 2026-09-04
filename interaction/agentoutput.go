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
