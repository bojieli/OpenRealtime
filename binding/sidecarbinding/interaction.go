package sidecarbinding

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// observeModelFloorInteractionAudio feeds policy perception without creating
// an engine acoustic floor. Native speech_started/stopped frames establish the
// utterance boundary and flush this observer in mirror.go.
func (runtime *runtime) observeModelFloorInteractionAudio(
	ctx context.Context, frame perception.Frame,
) error {
	runtime.audioMu.Lock()
	runtime.interactionPending = append(runtime.interactionPending, frame)
	now := runtime.scheduler.NowNS()
	cadence := uint64(runtime.interactionAudio.Cadence().Nanoseconds())
	due := runtime.interactionLastObserveNS == 0 ||
		now-runtime.interactionLastObserveNS >= cadence
	var batch []perception.Frame
	if due {
		batch, runtime.interactionPending = runtime.interactionPending, nil
		runtime.interactionLastObserveNS = now
	}
	runtime.audioMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	return runtime.observeInteractionAudio(ctx, batch, 0)
}

func (runtime *runtime) nativeSpeechStarted() string {
	runtime.inputMu.Lock()
	defer runtime.inputMu.Unlock()
	now := runtime.scheduler.NowNS()
	runtime.duplex.UserSpeechStarted(now)
	runtime.audioMu.Lock()
	runtime.utteranceID = fmt.Sprintf("%s_item_%d", runtime.spec.Name, runtime.sequence.Add(1))
	utteranceID := runtime.utteranceID
	runtime.audioMu.Unlock()
	return utteranceID
}

func (runtime *runtime) nativeSpeechStopped(ctx context.Context) (string, error) {
	runtime.inputMu.Lock()
	defer runtime.inputMu.Unlock()
	now := runtime.scheduler.NowNS()
	runtime.duplex.UserSpeechStopped(now)
	runtime.audioMu.Lock()
	utteranceID := runtime.utteranceID
	runtime.audioMu.Unlock()
	if runtime.spec.Ownership.Interaction == binding.OwnerEngine {
		if runtime.policies.Interaction != nil && runtime.interactionAudio != nil {
			if err := runtime.finishInteractionTurn(ctx, interaction.ActAnswer, true); err != nil {
				return utteranceID, err
			}
			return utteranceID, nil
		}
		if runtime.config.Sidecar.ProtocolVersion >= sidecar.VersionInteraction {
			if err := runtime.finishInteractionTurn(ctx, interaction.ActAnswer, true); err != nil {
				return utteranceID, err
			}
			return utteranceID, nil
		}
		if runtime.interactionAudio != nil {
			if err := runtime.finishInteractionTurn(ctx, interaction.ActAnswer, false); err != nil {
				return utteranceID, err
			}
		}
		if err := runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond}); err != nil {
			return utteranceID, err
		}
	}
	return utteranceID, nil
}

// observeInteractionAudio advances the policy-only recogniser. Its revisions
// are control-plane evidence: provisional text is exposed and considered, but
// only a final or an act that must happen mid-turn enters the canonical log.
func (runtime *runtime) observeInteractionAudio(
	ctx context.Context, frames []perception.Frame, silenceNS uint64,
) error {
	if runtime.interactionAudio == nil {
		return nil
	}
	observations, err := runtime.interactionAudio.Observe(ctx, frames)
	if err != nil {
		return err
	}
	for _, observation := range observations {
		revision := interaction.Revision{
			ID: observation.Revision, StableText: observation.StableText,
			UnstableText: strings.TrimPrefix(observation.Text, observation.StableText),
			ObservedNS:   runtime.scheduler.NowNS(), SilenceNS: silenceNS,
		}
		runtime.audioMu.Lock()
		runtime.interactionHeard = revision
		utteranceID := runtime.utteranceID
		runtime.audioMu.Unlock()
		if err := runtime.sink.Transcript(ctx, binding.TranscriptEvent{
			ItemID: utteranceID, Text: observation.Text,
		}); err != nil {
			return err
		}
		runtime.considerInteraction(revision)
	}
	return nil
}

// finishInteractionTurn finalises policy perception, chooses the endpoint act
// when requested, and records the utterance with that act. fallback is used
// when no decision is requested or when the policy cannot answer.
func (runtime *runtime) finishInteractionTurn(
	ctx context.Context, fallback interaction.Act, decide bool,
) error {
	if fallback == "" {
		fallback = interaction.ActAnswer
	}
	// A model-owned floor may close between policy-ASR cadences. Frames which
	// have not reached the recogniser yet must be advanced before Finalize, or
	// the controller decides from a clipped utterance even though the voice
	// model received all of it.
	if runtime.interactionAudio != nil {
		runtime.audioMu.Lock()
		pending := runtime.interactionPending
		runtime.interactionPending = nil
		runtime.audioMu.Unlock()
		if len(pending) > 0 {
			if err := runtime.observeInteractionAudio(ctx, pending, 0); err != nil {
				runtime.sink.Failed(ctx, binding.ErrorEvent{
					Code: "interaction_asr_error", Message: err.Error(),
				})
			}
		}
	}
	latest, text := runtime.latestInteractionEvidence()
	if runtime.interactionAudio != nil {
		observations, err := runtime.interactionAudio.Flush(ctx)
		if err != nil {
			runtime.sink.Failed(ctx, binding.ErrorEvent{
				Code: "interaction_asr_error", Message: err.Error(),
			})
			runtime.interactionAudio.Reset()
		} else {
			for _, observation := range observations {
				text = observation.Text
				latest = interaction.Revision{
					ID: observation.Revision, StableText: observation.StableText,
					UnstableText: strings.TrimPrefix(observation.Text, observation.StableText),
					Final:        true, ObservedNS: runtime.scheduler.NowNS(),
				}
			}
		}
		runtime.interactionAudio.Reset()
		runtime.audioMu.Lock()
		runtime.interactionPending = nil
		runtime.interactionLastObserveNS = 0
		runtime.interactionHeard = interaction.Revision{}
		runtime.audioMu.Unlock()
	}

	plan := runtime.fallbackPlan(fallback, latest)
	if decide && runtime.policies.Interaction != nil && !latest.Empty() {
		chosen, err := runtime.decideInteraction(ctx, latest)
		if err == nil {
			plan = chosen
		} else {
			runtime.debugInteraction("error", plan, latest, err)
		}
	}
	act := plan.Act
	if strings.TrimSpace(text) != "" {
		if _, err := runtime.commitUserSpeechAs(text, "interaction-asr", &act); err != nil {
			return err
		}
	}
	// A turn-scoped instruction governed the decision above and expires after
	// it. Extraction runs afterwards, so a new instruction in this utterance
	// survives to govern what follows.
	runtime.interactionPinboard.EndTurn()
	if strings.TrimSpace(text) != "" {
		runtime.extractInteractionPolicy(text)
	}
	if !decide {
		return nil
	}
	return runtime.applyInteractionPlan(plan, latest, true)
}

func (runtime *runtime) latestInteractionEvidence() (interaction.Revision, string) {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	return runtime.interactionHeard, runtime.interactionHeard.Text()
}

// considerInteraction handles acts that matter before the endpoint: yielding
// an agent turn, speaking through when concurrent I/O exists, interrupting,
// or acting silently. One decision at a time keeps a slower controller from
// building a queue of answers to moments that have already passed.
func (runtime *runtime) considerInteraction(revision interaction.Revision) {
	if runtime.policies.Interaction == nil || revision.Empty() ||
		!runtime.duplex.Snapshot().UserSpeaking ||
		!runtime.interactionInFlight.CompareAndSwap(false, true) {
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.interactionInFlight.Store(false)
		plan, err := runtime.decideInteraction(runtime.ctx, revision)
		if err != nil || runtime.ctx.Err() != nil {
			if err != nil {
				runtime.debugInteraction("error", runtime.fallbackPlan(
					interaction.ActStaySilent, revision), revision, err)
			}
			return
		}
		current, _ := runtime.latestInteractionEvidence()
		if current.Empty() || current.ID != revision.ID &&
			!strings.HasPrefix(current.Text(), revision.Text()) {
			return
		}
		if err := runtime.applyInteractionPlan(plan, revision, false); err != nil {
			runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
				Code: "interaction_act_error", Message: err.Error(),
			})
		}
	}()
}

func (runtime *runtime) decideInteraction(
	parent context.Context, revision interaction.Revision,
) (interaction.Plan, error) {
	runtime.interactionMu.Lock()
	defer runtime.interactionMu.Unlock()
	state := runtime.interactionSituation(revision)
	deadline := time.Now().Add(runtime.config.InteractionTimeout)
	ctx, cancel := context.WithTimeout(parent, runtime.config.InteractionTimeout)
	defer cancel()
	act, outcome, err := runtime.policies.Interaction.Decide(ctx, state)
	if err != nil {
		return interaction.Plan{}, err
	}
	plan, err := interaction.NewPlan(
		act, runtime.policies.Interaction.Name(), runtime.interactionEvidenceRef(revision),
		deadline, outcome,
	)
	if err == nil {
		runtime.debugInteraction("decision", plan, revision, nil)
	}
	return plan, err
}

func (runtime *runtime) interactionSituation(revision interaction.Revision) interaction.Situation {
	snapshot := runtime.store.Snapshot()
	duplex := runtime.duplex.Snapshot()
	runtime.stateMu.Lock()
	agentSaying := runtime.spokenText
	runtime.stateMu.Unlock()
	capabilities := runtime.stackCapabilities()
	allowed := []interaction.Act{
		interaction.ActStaySilent, interaction.ActAnswer, interaction.ActInterrupt,
		interaction.ActActSilently, interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
	}
	if capabilities.ConcurrentIO {
		allowed = append(allowed, interaction.ActSpeakThrough)
	}
	state := interaction.Situation{
		Contract:      runtime.Settings().Instruction,
		Pins:          runtime.interactionPinboard.Lines(runtime.scheduler.NowNS()),
		Recent:        runtime.interactionWindow.Lines(snapshot.Items),
		AgentSpeaking: duplex.AgentSpeaking, AgentSaying: agentSaying,
		Speaker: "user", Speaking: duplex.UserSpeaking, Heard: strings.TrimSpace(revision.Text()),
		HeardSince: strings.TrimSpace(revision.Text()),
		Silence:    fmt.Sprintf("%dms", time.Duration(revision.SilenceNS).Milliseconds()),
		InFlight:   interactionWorkInFlight(snapshot), Tools: runtime.interactionToolLines(),
		AllowedActs: allowed,
	}
	return state
}

func interactionWorkInFlight(snapshot trajectory.Snapshot) string {
	pending := trajectory.UnresolvedToolCalls(snapshot)
	if len(pending) == 0 {
		return ""
	}
	names := make([]string, 0, len(pending))
	for _, call := range pending {
		names = append(names, call.Call.Name+" awaiting its result")
	}
	return strings.Join(names, ", ")
}

func (runtime *runtime) interactionToolLines() []string {
	specs := runtime.registry.Specs()
	lines := make([]string, 0, len(specs))
	for _, spec := range specs {
		line := spec.Name
		if strings.TrimSpace(spec.Description) != "" {
			line += " - " + spec.Description
		}
		lines = append(lines, line)
	}
	return lines
}

func (runtime *runtime) interactionEvidenceRef(revision interaction.Revision) string {
	runtime.audioMu.Lock()
	utteranceID := runtime.utteranceID
	runtime.audioMu.Unlock()
	return fmt.Sprintf("%s:revision:%d", utteranceID, revision.ID)
}

func (runtime *runtime) fallbackPlan(act interaction.Act, revision interaction.Revision) interaction.Plan {
	plan, _ := interaction.NewPlan(
		act, "runtime-inertia", runtime.interactionEvidenceRef(revision),
		time.Now().Add(runtime.config.InteractionTimeout), interaction.Outcome{Option: string(act)},
	)
	return plan
}

func (runtime *runtime) applyInteractionPlan(
	plan interaction.Plan, revision interaction.Revision, final bool,
) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if !final && (plan.Act == interaction.ActStaySilent || plan.Act == interaction.ActKeepSpeaking ||
		plan.Act == interaction.ActAnswer) {
		return nil
	}
	if plan.Act != interaction.ActStaySilent && plan.Act != interaction.ActKeepSpeaking {
		runtime.audioMu.Lock()
		utteranceID := runtime.utteranceID
		runtime.audioMu.Unlock()
		runtime.interactionMu.Lock()
		repeated := runtime.lastPlanUtterance == utteranceID &&
			runtime.lastPlanAct == plan.Act && runtime.lastPlanHeard != "" &&
			strings.HasPrefix(revision.Text(), runtime.lastPlanHeard)
		if !repeated {
			runtime.lastPlanAct, runtime.lastPlanHeard = plan.Act, revision.Text()
			runtime.lastPlanUtterance = utteranceID
		}
		runtime.interactionMu.Unlock()
		if repeated {
			return nil
		}
	}
	if plan.Act == interaction.ActActSilently && !final {
		act := plan.Act
		if _, err := runtime.commitUserSpeechAs(revision.Text(), "interaction-asr", &act); err != nil {
			return err
		}
	}
	return runtime.sendInteractionPlan(plan)
}

func (runtime *runtime) sendInteractionPlan(plan interaction.Plan) error {
	if runtime.ready.Has(sidecar.CapabilityInteractionActs) {
		return runtime.model.Send(sidecar.Message{
			Type: sidecar.TypeInteractionAct, Act: string(plan.Act), Policy: plan.Policy,
			EvidenceRef: plan.EvidenceRef, Floor: string(plan.Floor),
			DeadlineMS: plan.Deadline.UnixMilli(), Confidence: plan.Confidence,
			Abstained: plan.Abstained,
		})
	}
	// Protocol-v1 translation preserves behavior but not the typed evidence.
	// It is retained only for existing sidecars; v2 is what benchmark cells
	// should use when the interaction seam itself is under study.
	switch plan.Act {
	case interaction.ActAnswer, interaction.ActInterrupt, interaction.ActSpeakThrough:
		return runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond})
	case interaction.ActStopSpeaking:
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
		return runtime.model.Send(sidecar.Message{Type: sidecar.TypeInterrupt})
	case interaction.ActStaySilent, interaction.ActKeepSpeaking, interaction.ActActSilently:
		return nil
	default:
		return fmt.Errorf("cannot execute unknown interaction act %q", plan.Act)
	}
}

func (runtime *runtime) debugInteraction(
	phase string, plan interaction.Plan, revision interaction.Revision, failure error,
) {
	sink, ok := runtime.sink.(binding.DebugSink)
	if !ok {
		return
	}
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	_ = sink.Debug(runtime.ctx, binding.DebugEvent{
		Category: "policy", Name: "policy.interaction", Phase: phase,
		CorrelationID: plan.EvidenceRef, Message: message,
		Attributes: map[string]any{
			"act": plan.Act, "floor": plan.Floor, "policy": plan.Policy,
			"confidence": plan.Confidence, "final": revision.Final,
		},
	})
}

// extractInteractionPolicy keeps spoken policies outside the rolling window.
// It runs off the audio path: the policy governs what happens next, not the
// endpoint decision that has already been taken.
func (runtime *runtime) extractInteractionPolicy(text string) {
	if runtime.policies.Extraction == nil || strings.TrimSpace(text) == "" {
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		snapshot := runtime.store.Snapshot()
		extraction, err := runtime.policies.Extraction.Extract(
			runtime.ctx, runtime.interactionPinboard.InForce(),
			interaction.RecentLines(snapshot.Items, 6), text,
		)
		if err != nil || runtime.ctx.Err() != nil {
			return
		}
		for _, revoked := range extraction.Revokes {
			runtime.interactionPinboard.Revoke(revoked)
		}
		now := runtime.scheduler.NowNS()
		for _, instruction := range extraction.Pins {
			instruction.SetNS = now
			runtime.interactionPinboard.Pin(instruction)
		}
	}()
}
