package perception

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/admission"
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

// ActionableNarrationPrompt is the narration a computer-use session wants.
//
// It asks for the same persistent text as the default prompt and adds the one
// thing an agent cannot get any other way: where the controls are. That is not
// a workaround for weak models. Narration is the value precisely because it
// turns a screen into something that survives and can be acted on, and a
// description that says a Confirm button exists without saying where it is has
// done half the job - the agent is left inferring a coordinate from pixels,
// which is a different and much harder skill than reading a screen.
//
// Positions are stated in the source's own coordinate space, which is the
// space computer-use actions are expressed in, so no conversion is needed
// anywhere.
const ActionableNarrationPrompt = "Describe this screen for an agent that cannot see it and must act on it.\n\n" +
	"State what is on screen: the application, the visible state, any dialog, error, or confirmation, and " +
	"the text of anything that asks for a decision. Quote on-screen text exactly when it carries a value, " +
	"an identifier, or an amount.\n\n" +
	"Then list every control a person could click, one per line, as:\n" +
	"  CONTROL: <label> at (<x>, <y>)\n" +
	"where x and y locate the centre of the control on a 0 to 1000 scale across the image: x=0 is the " +
	"left edge, x=1000 the right edge, y=0 the top, y=1000 the bottom. Use that scale and not pixels - " +
	"the image you are shown may have been resized, and a fraction of the image is the same wherever it " +
	"was resized to. Estimate as accurately as you can; an agent will click exactly there.\n\n" +
	"Do not speculate about what is not visible. If nothing meaningful has changed, reply with exactly: no change."

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
	vision   Vision
	prompt   string
	label    string
	governor *admission.Governor
	class    admission.Class
	deadline time.Duration

	narrations atomic.Uint64
	suppressed atomic.Uint64
	unadmitted atomic.Uint64
}

// NarratorConfig configures a model narrator.
type NarratorConfig struct {
	Vision Vision
	// Prompt overrides the shipped narration instruction.
	Prompt string
	// Label distinguishes a session narrator from a dedicated one in reports.
	// It records the composition, not the model.
	Label string
	// Governor admits narration against the same compute budget as everything
	// else. Video narration is a new class competing for the same GPU as the
	// recogniser, the fast model, and the synthesiser, so it belongs under the
	// same governor rather than beside it - a narrator with its own private
	// budget is a narrator that can starve the voice.
	//
	// Nil means unadmitted, which is correct for a deployment whose vision
	// model is hosted elsewhere and therefore competes for nothing local.
	Governor *admission.Governor
	// Class defaults to background: a screen description is worth having and
	// never worth delaying a spoken turn for.
	Class admission.Class
	// Deadline bounds how long a narration may wait for capacity. Zero selects
	// two seconds; a description of a screen that has since changed is not
	// worth the tokens.
	Deadline time.Duration
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
	if config.Class == 0 {
		config.Class = admission.ClassBackground
	}
	if config.Deadline <= 0 {
		config.Deadline = 2 * time.Second
	}
	return &ModelNarrator{
		vision: config.Vision, prompt: config.Prompt, label: config.Label,
		governor: config.Governor, class: config.Class, deadline: config.Deadline,
	}, nil
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
	if narrator.governor != nil {
		lease, err := narrator.governor.Acquire(ctx, admission.Request{
			Class: narrator.class, Cost: 1, Preemptible: true,
			Deadline: time.Now().Add(narrator.deadline), Label: "narration",
		})
		if err != nil {
			// Narration that cannot get compute is narration that did not
			// happen. It is not a session failure: the screen will be sampled
			// again, and describing a stale one would be worse than silence.
			narrator.unadmitted.Add(1)
			return "", nil
		}
		defer lease.Release()
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
	// Unadmitted counts narrations the governor refused. A number that climbs
	// says the deployment is trying to see more than its GPU can describe.
	Unadmitted uint64 `json:"unadmitted"`
}

func (narrator *ModelNarrator) Metrics() NarratorMetrics {
	return NarratorMetrics{
		Narrations: narrator.narrations.Load(), Suppressed: narrator.suppressed.Load(),
		Unadmitted: narrator.unadmitted.Load(),
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
