// Package perception turns the world into sparse, persistent text.
//
// One mechanism generalises across every stream, and it is why video is an
// addition to this system rather than a second architecture:
//
//	An observer converts a continuous stream into sparse, persistent text
//	appended to the trajectory, behind a gate that costs almost nothing and
//	produces nothing on unchanged input.
//
// The audio path was already this shape without naming it: an acoustic gate,
// buffered frames, a streaming recogniser, typed revisions, canonical
// observations. Naming it is what lets a video observer be a peer rather than
// a special case.
//
// Two findings are binding constraints on the contract. Narration is the
// value, not keyframe selection - persistent text is what survives after
// images are pruned, so Observe returns text and images are an optional
// attachment referenced by handle. And components must be selected per model,
// because at least one strong model regresses when handed a keyframe stream
// through image-token dilution; so observers are independently switchable per
// session rather than bundled.
package perception

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// FrameKind distinguishes what a frame carries.
type FrameKind string

const (
	FrameAudio FrameKind = "audio"
	FrameImage FrameKind = "image"
)

// Frame is one unit of raw input.
//
// Audio and image share one type deliberately. The Observer contract is the
// same for both, and two frame types would make it two contracts that happened
// to be spelled alike.
type Frame struct {
	Kind FrameKind `json:"kind"`
	// Source names the stream: "microphone", "screen", "camera", or an opaque
	// client-declared identifier. Screen and camera are simultaneously live and
	// semantically different - you act on the screen, you observe the camera -
	// so this is never optional.
	Source string `json:"source"`
	// CapturedNS is source time, which may precede commit time.
	CapturedNS uint64 `json:"captured_ns"`
	// Index counts frames within a source.
	Index uint64 `json:"index,omitempty"`

	// PCM16LE and SampleRateHz carry audio.
	PCM16LE      []byte `json:"-"`
	SampleRateHz uint32 `json:"sample_rate_hz,omitempty"`
	// SampleOffset is the absolute position of this audio in its source.
	SampleOffset uint64 `json:"sample_offset,omitempty"`

	// Image, MIMEType, Width, and Height carry a still image.
	Image    []byte `json:"-"`
	MIMEType string `json:"mime_type,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// Validate rejects a frame that does not carry what its kind promises.
func (frame Frame) Validate() error {
	if strings.TrimSpace(frame.Source) == "" {
		return errors.New("frame requires a source")
	}
	switch frame.Kind {
	case FrameAudio:
		if len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 || frame.SampleRateHz == 0 {
			return errors.New("audio frame requires non-empty even-length PCM16 and a sample rate")
		}
	case FrameImage:
		if len(frame.Image) == 0 || strings.TrimSpace(frame.MIMEType) == "" {
			return errors.New("image frame requires bytes and a MIME type")
		}
		if frame.Width <= 0 || frame.Height <= 0 {
			return errors.New("image frame requires positive dimensions")
		}
	default:
		return fmt.Errorf("unknown frame kind %q", frame.Kind)
	}
	return nil
}

// Bytes is the frame's payload size, for cost accounting.
func (frame Frame) Bytes() int { return len(frame.PCM16LE) + len(frame.Image) }

// Observation is persistent text committed to the trajectory, plus the
// provenance that decides what it is allowed to do.
//
// Authority is the injection defence. Text an observer read off a screen
// carries observer authority forever: it can be reasoned about and it can
// never become an instruction. Only the audio observer, reporting the human
// participant's own speech, produces user authority.
type Observation struct {
	Text      string               `json:"text"`
	Observer  string               `json:"observer"`
	Source    string               `json:"source,omitempty"`
	Authority trajectory.Authority `json:"authority"`
	// Media are handles to attachments retained outside the trajectory.
	Media []trajectory.MediaRef `json:"media,omitempty"`
	// Revision numbers successive views of the same continuous input.
	Revision uint64 `json:"revision,omitempty"`
	// Supersedes names an earlier revision this observation replaces.
	Supersedes uint64 `json:"supersedes,omitempty"`
	// StableText is the prefix the observer will not revise. It is what an
	// observation policy that admits partials is allowed to admit.
	StableText string `json:"stable_text,omitempty"`
	// Provisional marks an observation that later evidence may replace. The
	// binding's observation policy decides whether provisional observations
	// reach the canonical trajectory at all.
	Provisional bool `json:"provisional,omitempty"`
	// Final marks the terminal observation for a unit of input - the end of an
	// utterance, the close of a video source.
	Final      bool   `json:"final,omitempty"`
	OccurredNS uint64 `json:"occurred_ns,omitempty"`
}

// Validate rejects an observation that could not be committed.
func (observation Observation) Validate() error {
	if strings.TrimSpace(observation.Text) == "" {
		return errors.New("observation requires text")
	}
	if strings.TrimSpace(observation.Observer) == "" {
		return errors.New("observation requires an observer name")
	}
	switch observation.Authority {
	case trajectory.AuthorityUser, trajectory.AuthorityObserver:
	default:
		return fmt.Errorf("observation authority must be user or observer, got %q", observation.Authority)
	}
	if observation.Supersedes != 0 && observation.Supersedes >= observation.Revision {
		return errors.New("observation supersession must name an older revision")
	}
	return nil
}

// Producer renders the trajectory producer for this observation. Provenance is
// derived from authority rather than supplied alongside it, so the two cannot
// disagree.
func (observation Observation) Producer() trajectory.Producer {
	if observation.Authority == trajectory.AuthorityUser {
		return trajectory.Producer{Phase: trajectory.PhaseUser}
	}
	return trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: observation.Observer}
}

// Meta renders the trajectory observation provenance, or nil for plain user
// speech, which is the pre-existing shape of the audio path.
func (observation Observation) Meta() *trajectory.ObservationMeta {
	if observation.Authority == trajectory.AuthorityUser && len(observation.Media) == 0 {
		return nil
	}
	return &trajectory.ObservationMeta{
		Observer: observation.Observer, Source: observation.Source,
		Authority: observation.Authority, Media: slices.Clone(observation.Media),
	}
}

// Observer converts one continuous stream into observations.
//
// Gate must be sub-millisecond and must not perform I/O. It is what makes an
// idle stream nearly free: roughly six steps in ten are fully idle in the
// measured video case, and a gate that cost anything real would spend most of
// its budget on nothing happening.
type Observer interface {
	Name() string
	// Accepts reports whether this observer handles a source at all, so a
	// session with several sources does not hand every frame to every
	// observer.
	Accepts(Frame) bool
	// Gate reports whether a frame carries anything worth extracting. It must
	// be cheap and must not block.
	Gate(Frame) bool
	// Observe extracts persistent text from admitted frames. It may return no
	// observations, which is the normal outcome for input that changed too
	// little to be worth saying anything about.
	Observe(context.Context, []Frame) ([]Observation, error)
	// Flush completes any in-flight observation at a boundary - an endpoint, a
	// source closing, a session ending. It is not in the minimal contract
	// because it is not conceptually necessary; it is here because a streaming
	// recogniser genuinely has an in-flight state that must be finished rather
	// than dropped.
	Flush(context.Context) ([]Observation, error)
	// Cadence is how often Observe should be called while frames are being
	// admitted. Zero means call it as frames arrive.
	Cadence() time.Duration
	// Reset discards per-unit state, so one utterance's recogniser state
	// cannot leak into the next.
	Reset()
}

// Narrator turns admitted frames into the persistent text that survives after
// the images themselves are pruned.
//
// It is a composition slot rather than a fixed component. The primary
// configuration is the session's own model narrating as a side-output, which
// is what the measured result used and what avoids a second model in the loop.
// But that presumes the foreground model can see, and a full-duplex binding
// breaks the presumption outright, so a dedicated vision-language model is a
// supported alternative rather than a fallback nobody planned for.
type Narrator interface {
	Name() string
	Narrate(context.Context, []Frame, trajectory.Snapshot) (string, error)
}

// Set is the observer composition for one session.
//
// Observers are independently switchable, which is what makes the observer set
// a measured factor rather than an assumed bundle.
type Set struct {
	observers []Observer
}

// NewSet composes observers, rejecting duplicate names so a report can name
// exactly which observer produced what.
func NewSet(observers ...Observer) (*Set, error) {
	seen := make(map[string]struct{}, len(observers))
	for _, observer := range observers {
		if observer == nil {
			return nil, errors.New("observer set contains a nil observer")
		}
		if _, duplicate := seen[observer.Name()]; duplicate {
			return nil, fmt.Errorf("duplicate observer %q", observer.Name())
		}
		seen[observer.Name()] = struct{}{}
	}
	return &Set{observers: slices.Clone(observers)}, nil
}

// Observers returns the composed set.
func (set *Set) Observers() []Observer { return slices.Clone(set.observers) }

// Names reports the composition, for evidence and for the health endpoint.
func (set *Set) Names() []string {
	names := make([]string, 0, len(set.observers))
	for _, observer := range set.observers {
		names = append(names, observer.Name())
	}
	return names
}

// For returns the observers that accept a frame.
func (set *Set) For(frame Frame) []Observer {
	var matched []Observer
	for _, observer := range set.observers {
		if observer.Accepts(frame) {
			matched = append(matched, observer)
		}
	}
	return matched
}

// Reset clears per-unit state across the set.
func (set *Set) Reset() {
	for _, observer := range set.observers {
		observer.Reset()
	}
}
