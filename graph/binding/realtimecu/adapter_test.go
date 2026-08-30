package realtimecu

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

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
