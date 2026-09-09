package scenarioconversation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func (session *session) publishActivity(ctx context.Context, envelope element.Envelope) error {
	activity, ok := speechActivityPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation acoustic activity has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || activity.Source != SourceMicrophone ||
		!canonicalIdentity(activity.StreamID) || activity.SampleRateHz == 0 {
		return errors.New("scenario conversation acoustic activity drifted from its exact session or source")
	}
	session.activityMu.Lock()
	_, active := session.utterances[activity.StreamID]
	switch activity.Kind {
	case acousticelements.SpeechStarted:
		session.audioMu.Lock()
		expectedStream := session.audioStreamID(session.audioStream)
		_, closed := session.closedAudio[activity.StreamID]
		session.audioMu.Unlock()
		// Activity and close outcomes are separate graph output lanes. The
		// close may advance the adapter's stream revision before any number of
		// earlier, causally ordered activity events are scheduled. Admission
		// records exactly those closed stream identities until their FIFO
		// activity lane publishes the matching stop.
		if activity.StreamID != expectedStream && !closed {
			session.activityMu.Unlock()
			return fmt.Errorf(
				"scenario conversation acoustic stream %q is neither current %q nor an exact pending closed stream",
				activity.StreamID, expectedStream,
			)
		}
		if active {
			session.activityMu.Unlock()
			return fmt.Errorf("scenario conversation acoustic stream %q started twice", activity.StreamID)
		}
		session.utterances[activity.StreamID] = struct{}{}
	case acousticelements.SpeechStopped:
		if !active {
			session.activityMu.Unlock()
			return fmt.Errorf("scenario conversation acoustic stream %q stopped without starting", activity.StreamID)
		}
		delete(session.utterances, activity.StreamID)
		session.audioMu.Lock()
		delete(session.closedAudio, activity.StreamID)
		session.audioMu.Unlock()
	default:
		session.activityMu.Unlock()
		return fmt.Errorf("scenario conversation acoustic activity has unsupported kind %q", activity.Kind)
	}
	session.activityMu.Unlock()
	event := legacy.ActivityEvent{
		ItemID: activity.StreamID, AudioStartMS: activity.AudioStartMS, AudioEndMS: activity.AudioEndMS,
		Started: activity.Kind == acousticelements.SpeechStarted,
		Stopped: activity.Kind == acousticelements.SpeechStopped,
	}
	if err := session.sink.Activity(ctx, event); err != nil {
		return fmt.Errorf("publish scenario conversation acoustic activity: %w", err)
	}
	return nil
}

func (session *session) acceptSegmentationOutcome(envelope element.Envelope) error {
	outcome, ok := segmentationOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation segmentation outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(outcome.RunID) ||
		envelope.RunID != outcome.RunID || outcome.Segments < 0 ||
		outcome.Segments > maximumAdapterMemory {
		return errors.New("scenario conversation segmentation outcome drifted from its exact session, run, or bound")
	}
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	switch outcome.Kind {
	case interactionelements.OutcomeCompleted:
		session.rememberSegmentedRunLocked(outcome.RunID)
		delete(session.pendingSpeech, outcome.RunID)
		if _, duplicate := session.speechRuns[outcome.RunID]; duplicate {
			return fmt.Errorf("scenario conversation run %q completed segmentation twice", outcome.RunID)
		}
		terminal := 0
		for _, state := range session.playback {
			if state.runID == outcome.RunID && state.terminal {
				terminal++
			}
		}
		if terminal > outcome.Segments {
			return fmt.Errorf("scenario conversation run %q has %d terminal utterances for %d segments",
				outcome.RunID, terminal, outcome.Segments)
		}
		remaining := outcome.Segments - terminal
		if remaining == 0 {
			session.removeSpeechRunLocked(outcome.RunID)
			return nil
		}
		return session.rememberSpeechRunLocked(outcome.RunID, remaining)
	case interactionelements.OutcomeCanceled, interactionelements.OutcomeFailed,
		interactionelements.OutcomeRefused:
		session.rememberSegmentedRunLocked(outcome.RunID)
		delete(session.pendingSpeech, outcome.RunID)
		session.removeSpeechRunLocked(outcome.RunID)
		return nil
	case interactionelements.OutcomeIgnored:
		return nil
	default:
		return fmt.Errorf("scenario conversation segmentation outcome has unsupported kind %q", outcome.Kind)
	}
}

// acceptModelResult closes the conservative foreground speech horizon for a
// result that contains no assistant text. A foreground invocation is tracked
// from admission until either this proof of no speech or a terminal
// segmentation outcome arrives. That bridges independently drained model and
// segmentation lanes without retaining tool-only runs forever.
func (session *session) acceptModelResult(envelope element.Envelope) error {
	result, ok := scenarioCognitionResultPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation model result has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(result.RunID) ||
		envelope.RunID != result.RunID {
		return errors.New("scenario conversation model result drifted from its exact session or run")
	}
	if result.ProviderReference != ModelReference && result.ProviderReference != SilentModelReference {
		return fmt.Errorf("scenario conversation model result has provider %q", result.ProviderReference)
	}
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	if result.ProviderReference == SilentModelReference || result.AssistantText == "" {
		// Silent cognition is never connected to segmentation. An empty
		// foreground result also proves there is no speech pipeline to revoke.
		session.rememberSpeechlessRunLocked(result.RunID)
		delete(session.pendingSpeech, result.RunID)
		return nil
	}
	if _, segmented := session.segmentedRuns[result.RunID]; segmented {
		return nil
	}
	return session.rememberPendingSpeechLocked(result.RunID)
}

func (session *session) acceptPlaybackReceipt(
	ctx context.Context, boundary string, envelope element.Envelope,
) error {
	receipt, ok := playbackReceiptPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation playback receipt %s has payload %T", boundary, envelope.Payload)
	}
	expected, ok := playbackReceiptKindForBoundary(boundary)
	if !ok || receipt.Kind != expected {
		return fmt.Errorf("scenario conversation playback boundary %s carried kind %q", boundary, receipt.Kind)
	}
	if err := validatePlaybackReceipt(session.sessionID, envelope, receipt); err != nil {
		return fmt.Errorf("scenario conversation playback receipt %s: %w", boundary, err)
	}
	active := activePlaybackReceiptKind(receipt.Kind)
	terminal := terminalPlaybackReceiptKind(receipt.Kind)
	utterance := receipt.Utterance
	utterance.AssistantItemIDs = slices.Clone(receipt.Utterance.AssistantItemIDs)

	session.activityMu.Lock()
	if session.playback == nil {
		session.playback = make(map[string]playbackReceiptState)
	}
	previous, found := session.playback[utterance.ID]
	if found {
		if previous.runID != envelope.RunID || !samePlaybackUtterance(previous.utterance, utterance) {
			session.activityMu.Unlock()
			return fmt.Errorf("utterance %q changed its exact run or presentation contract", utterance.ID)
		}
		if receipt.Sequence < previous.sequence {
			// The six receipt boundaries drain concurrently. A later effect can be
			// observed first; its higher sequence is authoritative.
			session.activityMu.Unlock()
			return nil
		}
		if receipt.Sequence == previous.sequence {
			session.activityMu.Unlock()
			return fmt.Errorf("utterance %q repeated playback receipt sequence %d",
				utterance.ID, receipt.Sequence)
		}
		if previous.terminal && active {
			session.activityMu.Unlock()
			return fmt.Errorf("utterance %q became active after terminal playback", utterance.ID)
		}
	} else if err := session.reservePlaybackStateLocked(); err != nil {
		session.activityMu.Unlock()
		return err
	} else {
		session.playbackOrder = append(session.playbackOrder, utterance.ID)
	}
	state := playbackReceiptState{
		runID: envelope.RunID, utterance: utterance, sequence: receipt.Sequence,
		sourceSequence: envelope.Sequence, kind: receipt.Kind, outcome: receipt.Outcome,
		active: active, terminal: terminal || found && previous.terminal,
	}
	becameTerminal := state.terminal && (!found || !previous.terminal)
	session.playback[utterance.ID] = state
	if becameTerminal {
		session.completeSpeechRunUtteranceLocked(envelope.RunID)
	}
	session.activityMu.Unlock()
	if receipt.Kind == speechelements.PlaybackReleased {
		if session.bundle == nil || session.bundle.playback == nil || session.bundle.store == nil {
			return errors.New("scenario conversation playback release has no sink or trajectory barrier")
		}
		if err := session.recordPlaybackBoundary(ctx, envelope.RunID); err != nil {
			return fmt.Errorf("record scenario conversation playback boundary: %w", err)
		}
		if err := session.bundle.playback.Release(ctx, envelope.RunID, receipt); err != nil {
			return fmt.Errorf("complete scenario conversation playback release: %w", err)
		}
	}
	return nil
}

// recordPlaybackBoundary commits what the user had actually heard before the
// release exposes TurnEnd. Model-result commit and playback drain on separate
// graph lanes, so this waits for the canonical assistant items rather than
// guessing their identities from streamed sentence text.
func (session *session) recordPlaybackBoundary(ctx context.Context, runID string) error {
	if ctx == nil {
		return errors.New("playback boundary has nil context")
	}
	assistant, snapshot, err := session.awaitAssistantRun(ctx, runID)
	if err != nil {
		return err
	}

	session.activityMu.Lock()
	releases := make([]playbackReceiptState, 0, len(session.playback))
	order := make(map[string]int, len(session.playbackOrder))
	for index, utteranceID := range session.playbackOrder {
		order[utteranceID] = index
	}
	for _, state := range session.playback {
		if state.runID == runID && state.kind == speechelements.PlaybackReleased {
			releases = append(releases, state)
		}
	}
	session.activityMu.Unlock()
	if len(releases) == 0 {
		return fmt.Errorf("run %q has no released playback receipt", runID)
	}
	sort.SliceStable(releases, func(left, right int) bool {
		leftSequence, rightSequence := releases[left].sourceSequence, releases[right].sourceSequence
		if leftSequence != 0 && rightSequence != 0 && leftSequence != rightSequence {
			return leftSequence < rightSequence
		}
		return order[releases[left].utterance.ID] < order[releases[right].utterance.ID]
	})

	contents := make([]string, len(assistant))
	fullWords := make([]string, 0)
	for index, item := range assistant {
		contents[index] = item.Content
		fullWords = append(fullWords, spoken.Words(item.Content)...)
	}
	heardWords := 0
	cut := false
	measured := true
	playedMS := uint64(0)
	boundaryReached := false
	for _, release := range releases {
		mark := release.outcome.Mark
		segmentWords := spoken.Words(release.utterance.Text)
		segmentHeard := len(spoken.Words(mark.Spoken))
		if segmentHeard > len(segmentWords) {
			return fmt.Errorf("utterance %q reports %d heard words for %d generated words",
				release.utterance.ID, segmentHeard, len(segmentWords))
		}
		if release.outcome.PlayedMS > ^uint64(0)-playedMS {
			return errors.New("playback boundary duration overflow")
		}
		playedMS += release.outcome.PlayedMS
		heardWords += segmentHeard
		measured = measured && mark.Measured
		if !mark.Complete() {
			cut = strings.TrimSpace(mark.Cut) != ""
			boundaryReached = true
			break
		}
	}
	if heardWords > len(fullWords) {
		return fmt.Errorf("run %q reports %d heard words for %d committed words",
			runID, heardWords, len(fullWords))
	}
	// A completed prefix of sentence releases still leaves every not-yet-
	// released sentence pending. The complete model result is already durable,
	// so the aggregate split can state that fact exactly.
	if !boundaryReached && heardWords < len(fullWords) {
		boundaryReached = true
	}
	aggregate := spoken.Mark{
		Spoken:   strings.Join(fullWords[:heardWords], " "),
		Pending:  strings.Join(fullWords[heardWords:], " "),
		Measured: measured, PlayedMS: playedMS,
	}
	if cut && heardWords < len(fullWords) {
		aggregate.Cut = fullWords[heardWords]
	}
	if !boundaryReached {
		aggregate.Pending = ""
	}
	distributed := spoken.Distribute(aggregate, contents)
	visibility := trajectory.VisibilityPlayed
	if playedMS == 0 && !aggregate.Started() {
		visibility = trajectory.VisibilityCancelled
	}
	monotonicNS := uint64(0)
	if len(snapshot.Items) > 0 {
		monotonicNS = snapshot.Items[len(snapshot.Items)-1].MonotonicNS
	}
	items := make([]trajectory.Item, len(assistant))
	for index, source := range assistant {
		mark := distributed[index]
		items[index] = trajectory.Item{
			ID: session.nextItemID("playback-boundary"), Kind: trajectory.KindAssistantState,
			MonotonicNS: monotonicNS, CausalParentIDs: []string{source.ID},
			SourceRevision: source.SourceRevision, InvocationID: runID,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: source.ID, Visibility: visibility,
				PlayedAudioMS: playedMS, Heard: &mark,
			},
		}
	}
	return session.commitPlaybackState(ctx, runID, snapshot, items)
}

func (session *session) awaitAssistantRun(
	ctx context.Context, runID string,
) ([]trajectory.Item, trajectory.Snapshot, error) {
	for {
		session.contentMu.Lock()
		changed := session.snapshotChanged
		session.contentMu.Unlock()
		snapshot := session.bundle.store.Snapshot()
		assistant := make([]trajectory.Item, 0)
		for _, item := range snapshot.Items {
			if item.Kind == trajectory.KindAssistant && item.InvocationID == runID {
				assistant = append(assistant, item)
			}
		}
		if len(assistant) > 0 {
			return assistant, snapshot, nil
		}
		if changed == nil {
			return nil, trajectory.Snapshot{}, fmt.Errorf(
				"run %q has no assistant items and no trajectory publication barrier", runID)
		}
		select {
		case <-ctx.Done():
			return nil, trajectory.Snapshot{}, context.Cause(ctx)
		case <-changed:
		}
	}
}

func validatePlaybackReceipt(
	sessionID string, envelope element.Envelope, receipt speechelements.PlaybackReceipt,
) error {
	utterance := receipt.Utterance
	if envelope.SessionID != sessionID || !canonicalIdentity(envelope.RunID) ||
		!canonicalIdentity(utterance.ID) || receipt.Sequence == 0 ||
		envelope.SourceID != utterance.ID || envelope.CancellationScope != utterance.ID {
		return errors.New("receipt drifted from its exact session, run, sequence, or utterance")
	}
	if strings.TrimSpace(utterance.Text) == "" || utterance.Text != strings.TrimSpace(utterance.Text) ||
		len(utterance.Text) > maximumAdapterTextBytes {
		return errors.New("receipt utterance has invalid or unbounded text")
	}
	if len(utterance.AssistantItemIDs) > maximumAdapterMemory {
		return errors.New("receipt utterance exceeds the assistant-item bound")
	}
	for _, itemID := range utterance.AssistantItemIDs {
		if !canonicalIdentity(itemID) {
			return errors.New("receipt utterance has a noncanonical assistant item")
		}
	}
	if receipt.Kind == speechelements.PlaybackAudioEmitted {
		if len(receipt.Frame.PCM16LE) == 0 || len(receipt.Frame.PCM16LE)%2 != 0 ||
			receipt.Frame.SampleRateHz == 0 || receipt.Frame.Duration <= 0 {
			return errors.New("audio receipt has invalid PCM framing")
		}
	}
	return nil
}

func samePlaybackUtterance(left, right action.Utterance) bool {
	return left.ID == right.ID && left.Text == right.Text && left.Phase == right.Phase &&
		left.SourceRevision == right.SourceRevision && left.Continuer == right.Continuer &&
		left.SpokeOver == right.SpokeOver && slices.Equal(left.AssistantItemIDs, right.AssistantItemIDs)
}

func playbackReceiptKindForBoundary(boundary string) (speechelements.PlaybackReceiptKind, bool) {
	switch boundary {
	case gatewayTurnBeginBoundary:
		return speechelements.PlaybackReserved, true
	case gatewaySpeechBeginBoundary:
		return speechelements.PlaybackBegun, true
	case gatewaySpeechTextBoundary:
		return speechelements.PlaybackTextCommitted, true
	case gatewaySpeechAudioBoundary:
		return speechelements.PlaybackAudioEmitted, true
	case gatewaySpeechEndBoundary:
		return speechelements.PlaybackEnded, true
	case gatewayTurnEndBoundary:
		return speechelements.PlaybackReleased, true
	default:
		return "", false
	}
}

func activePlaybackReceiptKind(kind speechelements.PlaybackReceiptKind) bool {
	switch kind {
	case speechelements.PlaybackReserved, speechelements.PlaybackBegun,
		speechelements.PlaybackTextCommitted, speechelements.PlaybackAudioEmitted:
		return true
	default:
		return false
	}
}

func terminalPlaybackReceiptKind(kind speechelements.PlaybackReceiptKind) bool {
	return kind == speechelements.PlaybackEnded || kind == speechelements.PlaybackReleased
}

func (session *session) reservePlaybackStateLocked() error {
	if len(session.playback) < maximumAdapterMemory {
		return nil
	}
	for index, utteranceID := range session.playbackOrder {
		state, found := session.playback[utteranceID]
		if found && !state.terminal {
			continue
		}
		delete(session.playback, utteranceID)
		session.playbackOrder = append(session.playbackOrder[:index], session.playbackOrder[index+1:]...)
		return nil
	}
	return errors.New("scenario conversation active playback receipt bound reached")
}

func (session *session) rememberPendingSpeechLocked(runID string) error {
	if session.pendingSpeech == nil {
		session.pendingSpeech = make(map[string]struct{})
	}
	if _, found := session.pendingSpeech[runID]; found {
		return nil
	}
	if len(session.pendingSpeech) >= maximumAdapterMemory {
		return errors.New("scenario conversation pending foreground speech bound reached")
	}
	session.pendingSpeech[runID] = struct{}{}
	return nil
}

func (session *session) rememberSpeechlessRunLocked(runID string) {
	if session.speechlessRuns == nil {
		session.speechlessRuns = make(map[string]struct{})
	}
	rememberBoundedRunEvidence(session.speechlessRuns, &session.speechlessIDs, runID)
}

func (session *session) rememberSegmentedRunLocked(runID string) {
	if session.segmentedRuns == nil {
		session.segmentedRuns = make(map[string]struct{})
	}
	rememberBoundedRunEvidence(session.segmentedRuns, &session.segmentedRunIDs, runID)
}

func rememberBoundedRunEvidence(
	records map[string]struct{}, order *[]string, runID string,
) {
	if _, found := records[runID]; found {
		return
	}
	records[runID] = struct{}{}
	*order = append(*order, runID)
	for len(*order) > maximumAdapterMemory {
		oldest := (*order)[0]
		*order = (*order)[1:]
		delete(records, oldest)
	}
}

func (session *session) rememberSpeechRunLocked(runID string, remaining int) error {
	if session.speechRuns == nil {
		session.speechRuns = make(map[string]int)
	}
	if len(session.speechRuns) >= maximumAdapterMemory {
		return errors.New("scenario conversation pending speech-run bound reached")
	}
	session.speechRuns[runID] = remaining
	session.speechRunOrder = append(session.speechRunOrder, runID)
	return nil
}

func (session *session) completeSpeechRunUtteranceLocked(runID string) {
	remaining, found := session.speechRuns[runID]
	if !found {
		return
	}
	if remaining <= 1 {
		session.removeSpeechRunLocked(runID)
		return
	}
	session.speechRuns[runID] = remaining - 1
}

func (session *session) removeSpeechRunLocked(runID string) {
	if _, found := session.speechRuns[runID]; !found {
		return
	}
	delete(session.speechRuns, runID)
	for index, candidate := range session.speechRunOrder {
		if candidate == runID {
			session.speechRunOrder = append(session.speechRunOrder[:index], session.speechRunOrder[index+1:]...)
			break
		}
	}
}

func (session *session) acceptAdmissionState(envelope element.Envelope) error {
	state, ok := admissionStatePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation admission state has payload %T", envelope.Payload)
	}
	if state.Sequence == 0 {
		return errors.New("scenario conversation admission state has no sequence")
	}
	if state.Phase != acousticelements.AdmissionAwaitingPolicy {
		if state.Mode != acousticelements.EndpointAutomatic {
			return fmt.Errorf("scenario conversation admission selected endpoint mode %q", state.Mode)
		}
		session.audioReadyOnce.Do(func() { close(session.audioReady) })
	}
	return nil
}

func (session *session) acceptAdmissionOutcome(envelope element.Envelope) error {
	outcome, ok := admissionOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation admission outcome has payload %T", envelope.Payload)
	}
	if outcome.Operation == "policy" {
		if outcome.Kind != acousticelements.OutcomeSucceeded || outcome.Code != "policy_applied" ||
			outcome.StreamID != "" {
			return errors.New("scenario conversation admission policy did not apply exactly")
		}
		return nil
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(outcome.StreamID) {
		return errors.New("scenario conversation admission outcome drifted from its exact session or stream")
	}
	session.audioMu.Lock()
	var pending *pendingAudio
	for requestID, candidate := range session.audioOps {
		if slices.Contains(envelope.CausalParents, requestID) {
			if pending != nil {
				session.audioMu.Unlock()
				return errors.New("scenario conversation admission outcome names multiple pending audio frames")
			}
			pending = candidate
		}
	}
	if pending == nil {
		session.audioMu.Unlock()
		// Command outcomes can legitimately arrive after the exact audio caller
		// was released by a prior terminal state. They remain visible through
		// graph inspection and need no second gateway acknowledgement.
		if outcome.Operation == "command" {
			return nil
		}
		return errors.New("scenario conversation admission outcome has no exact pending audio request")
	}
	if outcome.StreamID != pending.streamID || pending.completed {
		session.audioMu.Unlock()
		return errors.New("scenario conversation admission outcome drifted or was delivered twice")
	}
	var terminal error
	switch outcome.Operation {
	case "audio":
		switch outcome.Kind {
		case acousticelements.OutcomeSucceeded:
			pending.completed = true
		case acousticelements.OutcomePending:
			if outcome.Code != "endpoint_candidate" || !canonicalIdentity(outcome.CandidateID) {
				terminal = fmt.Errorf("scenario conversation audio reached unexpected pending outcome %s/%s",
					outcome.Kind, outcome.Code)
				pending.completed = true
			} else {
				pending.awaitClose = true
			}
		default:
			terminal = fmt.Errorf("scenario conversation audio reached %s/%s: %s",
				outcome.Kind, outcome.Code, outcome.Message)
			pending.completed = true
		}
	case "command":
		if !pending.awaitClose {
			session.audioMu.Unlock()
			return errors.New("scenario conversation admission close arrived for a frame without an endpoint candidate")
		}
		if outcome.Kind != acousticelements.OutcomeSucceeded || outcome.Code != "closed" {
			terminal = fmt.Errorf("scenario conversation automatic close reached %s/%s: %s",
				outcome.Kind, outcome.Code, outcome.Message)
		} else if session.audioStreamID(session.audioStream) != pending.streamID {
			session.audioMu.Unlock()
			return errors.New("scenario conversation admission closed a stale audio stream")
		} else {
			if session.closedAudio == nil {
				session.closedAudio = make(map[string]struct{})
			}
			if _, found := session.closedAudio[pending.streamID]; !found &&
				len(session.closedAudio) >= maximumAdapterMemory {
				session.audioMu.Unlock()
				return errors.New("scenario conversation pending closed audio-stream bound reached")
			}
			session.closedAudio[pending.streamID] = struct{}{}
			session.audioStream++
		}
		pending.completed = true
	default:
		session.audioMu.Unlock()
		return fmt.Errorf("scenario conversation admission outcome has unexpected operation %q", outcome.Operation)
	}
	completed := pending.completed
	session.audioMu.Unlock()
	if completed {
		pending.result <- terminal
	}
	return nil
}

func (session *session) publishTranscript(ctx context.Context, envelope element.Envelope) error {
	observation, ok := observationPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation transcript has payload %T", envelope.Payload)
	}
	if err := session.validateObservation(envelope, observation); err != nil {
		return err
	}
	if observation.Source != SourceMicrophone && observation.Source != SourceOtherSpeaker {
		return errors.New("scenario conversation transcript boundary emitted a non-microphone observation")
	}
	if err := session.sink.Transcript(ctx, legacy.TranscriptEvent{
		ItemID: envelope.ItemID, Text: observation.Text, Final: observation.Final,
	}); err != nil {
		return fmt.Errorf("publish scenario conversation transcript: %w", err)
	}
	return nil
}

func (session *session) publishObservation(ctx context.Context, envelope element.Envelope) error {
	observation, ok := observationPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation observation has payload %T", envelope.Payload)
	}
	if err := session.validateObservation(envelope, observation); err != nil {
		return err
	}
	if err := session.sink.Observation(ctx, observation); err != nil {
		return fmt.Errorf("publish scenario conversation observation: %w", err)
	}
	return nil
}

func (session *session) validateObservation(
	envelope element.Envelope, observation perception.Observation,
) error {
	if envelope.SessionID != session.sessionID {
		return errors.New("scenario conversation observation crossed a session boundary")
	}
	if err := observation.Validate(); err != nil {
		return fmt.Errorf("scenario conversation graph observation: %w", err)
	}
	if observation.Authority != trajectory.AuthorityUser {
		return errors.New("scenario conversation graph observation drifted from user authority")
	}
	switch observation.Source {
	case SourceMicrophone:
		if !canonicalIdentity(observation.Observer) || len(observation.Media) != 0 {
			return errors.New("scenario conversation microphone observation has invalid observer or media")
		}
	case SourceOtherSpeaker:
		evidence := session.config.Architecture.Interaction.EvidenceCapabilities
		if evidence == nil || !evidence.SpeakerIdentity {
			return fmt.Errorf("scenario conversation graph emitted undeclared observation source %q", observation.Source)
		}
		if !canonicalIdentity(observation.Observer) || len(observation.Media) != 0 {
			return errors.New("scenario conversation attributed-speaker observation has invalid observer or media")
		}
	case SourceText:
		if observation.Observer != "client" || len(observation.Media) != 0 {
			return errors.New("scenario conversation typed observation drifted from the client text source")
		}
	case SourceMessage:
		if observation.Observer != "client" || len(observation.Media) != 1 || !observation.Final {
			return errors.New("scenario conversation attached-image observation drifted from the retained message source")
		}
	default:
		return fmt.Errorf("scenario conversation graph emitted undeclared observation source %q", observation.Source)
	}
	return nil
}

func (session *session) acceptIngressHandle(envelope element.Envelope) error {
	if err := session.bundle.media.RegisterHandle(envelope); err != nil {
		return err
	}
	handle, ok := attachmentHandlePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation media handle has payload %T", envelope.Payload)
	}
	session.contentMu.Lock()
	pending := session.pendingContentByRequestLocked(handle.AttachmentID)
	if pending == nil {
		_, terminal := session.terminalRequests[handle.AttachmentID]
		session.contentMu.Unlock()
		if terminal {
			return nil
		}
		return fmt.Errorf("scenario conversation media handle %q has no pending content request", handle.Handle)
	}
	if !pending.requiresHandle || !slices.Contains(envelope.CausalParents, pending.requestID) {
		session.contentMu.Unlock()
		return errors.New("scenario conversation media handle drifted from its exact image request")
	}
	pending.handleReady = true
	session.maybeCompleteContentLocked(pending, nil)
	session.contentMu.Unlock()
	return nil
}

func (session *session) acceptIngressOutcome(envelope element.Envelope) error {
	outcome, ok := ingressOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation ingress outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(outcome.StreamID) {
		return errors.New("scenario conversation ingress outcome drifted from its exact session or stream")
	}
	session.contentMu.Lock()
	pending := session.contentAcks[outcome.StreamID]
	if pending == nil {
		_, terminal := session.terminalContent[outcome.StreamID]
		session.contentMu.Unlock()
		if terminal {
			return nil
		}
		return fmt.Errorf("scenario conversation ingress outcome stream %q has no pending request", outcome.StreamID)
	}
	if len(envelope.CausalParents) != 1 || envelope.CausalParents[0] != pending.requestID ||
		outcome.ContentID != pending.requestID {
		session.contentMu.Unlock()
		return errors.New("scenario conversation ingress outcome has a non-exact content parent")
	}
	if outcome.Kind != ingresselements.OutcomeSucceeded {
		session.maybeCompleteContentLocked(pending, fmt.Errorf(
			"scenario conversation %s reached %s/%s: %s",
			outcome.Operation, outcome.Kind, outcome.Code, outcome.Message,
		))
	}
	session.contentMu.Unlock()
	return nil
}

func (session *session) acceptMessageCommit(envelope element.Envelope) error {
	outcome, ok := observationCommitOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation message commit has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(outcome.StreamID) {
		return errors.New("scenario conversation message commit drifted from its exact session or stream")
	}
	session.contentMu.Lock()
	pending := session.contentAcks[outcome.StreamID]
	if pending == nil {
		_, terminal := session.terminalContent[outcome.StreamID]
		session.contentMu.Unlock()
		if terminal {
			return nil
		}
		return fmt.Errorf("scenario conversation message commit stream %q has no pending request", outcome.StreamID)
	}
	if !slices.Contains(envelope.CausalParents, pending.requestID) {
		session.contentMu.Unlock()
		return errors.New("scenario conversation message commit does not name its exact ingress request")
	}
	if outcome.Kind != stateelements.ObservationCommitted {
		session.maybeCompleteContentLocked(pending, fmt.Errorf(
			"scenario conversation message commit reached %s/%s: %s",
			outcome.Kind, outcome.Code, outcome.Message,
		))
		session.contentMu.Unlock()
		return nil
	}
	if outcome.StoreVersion == 0 || outcome.ObservationRevision != 1 || outcome.SourceRevision == 0 ||
		!canonicalIdentity(outcome.TriggerItemID) || !canonicalIdentity(outcome.TrajectoryItemID) ||
		outcome.Context.Prefix.Version != outcome.StoreVersion ||
		!canonicalIdentity(outcome.Context.StateItemID) || outcome.Context.Prefix.Digest == "" ||
		!slices.Contains(envelope.CausalParents, outcome.TriggerItemID) ||
		!slices.Contains(envelope.CausalParents, outcome.Context.StateItemID) {
		session.contentMu.Unlock()
		return errors.New("scenario conversation committed message lacks canonical trajectory evidence")
	}
	pending.commitReady = true
	pending.commitVersion = outcome.StoreVersion
	pending.commitContext = outcome.Context
	pending.trajectoryItem = outcome.TrajectoryItemID
	if session.snapshotVersion >= pending.commitVersion {
		snapshot := session.bundle.store.Snapshot()
		if err := attestPendingSnapshot(pending, snapshot); err != nil {
			session.maybeCompleteContentLocked(pending, err)
		} else {
			pending.snapshotReady = true
		}
	}
	session.maybeCompleteContentLocked(pending, nil)
	session.contentMu.Unlock()
	return nil
}

func (session *session) acceptTrajectorySnapshot(envelope element.Envelope) error {
	snapshot, ok := trajectorySnapshotPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation trajectory snapshot has payload %T", envelope.Payload)
	}
	if envelope.SessionID != "" && envelope.SessionID != session.sessionID {
		return errors.New("scenario conversation trajectory snapshot crossed a session boundary")
	}
	if !canonicalIdentity(envelope.ItemID) {
		return errors.New("scenario conversation trajectory snapshot lacks a canonical state item ID")
	}
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return errors.New("scenario conversation trajectory snapshot version does not match its item count")
	}
	current := session.bundle.store.Snapshot()
	if snapshot.Version > current.Version {
		return errors.New("scenario conversation trajectory snapshot exceeds the durable store")
	}
	publishedPrefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		return fmt.Errorf("identify scenario conversation published trajectory prefix: %w", err)
	}
	durablePrefix, err := trajectory.IdentifyPrefix(current, snapshot.Version)
	if err != nil {
		return fmt.Errorf("identify scenario conversation durable trajectory prefix: %w", err)
	}
	if publishedPrefix != durablePrefix {
		return errors.New("scenario conversation trajectory snapshot drifted from the durable store prefix")
	}
	session.contentMu.Lock()
	if snapshot.Version < session.snapshotVersion {
		session.contentMu.Unlock()
		return errors.New("scenario conversation trajectory snapshot version moved backwards")
	}
	if snapshot.Version == session.snapshotVersion && session.snapshotItemID != "" &&
		envelope.ItemID != session.snapshotItemID {
		session.contentMu.Unlock()
		return errors.New("scenario conversation trajectory snapshot identity changed at one version")
	}
	changed := session.snapshotItemID == "" || snapshot.Version > session.snapshotVersion
	session.snapshotVersion = snapshot.Version
	session.snapshotItemID = envelope.ItemID
	if changed {
		close(session.snapshotChanged)
		session.snapshotChanged = make(chan struct{})
	}
	for _, pending := range session.contentAcks {
		if pending.commitReady && !pending.snapshotReady && snapshot.Version >= pending.commitVersion {
			if err := attestPendingSnapshot(pending, snapshot); err != nil {
				session.maybeCompleteContentLocked(pending, err)
				continue
			}
			pending.snapshotReady = true
		}
		session.maybeCompleteContentLocked(pending, nil)
	}
	session.contentMu.Unlock()
	return nil
}

func attestPendingSnapshot(pending *pendingContent, snapshot trajectory.Snapshot) error {
	if pending == nil || !pending.commitReady || pending.commitVersion == 0 ||
		pending.commitVersion > uint64(len(snapshot.Items)) {
		return errors.New("trajectory snapshot cannot attest the pending message commit")
	}
	identity, err := trajectory.IdentifyPrefix(snapshot, pending.commitVersion)
	if err != nil {
		return err
	}
	if identity != pending.commitContext.Prefix ||
		snapshot.Items[pending.commitVersion-1].ID != pending.trajectoryItem {
		return errors.New("trajectory snapshot does not attest the pending message commit")
	}
	return nil
}

func (session *session) pendingContentByRequestLocked(requestID string) *pendingContent {
	for _, pending := range session.contentAcks {
		if pending.requestID == requestID {
			return pending
		}
	}
	return nil
}

func (session *session) maybeCompleteContentLocked(pending *pendingContent, failure error) {
	if pending == nil || pending.completed {
		return
	}
	if failure == nil && (!pending.commitReady || !pending.handleReady || !pending.snapshotReady) {
		return
	}
	pending.completed = true
	pending.result <- failure
}

func (session *session) acceptInvocationOutcome(envelope element.Envelope) error {
	outcome, ok := sessionInvocationOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation invocation outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID ||
		(outcome.Role != "foreground" && outcome.Role != "silent") {
		return errors.New("scenario conversation invocation outcome drifted from its exact session or role")
	}
	// Both exact invocation nodes receive session updates and cancellation
	// memory. Foreground remains the single gateway acknowledgement owner;
	// the silent node must nevertheless prove it accepted the identical typed
	// control operation before its duplicate outcome is drained.
	if outcome.Role == "silent" && outcome.Operation == "update" {
		if outcome.Kind != policyelements.SessionInvocationUpdated ||
			outcome.InvocationRevision == 0 || !canonicalIdentity(outcome.InvocationDigest) {
			return errors.New("scenario conversation silent invocation refused or drifted from session update")
		}
		return nil
	}
	if outcome.Role == "silent" && outcome.Operation == "cancel" {
		if outcome.Kind != policyelements.SessionInvocationIgnored || outcome.Code != "cancel_recorded" ||
			!canonicalIdentity(outcome.GenerationID) {
			return errors.New("scenario conversation silent invocation refused cancellation memory")
		}
		return nil
	}
	internal := outcome.Operation == "committed"
	gatewayParent := ""
	if !internal {
		internal = exactPostCommitSilenceCreateOutcome(envelope, outcome)
		if !internal {
			var exact bool
			gatewayParent, exact = exactGatewayInvocationOutcome(envelope, outcome)
			if !exact {
				return errors.New("scenario conversation invocation outcome has no exact gateway and semantic-policy parents")
			}
		}
	}
	if internal {
		return session.registerEmittedInvocation(outcome)
	}
	parent := gatewayParent
	session.operationMu.Lock()
	pending := session.pendingOps[parent]
	if pending == nil {
		session.operationMu.Unlock()
		if outcome.Operation == "committed" {
			return nil
		}
		return fmt.Errorf("scenario conversation invocation outcome parent %q is not pending", parent)
	}
	if pending.completed || pending.operation != outcome.Operation {
		session.operationMu.Unlock()
		return errors.New("scenario conversation invocation outcome duplicated or changed operation")
	}
	var result operationAck
	switch pending.operation {
	case "update":
		if outcome.Kind != policyelements.SessionInvocationUpdated ||
			outcome.InvocationRevision != pending.revision || outcome.InvocationDigest != pending.digest {
			result.err = invocationOutcomeError(outcome)
		}
	case "create":
		if outcome.Kind != policyelements.SessionInvocationEmitted || !canonicalIdentity(outcome.GenerationID) {
			result.err = invocationOutcomeError(outcome)
		} else {
			result.generationID = outcome.GenerationID
		}
	case "cancel":
		if outcome.Kind != policyelements.SessionInvocationIgnored || outcome.Code != "cancel_recorded" ||
			outcome.GenerationID != pending.generation {
			result.err = invocationOutcomeError(outcome)
		}
	default:
		result.err = fmt.Errorf("scenario conversation unknown pending operation %q", pending.operation)
	}
	if err := session.registerEmittedInvocation(outcome); err != nil {
		session.operationMu.Unlock()
		return err
	}
	pending.completed = true
	delete(session.pendingOps, parent)
	abandoned := pending.abandoned
	session.operationMu.Unlock()
	if !abandoned {
		pending.result <- result
	}
	return nil
}

func (session *session) acceptSemanticAdmissionOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := semanticAdmissionOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation semantic admission outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID {
		return errors.New("scenario conversation semantic admission outcome crossed a session boundary")
	}
	switch outcome.Operation {
	case "committed", "quiet":
		// Observation and graph-owned timer decisions have no gateway operation
		// to acknowledge; their complete typed evidence remains on this boundary.
		if outcome.Kind == policyelements.SemanticAdmissionFailed ||
			outcome.Kind == policyelements.SemanticAdmissionRefused {
			return session.publishTypedFailure(ctx, outcome.Code, semanticAdmissionOutcomeError(outcome).Error())
		}
		return nil
	case "cancel":
		if outcome.Kind != policyelements.SemanticAdmissionIgnored ||
			(outcome.Code != "cancel_recorded" && outcome.Code != "generation_not_owned") {
			return semanticAdmissionOutcomeError(outcome)
		}
		return nil
	case "update":
		// A valid update produces state only. Any terminal outcome here means
		// SemanticAdmission rejected an update the invocation nodes may have
		// accepted, so the composed session can no longer claim one policy.
		return semanticAdmissionOutcomeError(outcome)
	case "create":
	default:
		return fmt.Errorf("scenario conversation semantic admission outcome has unsupported operation %q", outcome.Operation)
	}

	parent, exact := exactGatewaySemanticOutcome(envelope)
	if !exact {
		return errors.New("scenario conversation semantic admission outcome has no exact gateway parent")
	}
	if outcome.Kind == policyelements.SemanticAdmissionAdmitted &&
		((outcome.Act != coreinteraction.ActAnswer && outcome.Act != coreinteraction.ActActSilently) ||
			!exactSemanticDecisionIdentity(outcome.DecisionItemID)) {
		return errors.New("scenario conversation semantic admission produced an invalid admitted branch")
	}
	if outcome.Kind == policyelements.SemanticAdmissionSuppressed &&
		(outcome.Act != coreinteraction.ActStaySilent || !exactSemanticDecisionIdentity(outcome.DecisionItemID)) {
		return errors.New("scenario conversation semantic admission produced an invalid suppressed branch")
	}
	session.operationMu.Lock()
	pending := session.pendingOps[parent]
	if pending == nil {
		session.operationMu.Unlock()
		// The admitted branch can race its separate invocation-outcome boundary,
		// which may already have completed the exact pending request.
		if outcome.Kind == policyelements.SemanticAdmissionAdmitted {
			return nil
		}
		return fmt.Errorf("scenario conversation semantic admission parent %q is not pending", parent)
	}
	if pending.completed || pending.operation != "create" || pending.requestID != parent {
		session.operationMu.Unlock()
		return errors.New("scenario conversation semantic admission outcome duplicated or changed operation")
	}
	if outcome.Kind == policyelements.SemanticAdmissionAdmitted {
		session.operationMu.Unlock()
		return nil
	}
	result := operationAck{err: semanticAdmissionOutcomeError(outcome)}
	if outcome.Kind == policyelements.SemanticAdmissionSuppressed {
		// A semantic listen decision is the successful result of evaluating
		// this response.create. It intentionally owns no generation and emits
		// no response lifecycle. Treating it as a gateway error tears down
		// otherwise healthy sessions which use explicit creates to re-evaluate
		// incomplete visual evidence.
		result.err = nil
	}
	pending.completed = true
	delete(session.pendingOps, parent)
	abandoned := pending.abandoned
	session.operationMu.Unlock()
	if !abandoned {
		pending.result <- result
	}
	return nil
}

func exactGatewaySemanticOutcome(envelope element.Envelope) (string, bool) {
	const prefix = "semantic_admission:outcome:"
	if !strings.HasPrefix(envelope.ItemID, prefix) || len(envelope.CausalParents) != 1 {
		return "", false
	}
	sequence, err := strconv.ParseUint(strings.TrimPrefix(envelope.ItemID, prefix), 10, 64)
	if err != nil || sequence == 0 || !canonicalIdentity(envelope.CausalParents[0]) {
		return "", false
	}
	return envelope.CausalParents[0], true
}

type semanticAdmissionError struct {
	outcome policyelements.SemanticAdmissionOutcome
}

func (failure *semanticAdmissionError) Error() string {
	outcome := failure.outcome
	return fmt.Sprintf("scenario conversation semantic admission %s reached %s/%s: %s",
		outcome.Operation, outcome.Kind, outcome.Code, outcome.Message)
}

func semanticAdmissionOutcomeError(outcome policyelements.SemanticAdmissionOutcome) error {
	return &semanticAdmissionError{outcome: outcome}
}

func (session *session) registerEmittedInvocation(
	outcome policyelements.SessionInvocationOutcome,
) error {
	if outcome.Kind != policyelements.SessionInvocationEmitted {
		return nil
	}
	if !canonicalIdentity(outcome.GenerationID) ||
		(outcome.Operation != "create" && outcome.Operation != "committed") {
		return errors.New("scenario conversation emitted invocation has invalid generation identity")
	}
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	if _, terminal := session.terminalRuns[outcome.GenerationID]; terminal {
		return nil
	}
	if _, duplicate := session.active[outcome.GenerationID]; duplicate {
		return fmt.Errorf("scenario conversation generation %q was emitted twice", outcome.GenerationID)
	}
	if outcome.Role == "foreground" {
		_, speechless := session.speechlessRuns[outcome.GenerationID]
		_, segmented := session.segmentedRuns[outcome.GenerationID]
		if !speechless && !segmented {
			if err := session.rememberPendingSpeechLocked(outcome.GenerationID); err != nil {
				return err
			}
		}
	}
	session.active[outcome.GenerationID] = struct{}{}
	return nil
}

// exactGatewayInvocationOutcome binds a graph acknowledgement to the adapter
// operation that caused it. Update and cancellation remain direct one-parent
// control operations. Creation necessarily crosses SemanticAdmission, so its
// exact causal shape has both the gateway request and the fixed graph policy
// node's decision. Arbitrary extra parents, a different policy node, or an
// outcome identity derived from any other cause are rejected.
func exactGatewayInvocationOutcome(
	envelope element.Envelope, outcome policyelements.SessionInvocationOutcome,
) (string, bool) {
	const outcomeInfix = ":session_invocation_outcome:"
	causeID, outcomeSequence, found := strings.Cut(envelope.ItemID, outcomeInfix)
	if !found || !canonicalIdentity(causeID) {
		return "", false
	}
	if sequence, err := strconv.ParseUint(outcomeSequence, 10, 64); err != nil || sequence == 0 {
		return "", false
	}
	causeParents := 0
	otherParent := ""
	for _, parent := range envelope.CausalParents {
		if parent == causeID {
			causeParents++
			continue
		}
		if otherParent != "" {
			return "", false
		}
		otherParent = parent
	}
	if causeParents != 1 {
		return "", false
	}
	switch outcome.Operation {
	case "update", "cancel":
		if len(envelope.CausalParents) != 1 || otherParent != "" {
			return "", false
		}
	case "create":
		if len(envelope.CausalParents) != 2 || !exactSemanticDecisionIdentity(otherParent) {
			return "", false
		}
	default:
		return "", false
	}
	return causeID, true
}

func exactSemanticDecisionIdentity(value string) bool {
	const prefix = "semantic_admission:decision:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	sequence, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 64)
	return err == nil && sequence > 0
}

// exactPostCommitSilenceCreateOutcome recognizes the one graph-internal create
// path in this descriptor-locked profile. The silence element preserves the
// durable commit parents and adds its own response ID, so its policy outcome is
// intentionally not a one-parent gateway acknowledgement. Binding both the
// fixed node identity and the policy runner's direct-cause item identity keeps
// an arbitrary multi-parent create from acquiring an active generation.
func exactPostCommitSilenceCreateOutcome(
	envelope element.Envelope, outcome policyelements.SessionInvocationOutcome,
) bool {
	if outcome.Operation != "create" || outcome.Kind != policyelements.SessionInvocationEmitted {
		return false
	}
	const (
		causePrefix  = "post_commit_silence:post_commit_silence:"
		outcomeInfix = ":session_invocation_outcome:"
	)
	causeID, outcomeSequence, found := strings.Cut(envelope.ItemID, outcomeInfix)
	if !found || !strings.HasPrefix(causeID, causePrefix) {
		return false
	}
	causeSequence := strings.TrimPrefix(causeID, causePrefix)
	if sequence, err := strconv.ParseUint(causeSequence, 10, 64); err != nil || sequence == 0 {
		return false
	}
	if sequence, err := strconv.ParseUint(outcomeSequence, 10, 64); err != nil || sequence == 0 {
		return false
	}
	parents := 0
	decisions := 0
	for _, parent := range envelope.CausalParents {
		if parent == causeID {
			parents++
		}
		if exactSemanticDecisionIdentity(parent) {
			decisions++
		}
	}
	return parents == 1 && decisions == 1
}

func invocationOutcomeError(outcome policyelements.SessionInvocationOutcome) error {
	return fmt.Errorf("scenario conversation invocation %s reached %s/%s: %s",
		outcome.Operation, outcome.Kind, outcome.Code, outcome.Message)
}

func (session *session) publishCall(ctx context.Context, envelope element.Envelope) error {
	committed, ok := committedActionPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation dispatch commit has payload %T", envelope.Payload)
	}
	declared := committed.Executable.Canonical.Authorized.Confirmed.Declared
	admitted := declared.Admitted
	call := declaredActionCall(declared)
	if err := validateToolCall(call); err != nil {
		return err
	}
	if envelope.SessionID != session.sessionID || admitted.SessionID != session.sessionID ||
		!canonicalIdentity(admitted.ModelRunID) || envelope.RunID != admitted.ModelRunID ||
		admitted.Authority != trajectory.AuthorityUser ||
		!sameToolCall(call, declaredActionCall(declared)) ||
		!canonicalIdentity(committed.Executable.CommitmentID) {
		return errors.New("scenario conversation committed call drifted from canonical user authority")
	}
	if err := session.bundle.bridge.awaitEmission(ctx, session.sessionID, admitted.ModelRunID, committed); err != nil {
		return fmt.Errorf("authorize scenario conversation client call: %w", err)
	}
	active := activeClientCall{
		call: call, runID: admitted.ModelRunID, commitmentID: committed.Executable.CommitmentID,
	}
	session.activityMu.Lock()
	if _, duplicate := session.calls[call.CallID]; duplicate {
		session.activityMu.Unlock()
		err := fmt.Errorf("scenario conversation call %q is already active", call.CallID)
		session.bundle.bridge.failEmission(session.sessionID, admitted.ModelRunID, call.CallID, err)
		return err
	}
	session.calls[call.CallID] = active
	session.activityMu.Unlock()
	fail := func(err error) error {
		session.bundle.bridge.failEmission(session.sessionID, admitted.ModelRunID, call.CallID, err)
		return err
	}
	if err := session.sink.TurnBegin(ctx); err != nil {
		return fail(fmt.Errorf("begin scenario conversation action turn: %w", err))
	}
	if err := session.sink.ToolCalls(ctx, legacy.ToolCallEvent{
		InvocationID: admitted.ModelRunID, Calls: []trajectory.ToolCall{cloneToolCall(call)},
	}); err != nil {
		endErr := session.sink.TurnEnd(ctx, legacy.TurnOutcome{Incomplete: true, Detail: err.Error()})
		return fail(errors.Join(fmt.Errorf("publish scenario conversation client call: %w", err), endErr))
	}
	if err := session.sink.TurnEnd(ctx, legacy.TurnOutcome{}); err != nil {
		return fail(fmt.Errorf("end scenario conversation action turn: %w", err))
	}
	return nil
}

func (session *session) acceptCanonicalResult(envelope element.Envelope) error {
	canonical, ok := canonicalResultPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation canonical result has payload %T", envelope.Payload)
	}
	result := cloneToolResult(canonical.Execution.Result)
	if err := validateToolResult(result); err != nil {
		return fmt.Errorf("scenario conversation canonical result: %w", err)
	}
	session.activityMu.Lock()
	active, found := session.calls[result.CallID]
	session.activityMu.Unlock()
	if !found || envelope.SessionID != session.sessionID || envelope.RunID != active.runID ||
		canonical.Execution.CallID != result.CallID || canonical.Execution.Name != result.Name ||
		canonical.Execution.CommitmentID != active.commitmentID ||
		canonical.Execution.Executable.CommitmentID != active.commitmentID ||
		canonical.TrajectoryItemID == "" || canonical.StoreVersion == 0 ||
		!sameToolCall(active.call,
			declaredActionCall(canonical.Execution.Executable.Canonical.Authorized.Confirmed.Declared)) {
		return fmt.Errorf("scenario conversation canonical result %q has no exact emitted action", result.CallID)
	}
	switch canonical.Execution.CompletionOrigin {
	case actionelements.CompletionReturned:
		if err := session.bundle.bridge.canonical(canonical, envelope.ItemID, nil); err != nil {
			return err
		}
	case actionelements.CompletionDispatcherError:
		// A signed dispatcher_error has no client ingress receipt by definition.
		// Its exact ledger/store evidence above is sufficient; routing it through
		// the client-result rendezvous would fabricate an acceptance boundary.
	default:
		return errors.New("scenario conversation canonical result has unknown completion origin")
	}
	session.activityMu.Lock()
	session.rememberTerminalCallLocked(active)
	delete(session.calls, result.CallID)
	session.activityMu.Unlock()
	return nil
}

func (session *session) acceptModelOutcome(ctx context.Context, envelope element.Envelope) error {
	outcome, ok := cognitionOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation model outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || !canonicalIdentity(outcome.RunID) ||
		(envelope.RunID != "" && envelope.RunID != outcome.RunID) {
		return errors.New("scenario conversation model outcome drifted from its exact session or run")
	}
	if outcome.Operation == "cancel" {
		switch outcome.Kind {
		case cognitionelements.OutcomeIgnored, cognitionelements.OutcomeCanceled:
			return nil
		case cognitionelements.OutcomeRefused, cognitionelements.OutcomeFailed:
			return session.publishTypedFailure(ctx, outcome.Code, outcome.Message)
		default:
			return fmt.Errorf("scenario conversation model cancellation has unsupported kind %q", outcome.Kind)
		}
	}
	if outcome.Operation != "generate" {
		return fmt.Errorf("scenario conversation model outcome has unsupported operation %q", outcome.Operation)
	}
	session.activityMu.Lock()
	_, speechless := session.speechlessRuns[outcome.RunID]
	_, segmented := session.segmentedRuns[outcome.RunID]
	if !speechless && !segmented {
		if err := session.rememberPendingSpeechLocked(outcome.RunID); err != nil {
			session.activityMu.Unlock()
			return err
		}
	}
	delete(session.active, outcome.RunID)
	session.rememberTerminalRunLocked(outcome.RunID)
	session.activityMu.Unlock()
	switch outcome.Kind {
	case cognitionelements.OutcomeSucceeded, cognitionelements.OutcomeCanceled,
		cognitionelements.OutcomeIgnored:
		return nil
	case cognitionelements.OutcomeRefused:
		// The model-cancellation lane can overtake a queued committed-context
		// trigger. Cognition retains an exact (session, run) tombstone and emits
		// this refusal only when the later trigger proves that cancellation
		// already made the run terminal without invoking the provider. Treat the
		// proof as the clean terminal half of that cancellation, not as a new
		// user-visible model failure.
		if outcome.Code == "committed_run_replay" {
			return nil
		}
		return session.publishTypedFailure(ctx, outcome.Code, outcome.Message)
	case cognitionelements.OutcomeFailed:
		return session.publishTypedFailure(ctx, outcome.Code, outcome.Message)
	default:
		return fmt.Errorf("scenario conversation model outcome has unsupported kind %q", outcome.Kind)
	}
}

func (session *session) acceptActionOutcome(
	ctx context.Context, boundary string, envelope element.Envelope,
) error {
	outcome, ok := actionOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation action outcome %s has payload %T", boundary, envelope.Payload)
	}
	if envelope.SessionID != "" && envelope.SessionID != session.sessionID {
		return fmt.Errorf("scenario conversation action outcome %s crossed a session boundary", boundary)
	}
	switch outcome.Kind {
	case actionelements.OutcomeSucceeded, actionelements.OutcomeIgnored,
		actionelements.OutcomeCanceled:
		return nil
	case actionelements.OutcomeRejected, actionelements.OutcomeDenied,
		actionelements.OutcomeTimedOut, actionelements.OutcomeFailed:
		failure := fmt.Errorf("scenario conversation action %s/%s reached %s/%s: %s",
			outcome.Stage, outcome.Operation, outcome.Kind, outcome.Code, outcome.Message)
		if boundary == resultCommitOutcomeBoundary && outcome.CallID != "" {
			session.activityMu.Lock()
			active, found := session.calls[outcome.CallID]
			session.activityMu.Unlock()
			if !found || envelope.SessionID != session.sessionID || envelope.RunID != active.runID {
				return errors.New("scenario conversation result-commit failure has no exact active call")
			}
			if err := session.bundle.bridge.canonicalFailure(
				session.sessionID, active.runID, outcome.CallID, active.commitmentID, failure,
			); err != nil {
				return err
			}
			session.activityMu.Lock()
			session.rememberTerminalCallLocked(active)
			delete(session.calls, outcome.CallID)
			session.activityMu.Unlock()
		}
		if !session.rememberFailure(boundary + ":" + outcome.CallID + ":" + outcome.Code) {
			return nil
		}
		return session.publishTypedFailure(ctx, outcome.Code, failure.Error())
	default:
		return fmt.Errorf("scenario conversation action outcome %s has unsupported kind %q", boundary, outcome.Kind)
	}
}

func (session *session) acceptClientToolResultOutcome(envelope element.Envelope) error {
	outcome, ok := actionOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation client tool-result outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || outcome.Stage != "client_tool_result" ||
		!canonicalIdentity(outcome.CallID) || !canonicalIdentity(envelope.ItemID) {
		return errors.New("scenario conversation client tool-result outcome drifted from its exact operation")
	}
	if outcome.Operation == "cancel" {
		session.activityMu.Lock()
		active, found := session.calls[outcome.CallID]
		activeExact := found && active.runID == envelope.RunID
		_, terminal := session.terminalCalls[clientCallScopeKey(
			session.sessionID, envelope.RunID, outcome.CallID)]
		session.activityMu.Unlock()
		if (!activeExact && !terminal) || envelope.CancellationScope != envelope.RunID ||
			envelope.OpportunityID != outcome.CallID {
			return errors.New("scenario conversation client tool-result cancellation has no exact active call")
		}
		switch outcome.Kind {
		case actionelements.OutcomeSucceeded, actionelements.OutcomeIgnored,
			actionelements.OutcomeCanceled:
			return nil
		default:
			return fmt.Errorf("scenario conversation client tool-result cancellation reached %s/%s: %s",
				outcome.Kind, outcome.Code, outcome.Message)
		}
	}
	if outcome.Operation != "result" {
		return errors.New("scenario conversation client tool-result outcome has unsupported operation")
	}
	if !canonicalIdentity(outcome.IngressItemID) ||
		!slices.Contains(envelope.CausalParents, outcome.IngressItemID) {
		return errors.New("scenario conversation client tool-result outcome has no exact ingress parent")
	}
	session.operationMu.Lock()
	pending := session.pendingOps[outcome.IngressItemID]
	if pending == nil || pending.operation != "tool_result" || pending.completed ||
		pending.requestID != outcome.IngressItemID || pending.callID != outcome.CallID ||
		pending.generation != envelope.RunID || envelope.CancellationScope != pending.generation ||
		envelope.OpportunityID != pending.callID || outcome.ResultDigest != pending.digest {
		session.operationMu.Unlock()
		return errors.New("scenario conversation client tool-result outcome has no exact pending operation")
	}
	var result operationAck
	if err := validateClientToolResultFinalOutcome(envelope, outcome, pending); err != nil {
		session.operationMu.Unlock()
		return err
	}
	if outcome.Kind != actionelements.OutcomeSucceeded || outcome.Code != "" {
		result.err = fmt.Errorf("scenario conversation client tool result reached %s/%s: %s",
			outcome.Kind, outcome.Code, outcome.Message)
	}
	pending.completed = true
	delete(session.pendingOps, pending.requestID)
	abandoned := pending.abandoned
	session.operationMu.Unlock()
	if !abandoned {
		pending.result <- result
	}
	return nil
}

func (session *session) rememberTerminalCallLocked(active activeClientCall) {
	key := clientCallScopeKey(session.sessionID, active.runID, active.call.CallID)
	if _, found := session.terminalCalls[key]; found {
		return
	}
	session.terminalCalls[key] = struct{}{}
	session.terminalCallIDs = append(session.terminalCallIDs, key)
	for len(session.terminalCallIDs) > maximumAdapterMemory {
		oldest := session.terminalCallIDs[0]
		session.terminalCallIDs = session.terminalCallIDs[1:]
		delete(session.terminalCalls, oldest)
	}
}

func validateClientToolResultFinalOutcome(
	envelope element.Envelope, outcome actionelements.Outcome, pending *pendingOperation,
) error {
	if outcome.Kind != actionelements.OutcomeSucceeded || outcome.Code != "" {
		return nil
	}
	if !canonicalIdentity(outcome.AcceptedItemID) ||
		!canonicalIdentity(outcome.CanonicalEnvelopeItemID) ||
		!canonicalIdentity(outcome.CanonicalTrajectoryItemID) ||
		outcome.CanonicalStoreVersion == 0 ||
		outcome.AcceptedItemID == pending.requestID ||
		outcome.CanonicalEnvelopeItemID == pending.requestID ||
		outcome.CanonicalEnvelopeItemID == outcome.AcceptedItemID ||
		!slices.Contains(envelope.CausalParents, outcome.AcceptedItemID) ||
		!slices.Contains(envelope.CausalParents, outcome.CanonicalEnvelopeItemID) {
		return errors.New("scenario conversation client tool-result success lacks exact accepted and canonical evidence")
	}
	return nil
}

func (session *session) publishTypedFailure(ctx context.Context, code, message string) error {
	if !canonicalIdentity(code) {
		code = "graph_failure"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "scenario conversation graph operation failed"
	}
	if len(message) > maximumAdapterTextBytes {
		message = message[:maximumAdapterTextBytes]
	}
	if err := session.sink.TurnBegin(ctx); err != nil {
		return err
	}
	session.sink.Failed(ctx, legacy.ErrorEvent{Code: code, Message: message})
	return session.sink.TurnEnd(ctx, legacy.TurnOutcome{Incomplete: true, Detail: message})
}

func (session *session) rememberFailure(key string) bool {
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	if _, duplicate := session.failures[key]; duplicate {
		return false
	}
	session.failures[key] = struct{}{}
	session.failureIDs = append(session.failureIDs, key)
	for len(session.failureIDs) > maximumAdapterMemory {
		oldest := session.failureIDs[0]
		session.failureIDs = session.failureIDs[1:]
		delete(session.failures, oldest)
	}
	return true
}

func (session *session) rememberTerminalRunLocked(runID string) {
	if _, found := session.terminalRuns[runID]; !found {
		session.terminalRuns[runID] = struct{}{}
		session.terminalRunIDs = append(session.terminalRunIDs, runID)
	}
	for len(session.terminalRunIDs) > maximumAdapterMemory {
		oldest := session.terminalRunIDs[0]
		session.terminalRunIDs = session.terminalRunIDs[1:]
		delete(session.terminalRuns, oldest)
	}
}

func (session *session) publishDebug(
	ctx context.Context, name string, envelope element.Envelope,
) error {
	sink, ok := session.sink.(legacy.DebugSink)
	if !ok {
		return nil
	}
	// Keep media bytes and arbitrary provider data out of debug traffic.
	// The gateway withholds these decision details unless payloads are opted in.
	var payload map[string]any
	switch name {
	case "semantic_decision", "semantic_admission_state", "semantic_admission_outcome", "overlap_state", "segmentation_outcome", "model_commit_outcome":
		payload = map[string]any{"value": envelope.Payload}
	}
	return sink.Debug(ctx, legacy.DebugEvent{
		Category: string(openrealtime.DebugGraph), Name: name, Phase: "output", CorrelationID: envelope.ItemID,
		Payload: payload,
		Attributes: map[string]any{
			"run_id": envelope.RunID, "session_id": envelope.SessionID,
			"type": envelope.Type.String(),
		},
	})
}

func speechActivityPayload(payload any) (acousticelements.SpeechActivity, bool) {
	switch value := payload.(type) {
	case acousticelements.SpeechActivity:
		return value, true
	case *acousticelements.SpeechActivity:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.SpeechActivity{}, false
}

func admissionStatePayload(payload any) (acousticelements.AdmissionState, bool) {
	switch value := payload.(type) {
	case acousticelements.AdmissionState:
		return value, true
	case *acousticelements.AdmissionState:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.AdmissionState{}, false
}

func admissionOutcomePayload(payload any) (acousticelements.AdmissionOutcome, bool) {
	switch value := payload.(type) {
	case acousticelements.AdmissionOutcome:
		return value, true
	case *acousticelements.AdmissionOutcome:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.AdmissionOutcome{}, false
}

func observationPayload(payload any) (perception.Observation, bool) {
	switch value := payload.(type) {
	case perception.Observation:
		value.Media = slices.Clone(value.Media)
		return value, true
	case *perception.Observation:
		if value != nil {
			copy := *value
			copy.Media = slices.Clone(value.Media)
			return copy, true
		}
	}
	return perception.Observation{}, false
}

func ingressOutcomePayload(payload any) (ingresselements.Outcome, bool) {
	switch value := payload.(type) {
	case ingresselements.Outcome:
		return value, true
	case *ingresselements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return ingresselements.Outcome{}, false
}

func observationCommitOutcomePayload(payload any) (stateelements.ObservationCommitOutcome, bool) {
	switch value := payload.(type) {
	case stateelements.ObservationCommitOutcome:
		return value, true
	case *stateelements.ObservationCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.ObservationCommitOutcome{}, false
}

func trajectorySnapshotPayload(payload any) (trajectory.Snapshot, bool) {
	switch value := payload.(type) {
	case trajectory.Snapshot:
		value.Items = slices.Clone(value.Items)
		return value, true
	case *trajectory.Snapshot:
		if value != nil {
			copy := *value
			copy.Items = slices.Clone(value.Items)
			return copy, true
		}
	}
	return trajectory.Snapshot{}, false
}

func sessionInvocationOutcomePayload(payload any) (policyelements.SessionInvocationOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.SessionInvocationOutcome:
		return value, true
	case *policyelements.SessionInvocationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.SessionInvocationOutcome{}, false
}

func semanticAdmissionOutcomePayload(payload any) (policyelements.SemanticAdmissionOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.SemanticAdmissionOutcome:
		return value, true
	case *policyelements.SemanticAdmissionOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.SemanticAdmissionOutcome{}, false
}

func committedActionPayload(payload any) (actionelements.CommittedAction, bool) {
	switch value := payload.(type) {
	case actionelements.CommittedAction:
		return value, true
	case *actionelements.CommittedAction:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.CommittedAction{}, false
}

func canonicalResultPayload(payload any) (actionelements.CanonicalResult, bool) {
	switch value := payload.(type) {
	case actionelements.CanonicalResult:
		return value, true
	case *actionelements.CanonicalResult:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.CanonicalResult{}, false
}

func cognitionOutcomePayload(payload any) (cognitionelements.Outcome, bool) {
	switch value := payload.(type) {
	case cognitionelements.Outcome:
		return value, true
	case *cognitionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Outcome{}, false
}

func scenarioCognitionResultPayload(payload any) (cognitionelements.Result, bool) {
	switch typed := payload.(type) {
	case cognitionelements.Result:
		return typed, true
	case *cognitionelements.Result:
		if typed != nil {
			return *typed, true
		}
	}
	return cognitionelements.Result{}, false
}

func segmentationOutcomePayload(payload any) (interactionelements.SegmentationOutcome, bool) {
	switch typed := payload.(type) {
	case interactionelements.SegmentationOutcome:
		return typed, true
	case *interactionelements.SegmentationOutcome:
		if typed != nil {
			return *typed, true
		}
	}
	return interactionelements.SegmentationOutcome{}, false
}

func playbackReceiptPayload(payload any) (speechelements.PlaybackReceipt, bool) {
	switch typed := payload.(type) {
	case speechelements.PlaybackReceipt:
		return typed, true
	case *speechelements.PlaybackReceipt:
		if typed != nil {
			return *typed, true
		}
	}
	return speechelements.PlaybackReceipt{}, false
}

func actionOutcomePayload(payload any) (actionelements.Outcome, bool) {
	switch value := payload.(type) {
	case actionelements.Outcome:
		return value, true
	case *actionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.Outcome{}, false
}
