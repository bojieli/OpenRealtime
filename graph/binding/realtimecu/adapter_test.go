package realtimecu

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestTypedTextPreservesSourceTimeOnObservationEnvelope(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		occurredNS uint64
	}{
		{name: "gateway source time", occurredNS: 1_787_856_123_456_789_000},
		{name: "direct caller fallback remains available"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			port := &recordingOutputPort{name: textBoundary, typeName: stateelements.ObservationType()}
			session := &session{
				sessionID: "typed-time-session", text: port,
				revisions: make(map[string]uint64), captured: make(map[string]uint64),
				seenText: make(map[string]struct{}), active: make(map[string]activeCall),
			}
			if err := session.Text(context.Background(), legacy.TextInput{
				ItemID: "typed-item", Role: "user", Text: "click continue",
				OccurredNS: testCase.occurredNS,
			}); err != nil {
				t.Fatal(err)
			}
			envelopes := port.snapshot()
			if len(envelopes) != 1 || envelopes[0].CaptureNS != testCase.occurredNS {
				t.Fatalf("typed observation envelopes = %+v", envelopes)
			}
			observation, ok := envelopes[0].Payload.(perception.Observation)
			if !ok || observation.OccurredNS != testCase.occurredNS ||
				observation.Authority != trajectory.AuthorityUser || !observation.Final {
				t.Fatalf("typed observation = %+v (%T)", envelopes[0].Payload, envelopes[0].Payload)
			}
		})
	}
}

func TestScreenObservationsCausallyConsumeCanonicalClientEffectsInFIFOOrder(t *testing.T) {
	observer := &adapterTestObserver{name: "test-observer"}
	port := &recordingOutputPort{name: videoBoundary, typeName: stateelements.ObservationType()}
	session := &session{
		sessionID: "causal-session", observer: observer,
		config:    PluginConfig{Observer: ObserverPlugin{Name: observer.name}},
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: map[string]activeCall{
			"call-1": {call: trajectory.ToolCall{CallID: "call-1", Name: "computer_click"}},
			"call-2": {call: trajectory.ToolCall{CallID: "call-2", Name: "computer_click"}},
		},
	}
	results := []struct{ callID, itemID string }{
		{callID: "call-1", itemID: "tool-result-trajectory-item-1"},
		{callID: "call-2", itemID: "tool-result-trajectory-item-2"},
	}
	for index, result := range results {
		if err := session.acceptCanonicalResult(context.Background(), element.Envelope{
			Payload: actionelements.CanonicalResult{
				Execution:        actionelements.ExecutionResult{CallID: result.callID, Name: "computer_click"},
				TrajectoryItemID: result.itemID,
				StoreVersion:     uint64(index + 1),
			},
		}); err != nil {
			t.Fatalf("accept canonical result %d: %v", index+1, err)
		}
	}
	consequences := observer.consequenceSnapshot()
	if len(consequences) != 2 || consequences[0].CallID != "call-1" || consequences[1].CallID != "call-2" {
		t.Fatalf("observer consequences = %+v", consequences)
	}
	if err := session.observe(context.Background(), port, perception.Frame{
		Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 100,
		Image: []byte{1}, MIMEType: "image/png", Width: 10, Height: 10,
	}, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	envelopes := port.snapshot()
	if len(envelopes) != 1 ||
		!slices.Equal(envelopes[0].CausalParents, []string{"tool-result-trajectory-item-1"}) {
		t.Fatalf("screen observation causal parents = %+v", envelopes)
	}
	if len(session.pendingConsequences) != 1 ||
		session.pendingConsequences[0].CanonicalResultItemID != "tool-result-trajectory-item-2" {
		t.Fatalf("pending consequences after first screen = %+v", session.pendingConsequences)
	}

	port.clear()
	if err := session.observe(context.Background(), port, perception.Frame{
		Kind: perception.FrameImage, Source: SourceCamera, CapturedNS: 101,
		Image: []byte{2}, MIMEType: "image/png", Width: 10, Height: 10,
	}, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	if envelopes := port.snapshot(); len(envelopes) != 1 || len(envelopes[0].CausalParents) != 0 {
		t.Fatalf("camera observation inherited screen-effect authority: %+v", envelopes)
	}
	if len(session.pendingConsequences) != 1 ||
		session.pendingConsequences[0].CanonicalResultItemID != "tool-result-trajectory-item-2" {
		t.Fatalf("camera consumed pending screen consequence: %+v", session.pendingConsequences)
	}

	port.clear()
	if err := session.observe(context.Background(), port, perception.Frame{
		Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 102,
		Image: []byte{3}, MIMEType: "image/png", Width: 10, Height: 10,
	}, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	if envelopes := port.snapshot(); len(envelopes) != 1 ||
		!slices.Equal(envelopes[0].CausalParents, []string{"tool-result-trajectory-item-2"}) {
		t.Fatalf("second screen did not consume the second effect parent: %+v", envelopes)
	}
	if len(session.pendingConsequences) != 0 {
		t.Fatalf("pending consequences after second screen = %+v", session.pendingConsequences)
	}

	port.clear()
	if err := session.observe(context.Background(), port, perception.Frame{
		Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 103,
		Image: []byte{4}, MIMEType: "image/png", Width: 10, Height: 10,
	}, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	if envelopes := port.snapshot(); len(envelopes) != 1 || len(envelopes[0].CausalParents) != 0 {
		t.Fatalf("ordinary screen cadence repeated a consumed effect parent: %+v", envelopes)
	}
}

func TestVisualConsequenceQueueFailsClosedBeforeObserverAtCapacity(t *testing.T) {
	observer := &adapterTestObserver{name: "test-observer"}
	session := &session{observer: observer, pendingConsequences: make([]VisualConsequence, maximumPendingVisualConsequences)}
	err := session.queueVisualConsequence(context.Background(), VisualConsequence{
		CallID: "call-over-capacity", Name: "computer_click",
		CanonicalResultItemID: "result-over-capacity", TargetSource: SourceScreen, StoreVersion: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "capacity is exhausted") {
		t.Fatalf("queue capacity error = %v", err)
	}
	if consequences := observer.consequenceSnapshot(); len(consequences) != 0 {
		t.Fatalf("capacity-exhausted consequence reached observer: %+v", consequences)
	}
}

func TestEmptyScreenObservationDoesNotConsumeCanonicalClientEffect(t *testing.T) {
	observer := &adapterTestObserver{name: "test-observer", emptyVideos: 1}
	port := &recordingOutputPort{name: videoBoundary, typeName: stateelements.ObservationType()}
	session := &session{
		sessionID: "empty-screen-session", observer: observer,
		config:    PluginConfig{Observer: ObserverPlugin{Name: observer.name}},
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: make(map[string]activeCall),
		pendingConsequences: []VisualConsequence{{
			CanonicalResultItemID: "tool-result-after-empty-screen",
		}},
	}
	frame := perception.Frame{
		Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 100,
		Image: []byte{1}, MIMEType: "image/png", Width: 10, Height: 10,
	}
	if err := session.observe(context.Background(), port, frame, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	if len(port.snapshot()) != 0 || len(session.pendingConsequences) != 1 {
		t.Fatalf("empty screen consumed consequence: envelopes=%+v pending=%+v",
			port.snapshot(), session.pendingConsequences)
	}
	frame.CapturedNS++
	if err := session.observe(context.Background(), port, frame, trajectory.AuthorityObserver); err != nil {
		t.Fatal(err)
	}
	envelopes := port.snapshot()
	if len(envelopes) != 1 ||
		!slices.Equal(envelopes[0].CausalParents, []string{"tool-result-after-empty-screen"}) ||
		len(session.pendingConsequences) != 0 {
		t.Fatalf("nonempty retry did not consume consequence: envelopes=%+v pending=%+v",
			envelopes, session.pendingConsequences)
	}
}

func TestVisualAndAudioAuthorityDriftFailsBeforeGraphIngress(t *testing.T) {
	observer := &adapterTestObserver{name: "test-observer", authority: trajectory.AuthorityUser}
	port := &recordingOutputPort{name: videoBoundary, typeName: stateelements.ObservationType()}
	session := &session{
		sessionID: "authority-session", observer: observer,
		config:    PluginConfig{Observer: ObserverPlugin{Name: observer.name}},
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: make(map[string]activeCall),
	}
	err := session.Video(context.Background(), perception.Frame{
		Kind: perception.FrameImage, Source: SourceCamera, CapturedNS: 100,
		Image: []byte{1}, MIMEType: "image/png", Width: 10, Height: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("camera authority drift error = %v", err)
	}
	if len(port.snapshot()) != 0 {
		t.Fatal("authority-drifted camera observation reached graph ingress")
	}
	if err := session.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: SourceCamera, CapturedNS: 101,
		PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
	}); err == nil {
		t.Fatal("camera source was accepted on the microphone audio operation")
	}
}

type concurrentAdapterObserver struct {
	name         string
	videoStarted chan struct{}
	videoRelease chan struct{}
	start        sync.Once
	audioRev     atomic.Uint64
	videoRev     atomic.Uint64
}

func (observer *concurrentAdapterObserver) Audio(
	_ context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	return []perception.Observation{{
		Text: "heard", Observer: observer.name, Source: frame.Source,
		Authority: trajectory.AuthorityUser, Revision: observer.audioRev.Add(1), Final: true,
	}}, nil
}
func (observer *concurrentAdapterObserver) Video(
	ctx context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	observer.start.Do(func() { close(observer.videoStarted) })
	select {
	case <-observer.videoRelease:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	return []perception.Observation{{
		Text: "saw", Observer: observer.name, Source: frame.Source,
		Authority: trajectory.AuthorityObserver, Revision: observer.videoRev.Add(1), Final: true,
	}}, nil
}
func (*concurrentAdapterObserver) Consequence(context.Context, VisualConsequence) error { return nil }
func (*concurrentAdapterObserver) Close() error                                         { return nil }

func TestSessionBlockedVisionDoesNotStarveAudioObservation(t *testing.T) {
	observer := &concurrentAdapterObserver{
		name: "test-observer", videoStarted: make(chan struct{}), videoRelease: make(chan struct{}),
	}
	session := &session{
		sessionID: "concurrent-media-session", observer: observer,
		config:    PluginConfig{Observer: ObserverPlugin{Name: observer.name}},
		audio:     &recordingOutputPort{name: audioBoundary, typeName: stateelements.ObservationType()},
		video:     &recordingOutputPort{name: videoBoundary, typeName: stateelements.ObservationType()},
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: make(map[string]activeCall),
	}
	videoDone := make(chan error, 1)
	go func() {
		videoDone <- session.Video(t.Context(), perception.Frame{
			Kind: perception.FrameImage, Source: SourceScreen, CapturedNS: 1,
			Image: []byte{1}, MIMEType: "image/jpeg", Width: 1, Height: 1,
		})
	}()
	select {
	case <-observer.videoStarted:
	case <-time.After(time.Second):
		t.Fatal("vision fixture did not block")
	}
	audioDone := make(chan error, 1)
	go func() {
		audioDone <- session.Audio(t.Context(), perception.Frame{
			Kind: perception.FrameAudio, Source: SourceMicrophone, CapturedNS: 2,
			PCM16LE: []byte{1, 0}, SampleRateHz: 8_000,
		})
	}()
	select {
	case err := <-audioDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked vision starved adapter audio commit")
	}
	close(observer.videoRelease)
	if err := <-videoDone; err != nil {
		t.Fatal(err)
	}
}

func TestSessionCancelEmitsOneTypedRequestAndWaitsForExactTerminalOutcome(t *testing.T) {
	port := &recordingOutputPort{name: sessionCancelBoundary, typeName: SessionCancellationType()}
	session := &session{
		sessionID: "adapter-cancellation-session", sessionCancel: port,
		cancelAcks: make(map[string]chan error),
	}
	done := make(chan error, 1)
	go func() { done <- session.Cancel(t.Context(), " stop now ") }()
	request := waitForRecordedEnvelope(t, port, 1)[0]
	payload, ok := sessionCancellationPayload(request.Payload)
	if !ok || payload.SessionID != session.sessionID || payload.Reason != "stop now" ||
		request.SessionID != session.sessionID || request.CancellationScope != session.sessionID ||
		request.Sequence == 0 || request.ItemID == "" {
		t.Fatalf("adapter cancellation request = %+v payload=%+v", request, request.Payload)
	}

	// A terminal-looking outcome addressed to another request must not release
	// this waiter.
	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, "different-request", SessionCancellationCompleted,
		"all_required_acknowledgements",
	)); err != nil {
		t.Fatal(err)
	}
	assertAdapterCancelPending(t, done)
	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, request.ItemID, SessionCancellationProgress, "activation_acknowledged",
	)); err != nil {
		t.Fatal(err)
	}
	assertAdapterCancelPending(t, done)
	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, request.ItemID, SessionCancellationCompleted,
		"all_required_acknowledgements",
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("exact terminal cancellation outcome did not release adapter waiter")
	}
	if len(port.snapshot()) != 1 || len(session.cancelAcks) != 0 {
		t.Fatalf("adapter cancellation fanout/waiters = %d/%d", len(port.snapshot()), len(session.cancelAcks))
	}
}

func TestSessionCancelWaitsBehindEarlierCanonicalObservationCommit(t *testing.T) {
	textPort := &recordingOutputPort{name: textBoundary, typeName: stateelements.ObservationType()}
	cancelPort := &recordingOutputPort{name: sessionCancelBoundary, typeName: SessionCancellationType()}
	session := &session{
		sessionID: "adapter-observation-barrier", text: textPort, sessionCancel: cancelPort,
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: make(map[string]activeCall),
		observationAcks: make(map[string]chan error), cancelAcks: make(map[string]chan error),
	}
	textDone := make(chan error, 1)
	go func() {
		textDone <- session.Text(t.Context(), legacy.TextInput{
			ItemID: "user-before-cancel", Role: "user", Text: "stop after this request",
			OccurredNS: 10,
		})
	}()
	observation := waitForRecordedEnvelope(t, textPort, 1)[0]
	assertAdapterCancelPending(t, textDone)

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- session.Cancel(t.Context(), "stop") }()
	// Cancel takes the same ingress ordering barrier and therefore cannot even
	// publish its request while the earlier user observation lacks canonical
	// commit evidence.
	time.Sleep(10 * time.Millisecond)
	if envelopes := cancelPort.snapshot(); len(envelopes) != 0 {
		t.Fatalf("cancellation overtook uncommitted observation: %+v", envelopes)
	}
	if err := session.acceptObservationOutcome(element.Envelope{
		Type:   stateelements.ObservationCommitOutcomeType(),
		ItemID: observation.ItemID + ":commit", SessionID: session.sessionID, Sequence: 1,
		CausalParents: []string{observation.ItemID},
		Payload: stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationCommitted, TriggerItemID: observation.ItemID,
			TrajectoryItemID: "canonical-user-before-cancel", StoreVersion: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-textDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("text input did not return after exact canonical commit")
	}
	request := waitForRecordedEnvelope(t, cancelPort, 1)[0]
	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, request.ItemID, SessionCancellationCompleted,
		"all_required_acknowledgements",
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not complete after the ingress barrier")
	}
}

func TestObservationCommitRejectionReleasesIngressWithError(t *testing.T) {
	textPort := &recordingOutputPort{name: textBoundary, typeName: stateelements.ObservationType()}
	session := &session{
		sessionID: "adapter-observation-rejection", text: textPort,
		revisions: make(map[string]uint64), captured: make(map[string]uint64),
		seenText: make(map[string]struct{}), active: make(map[string]activeCall),
		observationAcks: make(map[string]chan error),
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Text(t.Context(), legacy.TextInput{
			ItemID: "rejected-user-observation", Role: "user", Text: "reject me",
		})
	}()
	observation := waitForRecordedEnvelope(t, textPort, 1)[0]
	if err := session.acceptObservationOutcome(element.Envelope{
		Type:          stateelements.ObservationCommitOutcomeType(),
		ItemID:        observation.ItemID + ":rejected",
		SessionID:     session.sessionID,
		Sequence:      1,
		CausalParents: []string{observation.ItemID},
		Payload: stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationRejected, TriggerItemID: observation.ItemID,
			Code: "invalid_observation", Message: "fixture rejection",
		},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid_observation") {
			t.Fatalf("observation rejection error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("observation rejection did not release ingress waiter")
	}
	if len(session.observationAcks) != 0 || len(session.seenText) != 0 {
		t.Fatalf("rejected observation left waiter/text state: %d/%d",
			len(session.observationAcks), len(session.seenText))
	}
}

func TestConcurrentSessionCancelDoesNotReportInProgressIntentAsQuiescent(t *testing.T) {
	port := &recordingOutputPort{name: sessionCancelBoundary, typeName: SessionCancellationType()}
	session := &session{
		sessionID: "adapter-concurrent-cancellation", sessionCancel: port,
		cancelAcks: make(map[string]chan error),
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- session.Cancel(t.Context(), "first") }()
	first := waitForRecordedEnvelope(t, port, 1)[0]
	secondDone := make(chan error, 1)
	go func() { secondDone <- session.Cancel(t.Context(), "second") }()
	second := waitForRecordedEnvelope(t, port, 2)[1]

	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, second.ItemID, SessionCancellationIgnored,
		"intent_cancellation_in_progress",
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "still in progress") {
			t.Fatalf("concurrent cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-progress cancellation did not return an explicit retry error")
	}
	assertAdapterCancelPending(t, firstDone)
	if err := session.acceptCancellationOutcome(cancellationAdapterOutcomeEnvelope(
		session.sessionID, first.ItemID, SessionCancellationCompleted,
		"all_required_acknowledgements",
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first cancellation did not await exact completion")
	}
}

func cancellationAdapterOutcomeEnvelope(
	sessionID, requestItemID string, kind SessionCancellationOutcomeKind, code string,
) element.Envelope {
	return element.Envelope{
		Type: SessionCancellationOutcomeType(), ItemID: requestItemID + ":outcome",
		SessionID: sessionID, Sequence: 1,
		Payload: SessionCancellationOutcome{
			Kind: kind, Operation: "complete", RequestItemID: requestItemID,
			SessionID: sessionID, Code: code, FinishedNS: 1,
		},
	}
}

func waitForRecordedEnvelope(
	t *testing.T, port *recordingOutputPort, count int,
) []element.Envelope {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if envelopes := port.snapshot(); len(envelopes) >= count {
			return envelopes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("recorded envelopes = %d, want at least %d", len(port.snapshot()), count)
	return nil
}

func assertAdapterCancelPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("cancellation returned before exact terminal outcome: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
}

type adapterTestObserver struct {
	name         string
	authority    trajectory.Authority
	mu           sync.Mutex
	revisions    map[string]uint64
	consequences []VisualConsequence
	emptyVideos  int
}

func (observer *adapterTestObserver) observe(frame perception.Frame, fallback trajectory.Authority) []perception.Observation {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.revisions == nil {
		observer.revisions = make(map[string]uint64)
	}
	observer.revisions[frame.Source]++
	authority := observer.authority
	if authority == "" {
		authority = fallback
	}
	return []perception.Observation{{
		Text: "observed", Observer: observer.name, Source: frame.Source,
		Authority: authority, Revision: observer.revisions[frame.Source], Final: true,
	}}
}

func (observer *adapterTestObserver) Audio(
	_ context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	return observer.observe(frame, trajectory.AuthorityUser), nil
}
func (observer *adapterTestObserver) Video(
	_ context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	observer.mu.Lock()
	if observer.emptyVideos != 0 {
		observer.emptyVideos--
		observer.mu.Unlock()
		return nil, nil
	}
	observer.mu.Unlock()
	return observer.observe(frame, trajectory.AuthorityObserver), nil
}
func (observer *adapterTestObserver) Consequence(_ context.Context, consequence VisualConsequence) error {
	observer.mu.Lock()
	observer.consequences = append(observer.consequences, consequence)
	observer.mu.Unlock()
	return nil
}
func (observer *adapterTestObserver) consequenceSnapshot() []VisualConsequence {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return slices.Clone(observer.consequences)
}
func (*adapterTestObserver) Close() error { return nil }

type recordingOutputPort struct {
	mu        sync.Mutex
	name      string
	typeName  element.Type
	envelopes []element.Envelope
}

func (port *recordingOutputPort) Name() string       { return port.name }
func (port *recordingOutputPort) Type() element.Type { return port.typeName.Clone() }
func (*recordingOutputPort) Lanes() []element.Sender { return nil }
func (port *recordingOutputPort) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	if err := context.Cause(ctx); err != nil {
		return element.SendResult{}, err
	}
	if err := envelope.ValidateFor(port.typeName); err != nil {
		return element.SendResult{}, err
	}
	port.mu.Lock()
	port.envelopes = append(port.envelopes, envelope.Clone())
	port.mu.Unlock()
	return element.SendResult{Delivered: 1}, nil
}

func (port *recordingOutputPort) snapshot() []element.Envelope {
	port.mu.Lock()
	defer port.mu.Unlock()
	result := make([]element.Envelope, len(port.envelopes))
	for index, envelope := range port.envelopes {
		result[index] = envelope.Clone()
	}
	return result
}

func (port *recordingOutputPort) clear() {
	port.mu.Lock()
	port.envelopes = nil
	port.mu.Unlock()
}

var _ element.OutputPort = (*recordingOutputPort)(nil)
var _ Observer = (*adapterTestObserver)(nil)
