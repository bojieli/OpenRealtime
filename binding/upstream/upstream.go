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
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
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
}

// Binding connects to a remote Realtime endpoint.
type Binding struct {
	config Config
}

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
	if config.Policies.Validate() != nil {
		config.Policies = defaultPolicies()
	}
	return &Binding{config: config}, nil
}

func defaultPolicies() interaction.Policies {
	policies := interaction.Defaults()
	// The remote is the fast provider. The engine's rollout must therefore run
	// slow and hand the answer back, never run a second voice of its own.
	policies.Rollout = interaction.NewEndpointedSlowOnlyRollout(interaction.RolloutOptions{
		VoiceSlowOutput: true,
	})
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
		Floor: bind.config.FloorOwner,
	}
}

// Capabilities reports what an upstream session supports. Video and computer
// use depend on the remote, which the base protocol gives no way to ask about,
// so they are reported off rather than guessed at.
func (bind *Binding) Capabilities() binding.Capabilities {
	return binding.Capabilities{Observations: true, FastSlow: true}
}

// Start opens a session against the remote endpoint.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	return newRuntime(ctx, bind, options)
}

var _ binding.Binding = (*Binding)(nil)
