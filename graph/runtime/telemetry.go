package runtime

import (
	"crypto/sha256"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const maximumObservedActiveRuns = 65_536

// nodeTelemetry projects the immutable reaction contract onto payload-free
// live evidence. Trigger streams can wake an existing run repeatedly (audio is
// the important example), so ActiveRuns counts unique non-empty run identities
// rather than trigger envelopes. Active-run keys are fixed-size digests. Raw
// item identifiers remain only in the in-process view; management redaction
// and trace encoding deliberately omit them.
type nodeTelemetry struct {
	mounted *Mounted
	node    string

	triggers      map[string]struct{}
	interrupts    map[string]struct{}
	outcomes      map[string]struct{}
	active        map[[sha256.Size]byte]struct{}
	hasTriggers   bool
	triggerSeen   atomic.Bool
	firstObserved atomic.Bool
	terminal      bool
	lastOutcomeID string
}

func newNodeTelemetry(mounted *Mounted, node ir.Node) *nodeTelemetry {
	return &nodeTelemetry{
		mounted: mounted, node: node.ID,
		triggers: namesSet(node.Reaction.Triggers), interrupts: namesSet(node.Reaction.Interrupts),
		outcomes: namesSet(node.Reaction.Outcomes), active: make(map[[sha256.Size]byte]struct{}),
		hasTriggers: len(node.Reaction.Triggers) != 0,
	}
}

func namesSet(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func (telemetry *nodeTelemetry) inputObserver(port string) envelopeObserver {
	if telemetry == nil {
		return nil
	}
	_, trigger := telemetry.triggers[port]
	_, interrupt := telemetry.interrupts[port]
	if !trigger && !interrupt {
		return nil
	}
	return func(envelope element.Envelope) {
		telemetry.observeInput(envelope, trigger, interrupt)
	}
}

func (telemetry *nodeTelemetry) outputObserver(port string) envelopeObserver {
	if telemetry == nil {
		return nil
	}
	_, outcome := telemetry.outcomes[port]
	if !telemetry.hasTriggers && !outcome {
		return nil
	}
	return func(envelope element.Envelope) {
		telemetry.observeOutput(envelope, outcome)
	}
}

func (telemetry *nodeTelemetry) observeInput(
	envelope element.Envelope, trigger, interrupt bool,
) {
	if telemetry.mounted == nil {
		return
	}
	now := uint64(0)
	if interrupt {
		now = telemetry.mounted.now()
	}
	telemetry.mounted.liveMu.Lock()
	live, found := telemetry.mounted.nodeLive[telemetry.node]
	if !found {
		telemetry.mounted.liveMu.Unlock()
		return
	}
	changed := false
	if trigger {
		telemetry.triggerSeen.Store(true)
		live.LastTriggerID = envelope.ItemID
		if envelope.RunID != "" {
			run := sha256.Sum256([]byte(envelope.RunID))
			if _, active := telemetry.active[run]; !active && len(telemetry.active) < maximumObservedActiveRuns {
				telemetry.active[run] = struct{}{}
			}
		}
		live.ActiveRuns = len(telemetry.active)
		changed = true
	}
	if interrupt && live.CancellationNS == 0 && now != 0 {
		live.CancellationNS = now
		changed = true
	}
	telemetry.mounted.nodeLive[telemetry.node] = live
	telemetry.mounted.liveMu.Unlock()
	if changed {
		telemetry.mounted.recorder.signal()
	}
}

func (telemetry *nodeTelemetry) observeOutput(envelope element.Envelope, outcome bool) {
	if telemetry.mounted == nil {
		return
	}
	if !outcome && (!telemetry.triggerSeen.Load() || telemetry.firstObserved.Load()) {
		return
	}
	now := telemetry.mounted.now()
	telemetry.mounted.liveMu.Lock()
	live, found := telemetry.mounted.nodeLive[telemetry.node]
	if !found {
		telemetry.mounted.liveMu.Unlock()
		return
	}
	changed := false
	if telemetry.triggerSeen.Load() && live.FirstOutputNS == 0 && now != 0 {
		live.FirstOutputNS = now
		telemetry.firstObserved.Store(true)
		changed = true
	}
	if outcome && (!telemetry.terminal || telemetry.lastOutcomeID != envelope.ItemID) {
		telemetry.terminal = true
		telemetry.lastOutcomeID = envelope.ItemID
		live.LastOutcome = envelope.ItemID
		if envelope.RunID != "" {
			delete(telemetry.active, sha256.Sum256([]byte(envelope.RunID)))
		} else if len(telemetry.active) == 1 {
			for runID := range telemetry.active {
				delete(telemetry.active, runID)
			}
		}
		live.ActiveRuns = len(telemetry.active)
		if telemetry.triggerSeen.Load() && live.CompletionNS == 0 && now != 0 {
			live.CompletionNS = now
		}
		changed = true
	}
	telemetry.mounted.nodeLive[telemetry.node] = live
	telemetry.mounted.liveMu.Unlock()
	if changed {
		telemetry.mounted.recorder.signal()
	}
}
