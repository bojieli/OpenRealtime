package perception

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// DefaultNarrationPrompt is the instruction a narrator is given.
//
// It asks for what survives. Images are pruned; the text an observer commits
// is what a model reads an hour later, so it has to carry the state rather
// than describe the picture. It also asks for silence on an unchanged screen,
// because a narrator that says "the same page is still open" every third of a
// second fills the trajectory with nothing.
const DefaultNarrationPrompt = "Describe what this screen shows, as a short factual note for an agent that cannot see it.\n\n" +
	"State what is on screen and what a person could do next: the application, the visible state, any dialog, error, or confirmation, and the text of anything that asks for a decision. Quote on-screen text exactly when it carries a value, an identifier, or an amount.\n\n" +
	"Two or three sentences at most. Do not speculate about what is not visible, do not describe layout or colour for its own sake, and do not address the user. If nothing meaningful has changed, reply with exactly: no change."

// Image is one frame handed to a vision model.
type Image struct {
	Bytes    []byte
	MIMEType string
	Width    int
	Height   int
}

// Vision describes images.
//
// It is deliberately narrower than continuation.Provider. A narrator does not
// continue the trajectory, does not stream, and cannot emit a tool call, and
// giving it the provider interface would make all three possible by accident -
// the same reasoning that keeps policy models on their own contract.
type Vision interface {
	Name() string
	Describe(ctx context.Context, images []Image, prompt string) (string, error)
}

// ModelNarrator narrates with a vision model.
//
// The narrator is a composition slot, not a fixed component. The primary
// configuration is the session's own model narrating as a side-output, which
// is what the measured result used and what avoids putting a second model in
// the loop. But that presumes the foreground model can see, and a full-duplex
// binding breaks the presumption outright - so a dedicated vision-language
// model is a supported alternative rather than a fallback nobody planned for.
// Which one is in use is a session-level composition and a measurable factor.
type ModelNarrator struct {
	vision Vision
	prompt string
	label  string

	narrations atomic.Uint64
	suppressed atomic.Uint64
}

// NarratorConfig configures a model narrator.
type NarratorConfig struct {
	Vision Vision
	// Prompt overrides the shipped narration instruction.
	Prompt string
	// Label distinguishes a session narrator from a dedicated one in reports.
	// It records the composition, not the model.
	Label string
}

// NewSessionNarrator narrates with the session's own model.
func NewSessionNarrator(vision Vision) (*ModelNarrator, error) {
	return NewNarrator(NarratorConfig{Vision: vision, Label: "session"})
}

// NewDedicatedNarrator narrates with a separate vision-language model.
func NewDedicatedNarrator(vision Vision) (*ModelNarrator, error) {
	return NewNarrator(NarratorConfig{Vision: vision, Label: "dedicated"})
}

// NewNarrator creates a narrator.
func NewNarrator(config NarratorConfig) (*ModelNarrator, error) {
	if config.Vision == nil {
		return nil, errors.New("a narrator requires a vision model")
	}
	if strings.TrimSpace(config.Prompt) == "" {
		config.Prompt = DefaultNarrationPrompt
	}
	if strings.TrimSpace(config.Label) == "" {
		config.Label = "model"
	}
	return &ModelNarrator{vision: config.Vision, prompt: config.Prompt, label: config.Label}, nil
}

// Name reports the composition and the model behind it.
func (narrator *ModelNarrator) Name() string {
	return narrator.label + ":" + narrator.vision.Name()
}

// Narrate describes the frames.
func (narrator *ModelNarrator) Narrate(
	ctx context.Context, frames []Frame, _ trajectory.Snapshot,
) (string, error) {
	if len(frames) == 0 {
		return "", nil
	}
	images := make([]Image, 0, len(frames))
	for _, frame := range frames {
		if frame.Kind != FrameImage {
			continue
		}
		images = append(images, Image{
			Bytes: frame.Image, MIMEType: frame.MIMEType, Width: frame.Width, Height: frame.Height,
		})
	}
	if len(images) == 0 {
		return "", nil
	}
	text, err := narrator.vision.Describe(ctx, images, narrator.prompt)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	// "No change" is the narrator declining, and it must not become a
	// trajectory item: an observation saying nothing happened is worse than no
	// observation, because a model will try to make sense of it.
	if text == "" || strings.EqualFold(strings.TrimRight(text, "."), "no change") {
		narrator.suppressed.Add(1)
		return "", nil
	}
	narrator.narrations.Add(1)
	return text, nil
}

// NarratorMetrics reports how often the narrator declined, which is part of
// what makes an idle source cheap.
type NarratorMetrics struct {
	Narrations uint64 `json:"narrations"`
	Suppressed uint64 `json:"suppressed"`
}

func (narrator *ModelNarrator) Metrics() NarratorMetrics {
	return NarratorMetrics{
		Narrations: narrator.narrations.Load(), Suppressed: narrator.suppressed.Load(),
	}
}

// StaticNarrator returns fixed text. It exists for tests and for the
// keyframe-only measurement level, where the question is what images are worth
// without narration.
type StaticNarrator struct {
	Text string
}

func (narrator StaticNarrator) Name() string { return "static" }

func (narrator StaticNarrator) Narrate(context.Context, []Frame, trajectory.Snapshot) (string, error) {
	return narrator.Text, nil
}

// ObserverSetSpec names a measured observer composition, so a report can state
// the level rather than a list of constructor arguments.
type ObserverSetSpec string

const (
	// SetAudioOnly is the reference: voice and nothing else.
	SetAudioOnly ObserverSetSpec = "audio"
	// SetAudioVideo adds the video observer.
	SetAudioVideo ObserverSetSpec = "audio+video"
	// SetVideoOnly removes the audio observer, for tasks with no speech.
	SetVideoOnly ObserverSetSpec = "video"
)

// ParseObserverSet validates a configured level of factor F3.
func ParseObserverSet(value string) (ObserverSetSpec, error) {
	switch spec := ObserverSetSpec(strings.ToLower(strings.TrimSpace(value))); spec {
	case "", SetAudioOnly:
		return SetAudioOnly, nil
	case SetAudioVideo, SetVideoOnly:
		return spec, nil
	default:
		return "", fmt.Errorf("observer set must be audio, audio+video, or video, got %q", value)
	}
}

// ComponentSpec names a measured observer component composition - factor F7.
//
// It exists because the bundle is not uniformly good: at least one strong
// model regresses when handed the keyframe stream, through image-token
// dilution. Shipping a default without measuring its components per model
// would repeat a mistake the evidence has already identified.
type ComponentSpec string

const (
	ComponentKeyframeNarration ComponentSpec = "keyframe+narration"
	ComponentNarrationOnly     ComponentSpec = "narration"
	ComponentKeyframeOnly      ComponentSpec = "keyframe"
)

// ParseComponents validates a configured level of factor F7.
func ParseComponents(value string) (ComponentSpec, error) {
	switch spec := ComponentSpec(strings.ToLower(strings.TrimSpace(value))); spec {
	case "", ComponentNarrationOnly:
		return ComponentNarrationOnly, nil
	case ComponentKeyframeNarration, ComponentKeyframeOnly:
		return spec, nil
	default:
		return "", fmt.Errorf("observer components must be keyframe+narration, narration, or keyframe, got %q", value)
	}
}
