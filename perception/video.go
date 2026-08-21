package perception

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Retainer stores frame bytes outside the trajectory and returns the handle
// that names them. It is the session's media store; the observer takes it as
// an interface so a test can retain nothing.
type Retainer interface {
	Retain(trajectory.MediaRef, []byte) (trajectory.MediaRef, error)
}

// VideoConfig configures the video observer.
type VideoConfig struct {
	// Name defaults to "video".
	Name string
	// Sources restricts the observer to named sources. Empty accepts every
	// image frame, which is the ordinary single-screen case.
	Sources []string
	// Cadence is the minimum interval between extractions. Zero selects 333 ms,
	// roughly three frames a second, which is the rate the measured result
	// used and enough for a screen a person is working on.
	Cadence time.Duration
	// ChangeThreshold is the fraction of the downscaled image that must differ
	// before a frame is worth narrating, in [0,1]. Zero selects 0.02.
	ChangeThreshold float64
	// Narrator turns admitted frames into persistent text. Without one the
	// observer is inert: keyframes with no narration are the configuration the
	// evidence found weakest, so it is not something to fall into by omission.
	Narrator Narrator
	// Retainer stores admitted frames so an observation can reference them.
	// Without one, observations carry text and no media, which is the
	// narration-only configuration.
	Retainer Retainer
	// AttachKeyframes controls whether admitted frames are retained and
	// referenced. It is a measured factor rather than an assumption: at least
	// one strong model regresses when handed a keyframe stream, through
	// image-token dilution.
	AttachKeyframes bool
}

// VideoObserver watches a video source and commits what changed.
//
// The gate is in two stages, and the split is honest about what "costs almost
// nothing" can actually mean. Gate itself does no decoding: it rejects a frame
// that is byte-identical to the last admitted one and enforces the sampling
// interval, which is the case that matters because a static screen is most of
// the time - the measured decomposition found roughly six steps in ten fully
// idle. Only a frame that survives that is decoded and compared pixel by
// pixel, inside Observe, where I/O is expected.
type VideoObserver struct {
	config VideoConfig

	mu            sync.Mutex
	lastAdmitted  []byte
	lastSignature []uint8
	lastAdmitNS   uint64
	frames        uint64
	admitted      uint64
	narrations    uint64
	revision      uint64
}

// NewVideoObserver creates the observer.
func NewVideoObserver(config VideoConfig) (*VideoObserver, error) {
	if strings.TrimSpace(config.Name) == "" {
		config.Name = "video"
	}
	if config.Cadence <= 0 {
		config.Cadence = 333 * time.Millisecond
	}
	if config.ChangeThreshold <= 0 {
		config.ChangeThreshold = 0.02
	}
	if config.ChangeThreshold > 1 {
		return nil, errors.New("the change threshold is a fraction and cannot exceed one")
	}
	if config.Narrator == nil {
		return nil, errors.New("a video observer requires a narrator: narration is the value, not keyframe selection")
	}
	if config.AttachKeyframes && config.Retainer == nil {
		return nil, errors.New("attaching keyframes requires a media store to retain them")
	}
	return &VideoObserver{config: config}, nil
}

func (observer *VideoObserver) Name() string { return observer.config.Name }

func (observer *VideoObserver) Cadence() time.Duration { return observer.config.Cadence }

func (observer *VideoObserver) Accepts(frame Frame) bool {
	if frame.Kind != FrameImage {
		return false
	}
	return len(observer.config.Sources) == 0 || slices.Contains(observer.config.Sources, frame.Source)
}

// Gate is the cheap half: no decoding, no I/O, no allocation beyond a compare.
func (observer *VideoObserver) Gate(frame Frame) bool {
	if len(frame.Image) == 0 {
		return false
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.frames++
	if observer.lastAdmitNS != 0 && frame.CapturedNS != 0 &&
		frame.CapturedNS-observer.lastAdmitNS < uint64(observer.config.Cadence.Nanoseconds()) {
		return false
	}
	// A screen that has not changed usually re-encodes to identical bytes, so
	// this rejects the common idle case for the price of a comparison.
	if len(observer.lastAdmitted) == len(frame.Image) && slices.Equal(observer.lastAdmitted, frame.Image) {
		return false
	}
	return true
}

// Observe decodes what the gate admitted, checks whether it actually changed,
// and narrates it if it did.
func (observer *VideoObserver) Observe(ctx context.Context, frames []Frame) ([]Observation, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	// Only the newest frame matters. Narrating a burst frame by frame would
	// spend the budget on intermediate states nobody needs, and the persistent
	// text is about what the screen shows now.
	frame := frames[len(frames)-1]
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	signature, err := signatureOf(frame.Image)
	if err != nil {
		return nil, fmt.Errorf("decode %s frame: %w", frame.Source, err)
	}

	observer.mu.Lock()
	previous := observer.lastSignature
	changed := changedFraction(previous, signature)
	if previous != nil && changed < observer.config.ChangeThreshold {
		observer.lastAdmitted = slices.Clone(frame.Image)
		observer.mu.Unlock()
		return nil, nil
	}
	observer.lastSignature = signature
	observer.lastAdmitted = slices.Clone(frame.Image)
	observer.lastAdmitNS = frame.CapturedNS
	observer.admitted++
	observer.revision++
	revision := observer.revision
	observer.mu.Unlock()

	text, err := observer.config.Narrator.Narrate(ctx, []Frame{frame}, trajectory.Snapshot{})
	if err != nil {
		return nil, fmt.Errorf("narrate %s frame: %w", frame.Source, err)
	}
	if strings.TrimSpace(text) == "" {
		// Nothing worth saying is a normal outcome for a gate that admitted a
		// change the narrator judged uninteresting.
		return nil, nil
	}
	observer.mu.Lock()
	observer.narrations++
	observer.mu.Unlock()

	observation := Observation{
		Text: text, Observer: observer.config.Name, Source: frame.Source,
		Authority: trajectory.AuthorityObserver, Revision: revision, Final: true,
		OccurredNS: frame.CapturedNS,
	}
	if observer.config.AttachKeyframes {
		reference, err := observer.config.Retainer.Retain(trajectory.MediaRef{
			MIMEType: frame.MIMEType, Source: frame.Source,
			Width: frame.Width, Height: frame.Height, CapturedNS: frame.CapturedNS,
		}, frame.Image)
		if err != nil {
			return nil, fmt.Errorf("retain %s keyframe: %w", frame.Source, err)
		}
		observation.Media = []trajectory.MediaRef{reference}
	}
	return []Observation{observation}, nil
}

// Flush has nothing to complete: a video observer commits each admitted frame
// as it goes, and there is no in-flight state to finish.
func (observer *VideoObserver) Flush(context.Context) ([]Observation, error) { return nil, nil }

// Reset forgets what the screen looked like, so the next frame is treated as
// new. A session boundary should not inherit a previous session's screen.
func (observer *VideoObserver) Reset() {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.lastAdmitted, observer.lastSignature, observer.lastAdmitNS = nil, nil, 0
}

// VideoMetrics is what the efficiency gates measure.
type VideoMetrics struct {
	Frames     uint64 `json:"frames"`
	Admitted   uint64 `json:"admitted"`
	Narrations uint64 `json:"narrations"`
}

// Metrics reports how much of the stream the gate discarded, which is the
// number the efficiency gate for an idle source is stated against.
func (observer *VideoObserver) Metrics() VideoMetrics {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return VideoMetrics{
		Frames: observer.frames, Admitted: observer.admitted, Narrations: observer.narrations,
	}
}

// signatureSize is the edge of the downscaled comparison grid. It is small on
// purpose: the question is "did anything meaningful change", not "what
// changed", and a 32x32 luminance grid answers it in microseconds.
const signatureSize = 32

func signatureOf(encoded []byte) ([]uint8, error) {
	decoded, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	bounds := decoded.Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		return nil, errors.New("image has no pixels")
	}
	signature := make([]uint8, signatureSize*signatureSize)
	for row := 0; row < signatureSize; row++ {
		for column := 0; column < signatureSize; column++ {
			x := bounds.Min.X + column*bounds.Dx()/signatureSize
			y := bounds.Min.Y + row*bounds.Dy()/signatureSize
			red, green, blue, _ := decoded.At(x, y).RGBA()
			// Rec. 601 luma, which is what a person's sense of "the screen
			// changed" tracks better than any single channel.
			luma := (299*uint32(red>>8) + 587*uint32(green>>8) + 114*uint32(blue>>8)) / 1000
			signature[row*signatureSize+column] = uint8(luma)
		}
	}
	return signature, nil
}

// changedFraction is the share of cells that moved by more than a just-visible
// step. A threshold on the count rather than on the total difference is what
// keeps a slow global fade from reading as a change while a small dialog
// appearing does.
func changedFraction(previous, current []uint8) float64 {
	if len(previous) != len(current) || len(current) == 0 {
		return 1
	}
	const justVisible = 8
	changed := 0
	for index := range current {
		delta := int(current[index]) - int(previous[index])
		if delta < 0 {
			delta = -delta
		}
		if delta > justVisible {
			changed++
		}
	}
	return float64(changed) / float64(len(current))
}

var _ Observer = (*VideoObserver)(nil)
