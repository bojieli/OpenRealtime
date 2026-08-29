// Package video provides graph-native ingress and observation-cadence policy
// for camera, screen, and general video sources. It deliberately performs no
// narration and chooses no agent response policy: its raw-image output is the
// existing image.FrameBatch contract consumed by perception.VisualObserver.
package video

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const (
	frameIngressRuntimeID = "builtin://openrealtime/elements/video.FrameIngress"
	policyRuntimeID       = "builtin://openrealtime/elements/video.AdaptiveObservation"
	implementationRev     = "implementation:1"
)

var (
	sourceStartType = element.Trigger(element.Named("video.SourceStart"))
	inlineFrameType = element.Stream(element.Named("video.InlineFrame"))
	frameRefType    = element.Stream(element.Named("video.FrameReference"))
	timingTickType  = element.Trigger(element.Named("timing.Tick"))
	refreshType     = element.Trigger(element.Named("video.Refresh"))
	sourceEndType   = element.Trigger(element.Named("video.SourceEnd"))
	cancelType      = element.Interrupt(element.Named("video.StreamID"))

	imageBatchType     = element.Trigger(element.Named("image.FrameBatch"))
	referenceBatchType = element.Trigger(element.Named("video.ReferenceBatch"))
	visualRefreshType  = element.Trigger(element.Named("image.Refresh"))
	visualCloseType    = element.Trigger(element.Named("image.SourceClose"))
	visualCancelType   = element.Interrupt(element.Named("image.StreamID"))

	policyStateType   = element.State(element.Named("video.ObservationPolicyState"))
	decisionType      = element.Event(element.Named("video.ObservationDecision"))
	policyOutcomeType = element.Event(element.Named("video.PolicyOutcome"))
)

func SourceStartType() element.Type    { return sourceStartType.Clone() }
func InlineFrameType() element.Type    { return inlineFrameType.Clone() }
func FrameReferenceType() element.Type { return frameRefType.Clone() }
func TimingTickType() element.Type     { return timingTickType.Clone() }
func RefreshType() element.Type        { return refreshType.Clone() }
func SourceEndType() element.Type      { return sourceEndType.Clone() }
func CancelType() element.Type         { return cancelType.Clone() }
func ImageBatchType() element.Type     { return imageBatchType.Clone() }
func ReferenceBatchType() element.Type { return referenceBatchType.Clone() }
func PolicyStateType() element.Type    { return policyStateType.Clone() }
func DecisionType() element.Type       { return decisionType.Clone() }
func PolicyOutcomeType() element.Type  { return policyOutcomeType.Clone() }

type SourceKind string

const (
	SourceCamera SourceKind = "camera"
	SourceScreen SourceKind = "screen"
	SourceVideo  SourceKind = "video"
)

func (kind SourceKind) validate() error {
	switch kind {
	case SourceCamera, SourceScreen, SourceVideo:
		return nil
	default:
		return fmt.Errorf("unsupported video source kind %q", kind)
	}
}

type CadenceMode string

const (
	CadenceFixed    CadenceMode = "fixed"
	CadenceAdaptive CadenceMode = "adaptive"
	CadenceManual   CadenceMode = "manual"
)

func (mode CadenceMode) validate() error {
	switch mode {
	case CadenceFixed, CadenceAdaptive, CadenceManual:
		return nil
	default:
		return fmt.Errorf("unsupported video cadence mode %q", mode)
	}
}

// SourceStart establishes the exact source generation accepted by one policy
// instance. A later generation must increase SourceRevision. OpenedNS is
// source-monotonic time and is the initial cadence anchor.
type SourceStart struct {
	Source               string     `json:"source"`
	StreamID             string     `json:"stream_id"`
	Kind                 SourceKind `json:"kind"`
	SourceRevision       uint64     `json:"source_revision"`
	OpenedNS             uint64     `json:"opened_ns"`
	NonBackpressurable   bool       `json:"non_backpressurable,omitempty"`
	ExpectedMIMEType     string     `json:"expected_mime_type,omitempty"`
	ExternalReferenceURI string     `json:"external_reference_uri,omitempty"`
}

// InlineFrame carries one immutable image frame. The wrapper binds the frame
// to a stream generation; Frame.Source, Frame.Index, and Frame.CapturedNS are
// checked again rather than trusted through duck typing.
type InlineFrame struct {
	StreamID       string               `json:"stream_id"`
	SourceRevision uint64               `json:"source_revision"`
	Frame          coreperception.Frame `json:"frame"`
}

// FrameReference is a capability-free pointer to media held elsewhere. It is
// emitted on a distinct typed output and never silently promoted into raw
// image bytes or observer-authority text.
type FrameReference struct {
	Source         string `json:"source"`
	StreamID       string `json:"stream_id"`
	SourceRevision uint64 `json:"source_revision"`
	Index          uint64 `json:"index"`
	CapturedNS     uint64 `json:"captured_ns"`
	Reference      string `json:"reference"`
	MIMEType       string `json:"mime_type"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	Bytes          int    `json:"bytes,omitempty"`
	// Fingerprint is a source-authored immutable revision/hash token. Adaptive
	// cadence compares it for equality; it is never interpreted as authority.
	Fingerprint string `json:"fingerprint"`
}

// TimingTick is the only input that advances fixed/adaptive cadence. The
// element creates no timer and never consults wall time for policy decisions.
type TimingTick struct {
	Source         string `json:"source"`
	StreamID       string `json:"stream_id"`
	SourceRevision uint64 `json:"source_revision"`
	NowNS          uint64 `json:"now_ns"`
}

// Refresh arms one forced observation. It does not bypass explicit timing:
// the next causally later TimingTick performs the observation. The separate
// VisualRefresh output lets an existing VisualObserver arm its own content
// gate before that later batch arrives.
type Refresh struct {
	Source         string `json:"source"`
	StreamID       string `json:"stream_id"`
	SourceRevision uint64 `json:"source_revision"`
	RequestedNS    uint64 `json:"requested_ns"`
	Reason         string `json:"reason,omitempty"`
}

type SourceEnd struct {
	Source         string `json:"source"`
	StreamID       string `json:"stream_id"`
	SourceRevision uint64 `json:"source_revision"`
	EndedNS        uint64 `json:"ended_ns"`
	Reason         string `json:"reason,omitempty"`
}

type Cancel struct {
	Source         string `json:"source"`
	StreamID       string `json:"stream_id"`
	SourceRevision uint64 `json:"source_revision"`
	CanceledNS     uint64 `json:"canceled_ns"`
	Reason         string `json:"reason,omitempty"`
}

type ReferenceBatch struct {
	StreamID   string           `json:"stream_id"`
	References []FrameReference `json:"references"`
}

type DecisionKind string

const (
	DecisionObserve DecisionKind = "observe"
	DecisionSkip    DecisionKind = "skip"
)

type ObservationDecision struct {
	Kind           DecisionKind `json:"kind"`
	Mode           CadenceMode  `json:"mode"`
	Reason         string       `json:"reason"`
	Source         string       `json:"source"`
	StreamID       string       `json:"stream_id"`
	SourceRevision uint64       `json:"source_revision"`
	FrameKind      string       `json:"frame_kind,omitempty"`
	FrameIndex     uint64       `json:"frame_index,omitempty"`
	FrameItemID    string       `json:"frame_item_id,omitempty"`
	TriggerItemID  string       `json:"trigger_item_id"`
	AtNS           uint64       `json:"at_ns"`
	ChangeScore    float64      `json:"change_score,omitempty"`
	NextDueNS      uint64       `json:"next_due_ns,omitempty"`
	Forced         bool         `json:"forced,omitempty"`
}

type PolicyPhase string

const (
	PhaseIdle     PolicyPhase = "idle"
	PhaseWatching PolicyPhase = "watching"
	PhaseEnded    PolicyPhase = "ended"
	PhaseCanceled PolicyPhase = "canceled"
)

// ObservationPolicyState is payload-free, bounded inspection state. It never
// exposes raw bytes, an external reference, or a media capability.
type ObservationPolicyState struct {
	Sequence              uint64      `json:"sequence"`
	Phase                 PolicyPhase `json:"phase"`
	Mode                  CadenceMode `json:"mode"`
	Source                string      `json:"source"`
	StreamID              string      `json:"stream_id,omitempty"`
	SessionID             string      `json:"session_id,omitempty"`
	SourceRevision        uint64      `json:"source_revision,omitempty"`
	SourceStartItemID     string      `json:"source_start_item_id,omitempty"`
	LatestKind            string      `json:"latest_kind,omitempty"`
	LatestItemID          string      `json:"latest_item_id,omitempty"`
	LatestIndex           uint64      `json:"latest_index,omitempty"`
	LatestCapturedNS      uint64      `json:"latest_captured_ns,omitempty"`
	LastObservedIndex     uint64      `json:"last_observed_index,omitempty"`
	LastObservedNS        uint64      `json:"last_observed_ns,omitempty"`
	LastTickNS            uint64      `json:"last_tick_ns,omitempty"`
	NextDueNS             uint64      `json:"next_due_ns,omitempty"`
	ChangeScore           float64     `json:"change_score,omitempty"`
	RefreshArmed          bool        `json:"refresh_armed,omitempty"`
	AcceptedFrames        uint64      `json:"accepted_frames"`
	SupersededFrames      uint64      `json:"superseded_frames"`
	RejectedFrames        uint64      `json:"rejected_frames"`
	Observations          uint64      `json:"observations"`
	SkippedTicks          uint64      `json:"skipped_ticks"`
	TerminalTombstones    int         `json:"terminal_tombstones"`
	TerminalMemoryLimit   int         `json:"terminal_memory_limit"`
	MaxRetainedFrameBytes int         `json:"max_retained_frame_bytes"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type PolicyOutcome struct {
	Kind           OutcomeKind `json:"kind"`
	Operation      string      `json:"operation"`
	Source         string      `json:"source,omitempty"`
	StreamID       string      `json:"stream_id,omitempty"`
	SourceRevision uint64      `json:"source_revision,omitempty"`
	FrameIndex     uint64      `json:"frame_index,omitempty"`
	Code           string      `json:"code,omitempty"`
	Message        string      `json:"message,omitempty"`
	Terminal       bool        `json:"terminal"`
}

func canonicalID(value, field string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s is required and must be canonical", field)
	}
	return value, nil
}

func validateInlineFrame(value InlineFrame) error {
	if _, err := canonicalID(value.StreamID, "stream_id"); err != nil {
		return err
	}
	if value.SourceRevision == 0 {
		return errors.New("source_revision must be positive")
	}
	if err := value.Frame.Validate(); err != nil {
		return err
	}
	if value.Frame.Kind != coreperception.FrameImage {
		return errors.New("video cadence accepts only image frames")
	}
	if len(value.Frame.PCM16LE) != 0 || value.Frame.SampleRateHz != 0 || value.Frame.SampleOffset != 0 {
		return errors.New("video image frame contains audio fields")
	}
	return nil
}

// The compile-time references below ensure that a descriptor drift in the
// existing observer payload structs is caught where this adapter is built.
var (
	_ = perceptionelements.ImageBatch{}
	_ = perceptionelements.VisualRefresh{}
	_ = perceptionelements.VisualSourceClose{}
	_ = perceptionelements.VisualCancel{}
)
