// Package upstream puts a remote Realtime endpoint behind OpenRealtime's
// background reasoner.
//
// The remote model owns perception, the fast voice, and action; it is a
// complete voice stack already. What it does not have, and cannot have, is a
// second model reasoning over the same conversation while it talks - a
// single-model server has no second model and no shared log to put one on.
//
// So this binding mirrors the remote conversation into the canonical
// trajectory, runs the engine's slow provider over it, executes the tools that
// provider calls, and hands the result back for the remote to say. The hand-off
// is explicit rather than clever, which is what makes it work against any
// Realtime-compatible endpoint rather than against one vendor's internals.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/bojieli/OpenRealtime/session"
)

// Config configures the binding.
type Config struct {
	// URL is the remote Realtime WebSocket endpoint.
	URL string
	// Token is the remote credential.
	Token string
	// Model is the remote model identifier.
	Model string
	// Header carries provider-specific headers.
	Header http.Header
	// EventAliases renames inbound server events onto the ones the mirror
	// reads, for an endpoint that implements an older spelling of the spec.
	EventAliases map[string]string
	// Handoff selects how the reasoner's answer is given to the remote to
	// say. Empty selects the conversation-item form.
	Handoff Handoff
	// Dial opens the connection. Empty selects a Realtime WebSocket client,
	// which is what every endpoint that implements the protocol needs. An
	// endpoint that does not - Gemini Live - supplies a translator here.
	Dial Dialer

	// Slow is the engine's background reasoner. It is the whole point of this
	// binding and is therefore required, unlike every other component here.
	Slow          continuation.Provider
	SlowMaxTokens int
	// AgentInstruction is composed ahead of the slow phase instruction.
	AgentInstruction string

	// Policies overrides the interaction policy set. The default is
	// endpointed slow-only with voicing, because the remote already owns the
	// fast voice: running the engine's fast provider too would produce two
	// voices answering the same question.
	Policies interaction.Policies

	// FloorOwner declares who decides endpoints. The remote's own VAD is the
	// default because it is already running; declaring the engine here is the
	// F5 comparison this binding makes available.
	FloorOwner binding.Owner

	MaxPendingEvents int
	MediaRetention   session.MediaConfig
	Scheduler        clock.Scheduler
	// HandoffTimeout bounds how long a slow answer waits to be voiced.
	HandoffTimeout time.Duration
	// ClientToolTimeout bounds how long a client has to return results for
	// the tools it executes. Zero selects the default; negative disables the
	// deadline, which only a harness driving results by hand should do.
	ClientToolTimeout time.Duration

	// Dialect names the wire contract the remote speaks, from the provider
	// catalogue. Most of this binding is portable; the parts that are not are
	// decided here rather than by probing what the connection happens to do.
	Dialect string
	// DelegationGating decides whether the reasoner runs on every user turn
	// or only when the remote asks for help. Empty selects the dialect's
	// default: gated on an endpoint that delegates, because there the voice
	// answering by itself is the design and a reasoner answering "hello" is a
	// cost; ungated everywhere else, because nothing there ever asks.
	DelegationGating Gating
	// IdleTimeout closes a session that has heard no user speech for this
	// long. It exists because a full-duplex endpoint is billed by the second
	// and kept alive by this binding's own silence frames: without a bound an
	// abandoned tab is a running meter. Zero selects the dialect's default -
	// fifteen minutes on GPT-Live, none elsewhere - and negative disables it.
	IdleTimeout time.Duration
	// ContextPushDebounce coalesces observer evidence bound for the remote,
	// so a burst of screen changes becomes one summary of the latest state
	// rather than a summary per change. Zero selects 400ms.
	ContextPushDebounce time.Duration

	// Observers are the video observers a session may run. The remote owns
	// hearing; seeing is this side's, because no remote in the catalogue can
	// be sent a frame, and what an observer narrates reaches the remote as
	// context. Empty leaves video unsupported, as it always was.
	Observers []perception.Factory
	// DefaultObservers is the set a session runs when the client names none.
	// Empty selects every configured observer.
	DefaultObservers []string
	// Greeting is an application instruction sent once the session has
	// started, for a voice that should speak first. On GPT-Live it goes down
	// the instruction channel with audio already running, which is the
	// vendor's own recipe; on a Realtime endpoint it is context the first
	// response reads.
	Greeting string
	// MaxSessions bounds concurrent sessions on this endpoint, so the
	// vendor's own tier limit is met here with a reason rather than there
	// with a refusal mid-conversation. Zero is unbounded.
	MaxSessions int
	// Extraction notices when the user sets a policy out loud and steers
	// the remote with it. Nil leaves standing instructions to the remote.
	Extraction interaction.Extractor
	// Store asks the remote to keep a resumable recording of the session,
	// on the endpoints that can. It is what makes a dropped connection
	// recoverable by forking rather than starting cold.
	Store bool
	// AudioFormat is the wire format the remote is opened with, on the
	// endpoints that offer more than one. Empty selects PCM.
	AudioFormat string
	// BargeIn lets this binding's own barge-in policy stop the remote when the
	// user talks over it, and hold the remote's audio back until it does. Nil
	// selects off, which is the right default and was not the first answer.
	//
	// A full-duplex model handles being interrupted itself; that is most of
	// what full duplex is for, and GPT-Live is good at it. Measured against
	// the real endpoint on one Full-Duplex-Bench interruption it yielded in
	// 2.8 s with nothing helping it - after this side stopped cutting silence
	// into the audio on the way in, which had been costing 16.4 s and was a
	// defect here rather than a limit there.
	//
	// So this is an override, not a fix. It buys a sub-second yield - 0.26 s
	// on the same recording - by acting where the model cannot: the
	// instruction channel stops the model itself, slowly, and holding the
	// audio at this relay stops what the person hears at once, which is where
	// the vendor says to block output when an application needs speech to
	// stop. A deployment that must guarantee the floor within a second turns
	// it on and accepts that its own policy, not the model's judgement, is
	// deciding; everything else should leave the model to it.
	BargeIn *bool

	delegationGated bool
}

// Gating is how the reasoner is triggered.
type Gating string

const (
	// GatingAuto selects the dialect's default.
	GatingAuto Gating = ""
	// GatingOn runs the reasoner only when the remote delegates.
	GatingOn Gating = "on"
	// GatingOff runs the reasoner on every user turn.
	GatingOff Gating = "off"
)

// DialectGPTLive names OpenAI's Live protocol, the one endpoint in the
// catalogue that delegates. The string matches the provider catalogue's; it is
// repeated here because the catalogue imports this package.
const DialectGPTLive = "gpt-live"

// ParseGating resolves a configured gating level.
func ParseGating(value string) (Gating, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return GatingAuto, nil
	case "on", "delegation":
		return GatingOn, nil
	case "off", "always":
		return GatingOff, nil
	default:
		return "", fmt.Errorf("delegation gating must be auto, on, or off, got %q", value)
	}
}

// Handoff is how a completed answer reaches the remote's voice.
//
// It exists because the base protocol turned out not to be as portable as it
// looked. Injecting a conversation item is the natural form and every endpoint
// modelled on OpenAI's own accepts it - but Alibaba's Qwen-Omni-Realtime
// accepts conversation.item.create only for tool results, and its
// response.create takes no per-response instructions. On that endpoint the
// session instruction is the only writable channel there is.
//
// So the mechanism is a declared property of the endpoint rather than an
// assumption, in the same way a provider's reasoning switch is. An endpoint
// that supports neither cannot host this binding at all, and saying which one
// it supports is what makes that checkable.
type Handoff string

const (
	// HandoffConversationItem appends a system message the remote reads as
	// context, then asks for a response. It is the portable form.
	HandoffConversationItem Handoff = "conversation-item"
	// HandoffSessionInstruction rewrites the session instruction to carry the
	// answer, asks for a response, and restores the instruction when the
	// response completes. It is for an endpoint whose only writable channel
	// is the session.
	HandoffSessionInstruction Handoff = "session-instruction"
)

// Dialer opens a connection to a remote realtime endpoint.
//
// It is a function rather than a dialect enum so that translating a foreign
// protocol into this one stays outside the binding. The binding's job is the
// background reasoner; which wire format the remote happens to speak is not
// something it should have opinions about.
type Dialer func(ctx context.Context, config Config) (RemoteConn, error)

// RemoteConn is the connection to whatever is at the other end.
type RemoteConn interface {
	Events() <-chan realtimeclient.Event
	Err() error
	Send(ctx context.Context, value any) error
	Close() error
}

// Binding connects to a remote Realtime endpoint.
// wireSampleRateHz is the PCM rate a Realtime endpoint speaks and listens at,
// in both dialects. It is not negotiable per session: a client that arrives at
// another rate - a telephone leg at 8kHz, say - is converted to this one on
// the way in and back again on the way out.
const wireSampleRateHz = 24_000

type Binding struct {
	config Config
	// slots holds one token per allowed concurrent session.
	slots chan struct{}
}

// ErrAtCapacity reports a session refused because the endpoint's concurrent
// session bound has been reached.
var ErrAtCapacity = errors.New("upstream endpoint is at its concurrent session bound")

// New validates the configuration.
func New(config Config) (*Binding, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, errors.New("upstream requires a remote Realtime URL")
	}
	if config.Slow == nil {
		return nil, errors.New("upstream requires a slow continuation provider: it is what this binding adds")
	}
	if config.SlowMaxTokens <= 0 {
		config.SlowMaxTokens = 2048
	}
	if config.MaxPendingEvents <= 0 {
		config.MaxPendingEvents = 128
	}
	if config.HandoffTimeout <= 0 {
		config.HandoffTimeout = 30 * time.Second
	}
	if config.Handoff == "" {
		config.Handoff = HandoffConversationItem
	}
	switch config.Handoff {
	case HandoffConversationItem, HandoffSessionInstruction:
	default:
		return nil, fmt.Errorf("unsupported upstream handoff %q", config.Handoff)
	}
	config.EventAliases = maps.Clone(config.EventAliases)
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	switch config.FloorOwner {
	case "":
		config.FloorOwner = binding.OwnerRemote
	case binding.OwnerEngine, binding.OwnerRemote:
	default:
		return nil, errors.New("upstream floor must be owned by the engine or the remote")
	}
	live := config.Dialect == DialectGPTLive
	if live && config.FloorOwner == binding.OwnerEngine {
		// Live has no input commit and no detector to switch off. An engine
		// floor would be declared, forwarded as nothing, and honoured by
		// nobody - refusing is the only honest answer.
		return nil, errors.New("GPT-Live owns the floor: it has no turn commit for the engine to drive")
	}
	switch config.DelegationGating {
	case GatingAuto:
		config.delegationGated = live
	case GatingOn:
		config.delegationGated = true
	case GatingOff:
		config.delegationGated = false
	default:
		return nil, fmt.Errorf("unsupported delegation gating %q", config.DelegationGating)
	}
	switch {
	case config.IdleTimeout < 0:
		config.IdleTimeout = 0
	case config.IdleTimeout == 0 && live:
		config.IdleTimeout = 15 * time.Minute
	}
	if config.ContextPushDebounce <= 0 {
		config.ContextPushDebounce = 400 * time.Millisecond
	}
	if config.BargeIn == nil {
		// Off. Every endpoint here handles its own interruption, and taking
		// that decision away from a model that is good at it is a choice a
		// deployment makes deliberately rather than one it inherits.
		disabled := false
		config.BargeIn = &disabled
	}
	if *config.BargeIn && !live {
		// Only a steerable dialect can be stopped. A Realtime endpoint has no
		// instruction channel that interrupts speech, so there is nothing to
		// turn on.
		return nil, errors.New("upstream barge-in needs an endpoint that can be steered mid-sentence, which only GPT-Live is")
	}
	for _, factory := range config.Observers {
		if err := factory.Validate(); err != nil {
			return nil, err
		}
	}
	config.Greeting = strings.TrimSpace(config.Greeting)
	if config.Policies.Validate() != nil {
		config.Policies = defaultPolicies()
	}
	if config.Extraction != nil {
		config.Policies.Extraction = config.Extraction
	}
	bind := &Binding{config: config}
	if config.MaxSessions > 0 {
		bind.slots = make(chan struct{}, config.MaxSessions)
	}
	return bind, nil
}

// acquire takes a session slot, or refuses.
func (bind *Binding) acquire() error {
	if bind.slots == nil {
		return nil
	}
	select {
	case bind.slots <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("%w (%d)", ErrAtCapacity, cap(bind.slots))
	}
}

// release returns a session slot.
func (bind *Binding) release() {
	if bind.slots == nil {
		return
	}
	select {
	case <-bind.slots:
	default:
	}
}

func defaultPolicies() interaction.Policies {
	policies := interaction.Defaults()
	// The remote is the fast provider. The engine's rollout must therefore run
	// slow and hand the answer back, never run a second voice of its own.
	policies.Rollout = interaction.NewEndpointedSlowOnlyRollout(interaction.RolloutOptions{})
	policies.Trigger = interaction.NewEndpointTrigger()
	policies.Preparation = interaction.NewEndpointPreparation()
	// The remote paces and cancels its own speech, so the engine does not
	// defer on a duplex state it is not the authority for.
	policies.Deferral = interaction.AlwaysRun{}
	return policies
}

// Name is the binding's stable identifier.
func (bind *Binding) Name() string { return "upstream" }

// Ownership declares that the remote owns the voice stack and the engine owns
// the background reasoner.
func (bind *Binding) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerRemote, FastCognition: binding.OwnerRemote,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerRemote,
		Interaction: binding.OwnerRemote, Floor: bind.config.FloorOwner,
	}
}

// Capabilities reports what an upstream session supports. Video and computer
// use depend on the remote, which the base protocol gives no way to ask about,
// so they are reported off rather than guessed at.
func (bind *Binding) Capabilities() binding.Capabilities {
	// The remote owns the voice stack, and this binding does not forward a
	// voice to it. Neither field can be filled in honestly: the session cannot
	// choose, and the default belongs to the provider rather than to us.
	var observers []string
	for _, factory := range bind.config.Observers {
		observers = append(observers, factory.Name)
	}
	return binding.Capabilities{
		Observations: true, FastSlow: true,
		// GPT-Live names its voice in session.start and this binding forwards
		// what the client chose, so a session may select one. Declaring
		// otherwise made the gateway refuse the field outright, which is how a
		// client that names a voice in every session.update - the ordinary
		// shape of a Realtime client, and what tau2-bench does - had its
		// configuration answered with an error it did not expect.
		Voice: binding.VoiceControl{Selectable: bind.config.Dialect == DialectGPTLive},
		// Seeing is this side's. A frame never goes to the remote; it goes to
		// an observer configured here, and what the observer says reaches the
		// remote as context. So video is supported exactly when an observer
		// is, and the names are the ones a client may select from.
		Video: len(bind.config.Observers) > 0, Observers: observers,
		// The remote runs the floor, and a Realtime endpoint speaks this
		// protocol: a client taking the floor is forwarded rather than
		// interpreted. Live has nothing to forward it to - no commit, no
		// detector - so there the answer is no, and a client told otherwise
		// would be talking over an agent that never agreed to listen.
		ManualTurns: bind.config.Dialect != DialectGPTLive,
		Stack: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true,
			TextInjection: true,
		},
	}
}

// Start opens a session against the remote endpoint.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	if err := bind.acquire(); err != nil {
		return nil, err
	}
	runtime, err := newRuntime(ctx, bind, options)
	if err != nil {
		bind.release()
		return nil, err
	}
	return runtime, nil
}

var _ binding.Binding = (*Binding)(nil)
