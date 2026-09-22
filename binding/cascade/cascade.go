// Package cascade is the reference binding: the engine owns everything.
//
// A cascade has no interaction capability inside any of its models. The
// recogniser knows nothing about turn-taking, the language model knows nothing
// about the conversation's timing, and the synthesiser knows nothing at all -
// so every bit of responsiveness the system exhibits is manufactured in the
// interaction plane. That makes this binding the honest test of whether the
// control plane works, and it is why it is the default: it is fully local, it
// needs no third-party account, and it is the right first impression for an
// open implementation.
package cascade

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/admission"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/voices"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ObservationPolicy controls which perception revisions may enter the
// canonical trajectory and therefore start authoritative cognition.
//
// It is independent from preparation: speculative work has no speech sink and
// no tool authority, while every observation admitted here may eventually
// produce both.
type ObservationPolicy string

const (
	// ObservationEndpointOnly admits only the terminal revision.
	ObservationEndpointOnly ObservationPolicy = "endpoint-only"
	// ObservationStablePartial also admits a changed, non-empty stable prefix
	// before the endpoint. Eligibility comes only from the recogniser's typed
	// stable/unstable boundary, never from inspecting the words.
	ObservationStablePartial ObservationPolicy = "stable-partial"
)

// ParseObservationPolicy validates a configured level.
func ParseObservationPolicy(value string) (ObservationPolicy, error) {
	switch policy := ObservationPolicy(strings.ToLower(strings.TrimSpace(value))); policy {
	case "", ObservationEndpointOnly:
		return ObservationEndpointOnly, nil
	case ObservationStablePartial:
		return policy, nil
	default:
		return "", errors.New("observation policy must be endpoint-only or stable-partial")
	}
}

// TurnEndEvidence classifies whether a pause ends the user's turn from the
// last seconds of their audio, 16 kHz mono PCM16 ending at the pause.
type TurnEndEvidence interface {
	Name() string
	Evaluate(ctx context.Context, pcm16le []byte) (interaction.AcousticEndpoint, error)
}

// Config is the cascade's component set and its defaults.
type Config struct {
	// Profile names the normalized deployment shape for health and evidence.
	// Empty preserves the pre-profile voice configuration.
	Profile string
	// Perception creates one streaming recogniser per utterance, so
	// recogniser state cannot leak between turns.
	Perception func() (v1.PerceptionProvider, error)
	// PerceptionDescriptor identifies the lazily created recogniser before an
	// utterance starts, for session evidence and controlled experiments.
	PerceptionDescriptor v1.Descriptor
	// Narrator turns a picture a client put into the conversation into text.
	//
	// A video observer has one because a frame nobody describes is a frame
	// only a model that can see will ever know about. A picture attached to a
	// message needs it for exactly the same reason and had nothing: the fast
	// provider was handed the image and everything else in the session was
	// handed the sentence "The user attached an image", including the
	// interaction model, which then decided whether to speak about a screen it
	// had been told nothing about.
	//
	// Nil leaves that sentence in place, which is the honest fallback for a
	// deployment with no vision model configured.
	Narrator perception.Narrator
	// DeciderSees says the interaction model can look at a picture, so frames
	// reach it directly instead of as somebody's description of one.
	//
	// It is a property of the model an operator configured, not something to
	// probe for: a text-only decider handed a screenshot either refuses it or
	// silently ignores it, and both are worse than the description it would
	// otherwise have had.
	DeciderSees bool
	// ProfileTurns logs how long each stage of a turn took.
	//
	// Off by default: it is a line per turn on stderr, which is noise in a
	// deployment and the only way to answer "what was the person waiting
	// through" in a measurement.
	ProfileTurns bool
	// HoldLimit is the longest one utterance may be held open across pauses.
	//
	// The floor's own liveness bound is measured from the last pause, and the
	// pause clock resets whenever the speaker says something new - which is
	// right for "don't interrupt me while I think this through" and lets a
	// speaker who never stops hold the turn for as long as they keep talking.
	// Measured on an interpreting policy that says not to wait for the speaker
	// to finish, the gate opened four seconds in and did not close for the
	// remaining thirty-one: nothing was ever committed, the conversation never
	// advanced, and the agent said nothing at all.
	//
	// Zero selects the same twenty seconds, measured from the first pause the
	// floor declined to end the turn at.
	HoldLimit time.Duration
	// EndpointSilenceMS overrides how much quiet closes an utterance. Zero
	// keeps the recogniser default.
	EndpointSilenceMS int
	// ASRCadence is how often the recogniser is advanced.
	ASRCadence time.Duration
	// HoldingAfter is how long the reasoner may run before the voice says what
	// is happening, in the turn already in progress. Zero leaves the user
	// listening to silence for as long as the reasoner takes.
	HoldingAfter time.Duration

	// Fast is the voice; Slow is the background reasoner. Slow must be
	// configured silent: the cognition engine refuses the pair otherwise.
	Fast          continuation.Provider
	Slow          continuation.Provider
	FastMaxTokens int
	SlowMaxTokens int
	// VisualReflex is an optional, separately configured visual action model.
	// It never replaces Fast: Fast remains the voice, so a voice-only profile
	// and a voice+vision profile use the same runtime and differ only here and
	// in their observer selection.
	VisualReflex          continuation.Provider
	VisualReflexMaxTokens int
	VisualReflexTimeout   time.Duration
	// RequireExplicitVisualAuthority closes the compatibility fallback used
	// when no learned interaction policy is configured. In that mode the
	// visual role runs only for compiler-recognized direct UI commands or
	// explicit future visual monitors; semantic work alone cannot reach a
	// coordinate actor. A configured interaction policy already owns this
	// classification and makes the flag irrelevant.
	RequireExplicitVisualAuthority bool

	// Speech synthesises what the fast provider says.
	Speech v1.StreamingSpeechProvider
	// WordTimings listens to the agent's own synthesised audio and reports
	// where each word sat inside it.
	//
	// It is what turns "the agent was audible for 2.1 seconds" into "the agent
	// said these six words and not the four after them", which is the fact
	// every decision after an interruption actually needs. Nil is a supported
	// deployment: the boundary is then modelled proportionally from the same
	// audio, which is within about a word and is recorded as an estimate
	// rather than as a measurement.
	WordTimings spoken.Aligner
	// WordTimingInterval is how much new audio is worth another listen while
	// an utterance is still being synthesised. Zero selects one second.
	WordTimingInterval time.Duration
	// Voice names the voice that synthesiser was built with. It is reported
	// to clients and cannot be changed per session: a speech plan carries text
	// and nothing else, so the voice is fixed when the provider is created.
	// Naming it here is what lets a session be told the truth about what it is
	// hearing instead of a protocol default nothing uses.
	Voice string
	// FrameDuration is the paced wire frame size.
	FrameDuration time.Duration

	// Voices answers who is speaking, so a voice that is not the person the
	// session is with is not reported as the user. Nil leaves that prior in
	// place.
	//
	// The embedder rather than the recogniser: a recogniser learns whose
	// session it is from the first voice it hears, and one shared by every
	// session learns it once. Held that way it enrolled the first speaker of
	// the first scenario in a suite and reported every user after that as a
	// stranger - which the agent then correctly declined to answer.
	Voices voices.Embedder
	// SpeakerIdentityDescriptor identifies the live adapter used by Voices.
	// Its immutable model artifact remains a deployment pin; this is the
	// independently observed runtime spelling and adapter revision.
	SpeakerIdentityDescriptor v1.Descriptor
	// TurnEnd, when set, is asked at every pause decision whether the recent
	// user audio sounds like a finished turn. Its answer is attached to the
	// decision as interaction.AcousticEndpoint evidence and recorded on the
	// timeline; it ends or holds a turn only if the selected floor's
	// projection reads it (interaction.AcousticProjection). Nil consults
	// nothing.
	TurnEnd TurnEndEvidence
	// Policies is the interaction policy set. A zero value selects the
	// shipped defaults.
	Policies interaction.Policies
	// ManualDeferral optionally replaces the standard client-driven deferral
	// when a session disables server turn detection. Nil preserves the shipped
	// behavior. This is a composition seam, not an implicit profile switch: a
	// caller selecting it is responsible for reporting the policy and ensuring
	// any work it admits without response.create cannot speak.
	ManualDeferral    interaction.Deferral
	ObservationPolicy ObservationPolicy

	// Observers extends the default set with one factory per additional
	// observer. The audio observer is always present. Factories rather than
	// instances because an observer holds per-session state: two sessions
	// sharing one video observer would each see the other's screen as
	// unchanged.
	Observers []perception.Factory

	// DefaultObservers is the binding's documented default set, by name. Empty
	// selects every configured observer. A session that names its own set
	// overrides it, which is what makes the set a per-session factor rather
	// than a property of the process.
	DefaultObservers []string

	// Tools are server-side tools every session declares, on top of whatever a
	// client declares for itself. Computer use arrives this way: a client
	// should not have to know the coordinate space of a browser the server is
	// driving.
	Tools []action.ToolSpec
	// FastComputerUse lets the fast provider execute the exact standard
	// computer-use actions present in Tools and backed by an in-process
	// dispatcher. It also admits standard client-executed computer actions only
	// when the client explicitly declares confirm=never and a target. It is
	// opt-in. Arbitrary tools and names that merely share the computer. prefix
	// remain slow-only, and every emitted call crosses the server's authority,
	// confirmation, ledger, and audit path.
	FastComputerUse bool
	// FastBackgroundTools lets the fast provider start only tools whose
	// declaration explicitly marks them Background. It is independent from
	// FastComputerUse so a deployment can give the local foreground authority
	// to launch safe asynchronous work without creating a second coordinate
	// controller. The provider must still declare execution authority.
	FastBackgroundTools bool

	// Confirmer authorizes actions whose declared requirement is "always".
	// Nil denies them, which is the right default and a real one: an action a
	// developer marked as needing explicit authorization does not execute
	// because nobody was there to authorize it.
	Confirmer action.Confirmer
	// ConfirmPolicy answers the "policy" requirement. Nil reads "policy" as
	// "always", which denies without a confirmer - so a deployment that
	// declares tools requiring a policy and supplies none gets tools that
	// cannot run, rather than tools that run unpoliced.
	ConfirmPolicy action.PolicyDecision
	// ActionAudit receives every dispatch attempt and outcome.
	ActionAudit func(action.Record)

	// Governor admits speculative preparation against the same compute budget
	// as everything else. A speculation that starves the foreground turn has
	// spent the latency it was trying to save, so preparation runs at the
	// speculative class - below the voice, above nothing.
	//
	// Nil means preparation is unadmitted, which is correct for a deployment
	// whose providers are hosted and compete for no local capacity.
	Governor *admission.Governor

	MaxPendingEvents int
	MediaRetention   session.MediaConfig
	Scheduler        clock.Scheduler
	// ClientToolTimeout bounds how long a client has to return results for
	// the tools it executes. Zero selects the default; negative disables the
	// deadline, which only a harness driving results by hand should do.
	ClientToolTimeout time.Duration
	// AgentInstruction is the deployment's own instruction.
	AgentInstruction string
}

// Binding is the cascade binding.
type Binding struct {
	config Config
}

// New validates the component set and creates the binding.
func New(config Config) (*Binding, error) {
	if config.Profile == "" {
		config.Profile = "voice"
	}
	if config.Profile != "voice" && config.Profile != "voice+vision" {
		return nil, errors.New("cascade profile must be voice or voice+vision")
	}
	if config.Perception == nil {
		return nil, errors.New("cascade requires a perception provider factory")
	}
	if config.Fast == nil || config.Slow == nil {
		return nil, errors.New("cascade requires fast and slow continuation providers")
	}
	if config.Speech == nil {
		return nil, errors.New("cascade requires a streaming speech provider")
	}
	if config.Voices == nil {
		if config.SpeakerIdentityDescriptor.Name != "" ||
			config.SpeakerIdentityDescriptor.Version != "" ||
			config.SpeakerIdentityDescriptor.Capabilities != nil {
			return nil, errors.New("a speaker identity descriptor requires a speaker embedder")
		}
	} else if config.SpeakerIdentityDescriptor.Name != "" ||
		config.SpeakerIdentityDescriptor.Version != "" ||
		config.SpeakerIdentityDescriptor.Capabilities != nil {
		if err := config.SpeakerIdentityDescriptor.Validate(); err != nil {
			return nil, fmt.Errorf("invalid speaker identity descriptor: %w", err)
		}
	}
	fastAuthority := config.Fast.Descriptor().EffectiveToolAuthority()
	if config.FastComputerUse || config.FastBackgroundTools {
		if fastAuthority != continuation.ToolAuthorityExecute {
			return nil, errors.New("fast tool execution requires a fast provider with execution authority")
		}
	} else if fastAuthority == continuation.ToolAuthorityExecute {
		return nil, errors.New("fast provider execution authority requires an explicit fast-tool mode")
	}
	if config.VisualReflex != nil {
		descriptor := config.VisualReflex.Descriptor()
		if err := continuation.ValidateDescriptor(descriptor); err != nil {
			return nil, fmt.Errorf("invalid visual reflex provider: %w", err)
		}
		if descriptor.Phase != trajectory.PhaseFast || !descriptor.Vision ||
			descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityExecute ||
			descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
			return nil, errors.New("visual reflex must be a silent, vision-capable fast provider with execution authority")
		}
		if config.VisualReflexMaxTokens < 0 || config.VisualReflexTimeout < 0 {
			return nil, errors.New("visual reflex token limit and timeout cannot be negative")
		}
	}
	policy, err := ParseObservationPolicy(string(config.ObservationPolicy))
	if err != nil {
		return nil, err
	}
	config.ObservationPolicy = policy
	if config.ASRCadence <= 0 {
		config.ASRCadence = 200 * time.Millisecond
	}
	if config.FrameDuration <= 0 {
		config.FrameDuration = 100 * time.Millisecond
	}
	if config.MaxPendingEvents <= 0 {
		config.MaxPendingEvents = 128
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	if config.Policies.Validate() != nil {
		config.Policies = interaction.Defaults()
	}
	if err := validateObservationAgainstDeferral(config.ObservationPolicy, config.Policies.Deferral); err != nil {
		return nil, err
	}
	if config.ManualDeferral != nil {
		if err := validateObservationAgainstDeferral(
			config.ObservationPolicy, config.ManualDeferral,
		); err != nil {
			return nil, fmt.Errorf("manual deferral: %w", err)
		}
	}
	for index, factory := range config.Observers {
		if err := factory.Validate(); err != nil {
			return nil, fmt.Errorf("observer %d: %w", index, err)
		}
	}
	return &Binding{config: config}, nil
}

// validateObservationAgainstDeferral refuses a pair of policies that cancel
// each other out.
//
// The stable-partial policy exists to let the agent start answering before the
// user has finished, and that is the whole of its value. A deferral policy
// that waits for the user to stop speaking holds every one of those partials
// until the endpoint, where the final observation arrives anyway - so the
// configuration costs an extra committed observation and a supersession and
// buys exactly nothing.
//
// Refusing is better than quietly picking one. Both policies are measured
// factors, and a runtime that silently overrode one of them would report a
// configuration it was not running - which is the failure this project keeps
// pointing at everywhere else.
func validateObservationAgainstDeferral(observation ObservationPolicy, deferral interaction.Deferral) error {
	if observation != ObservationStablePartial || deferral == nil {
		return nil
	}
	if !slices.Contains(deferral.Conditions(), session.UserSpeechStopped) {
		return nil
	}
	return fmt.Errorf(
		"the %s observation policy admits partials before the endpoint, but the %q deferral policy defers "+
			"every run until the user stops speaking, so nothing would ever act on one: either configure a "+
			"deferral that allows running while the user is speaking, or use the %s observation policy",
		ObservationStablePartial, deferral.Name(), ObservationEndpointOnly)
}

// Name is the binding's stable identifier.
func (bind *Binding) Name() string { return "cascade" }

// Ownership declares that the engine provides everything.
func (bind *Binding) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
}

// ObserverNames is every observer a session here may select from.
//
// The audio observer is always in the list because a cascade's recogniser is
// the binding, but it is selectable: a task with no speech in it should not
// pay for speech recognition, and factor F3's video-only level is exactly that
// question. A level that silently ran the audio observer anyway would be the
// same experiment as audio+video wearing a different name.
func (bind *Binding) ObserverNames() []string {
	names := make([]string, 0, len(bind.config.Observers)+1)
	names = append(names, audioObserverName)
	for _, factory := range bind.config.Observers {
		names = append(names, factory.Name)
	}
	return names
}

// audioObserverName is the recogniser's name in an observer selection.
const audioObserverName = "audio"

// Capabilities reports what a cascade session supports.
//
// Video is reported from the configured observer set rather than from a build
// flag, so a deployment that did not configure a video observer negotiates
// honestly instead of accepting frames it will discard.
func (bind *Binding) Capabilities() binding.Capabilities {
	video := false
	for _, factory := range bind.config.Observers {
		if factory.Kind == perception.FrameImage {
			video = true
		}
	}
	return binding.Capabilities{
		Voice:           binding.VoiceControl{InForce: bind.config.Voice},
		ManualTurns:     true,
		MaxOutputTokens: bind.config.FastMaxTokens,
		Video:           video, ComputerUse: true, Observations: true, FastSlow: true,
		Observers: bind.ObserverNames(),
		Stack: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, Transcription: true,
			TurnGeneration: true, ConcurrentIO: true,
		},
	}
}

// Start creates a session runtime.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	return newRuntime(ctx, bind, options)
}

var _ binding.Binding = (*Binding)(nil)
