package trajectory

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Authority separates what an item may *do* from what it merely *says*.
//
// Speech attributed to the human participant carries user authority. Content
// that an observer extracted from the environment — narrated screen text, a
// camera description, a sensor reading — carries observer authority and is
// data forever: it can be reasoned about, but it can never be promoted to an
// instruction, and no tool call may cite it as its own justification. This is
// the structural defence against prompt injection through observed content.
type Authority string

const (
	// AuthorityUser marks speech or text attributed to the human participant.
	AuthorityUser Authority = "user"
	// AuthorityObserver marks environment content extracted by an observer.
	AuthorityObserver Authority = "observer"
	// AuthoritySystem marks runtime-authored content such as instructions.
	AuthoritySystem Authority = "system"
)

// PhaseObserver produces observations from the environment rather than from
// the human participant. It is distinct from PhaseUser precisely so authority
// is decidable from provenance alone.
const PhaseObserver Phase = "observer"

// MediaRef is a handle to media retained outside the trajectory.
//
// The trajectory is copied in full for every continuation request, so it never
// inlines bytes. An observer commits persistent text and attaches handles; a
// provider adapter that can use media resolves the handle through a Store
// supplied by the runtime, and one that cannot ignores it. Retention is
// bounded: a handle may resolve to nothing once its window has passed, and a
// dangling handle is a normal state rather than an error.
type MediaRef struct {
	Handle   string `json:"handle"`
	MIMEType string `json:"mime_type"`
	Source   string `json:"source,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Bytes    int    `json:"bytes,omitempty"`
	// CapturedNS is source time, matching EventMetadata.OccurredNS semantics.
	CapturedNS uint64 `json:"captured_ns,omitempty"`
}

func (media MediaRef) validate() error {
	if strings.TrimSpace(media.Handle) == "" {
		return errors.New("media reference requires a handle")
	}
	if strings.TrimSpace(media.MIMEType) == "" {
		return errors.New("media reference requires a MIME type")
	}
	if media.Width < 0 || media.Height < 0 || media.Bytes < 0 {
		return errors.New("media reference dimensions and size cannot be negative")
	}
	return nil
}

// ObservationMeta records which observer produced an observation and what it
// was looking at. Its presence is what distinguishes environment content from
// participant speech; absence means the observation carries user authority,
// which is the pre-existing behaviour of the audio path.
type ObservationMeta struct {
	Observer  string     `json:"observer"`
	Source    string     `json:"source,omitempty"`
	Authority Authority  `json:"authority"`
	Media     []MediaRef `json:"media,omitempty"`
}

func (meta ObservationMeta) validate(producer Producer) error {
	if strings.TrimSpace(meta.Observer) == "" {
		return errors.New("observation metadata requires an observer name")
	}
	switch meta.Authority {
	case AuthorityUser:
		if producer.Phase != PhaseUser {
			return errors.New("user-authority observation must be produced by the user phase")
		}
	case AuthorityObserver:
		if producer.Phase != PhaseObserver {
			return errors.New("observer-authority observation must be produced by the observer phase")
		}
	default:
		return fmt.Errorf("observation authority must be %q or %q, got %q", AuthorityUser, AuthorityObserver, meta.Authority)
	}
	seen := make(map[string]struct{}, len(meta.Media))
	for index, media := range meta.Media {
		if err := media.validate(); err != nil {
			return fmt.Errorf("observation media %d: %w", index, err)
		}
		if _, duplicate := seen[media.Handle]; duplicate {
			return fmt.Errorf("duplicate observation media handle %q", media.Handle)
		}
		seen[media.Handle] = struct{}{}
	}
	return nil
}

func cloneObservationMeta(meta *ObservationMeta) *ObservationMeta {
	if meta == nil {
		return nil
	}
	copied := *meta
	copied.Media = slices.Clone(meta.Media)
	return &copied
}

// AuthorityOf resolves the authority an item carries. Only observations can
// carry observer authority; everything else is attributed by producer phase.
func AuthorityOf(item Item) Authority {
	switch item.Kind {
	case KindObservation:
		if item.Observation != nil {
			return item.Observation.Authority
		}
		if item.Producer.Phase == PhaseObserver {
			return AuthorityObserver
		}
		return AuthorityUser
	case KindInstruction:
		return AuthoritySystem
	default:
		return AuthoritySystem
	}
}

// ToolPlaceholder marks an executable call that was in flight when the
// trajectory was interrupted.
//
// It exists so an interrupted prefix is well-formed rather than dangling: a
// reader sees "this call was issued, the runtime stopped waiting, the result
// may still arrive" instead of a call that simply trails off. The eventual
// result supersedes it; a placeholder is never a terminal outcome and never
// satisfies the call.
type ToolPlaceholder struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Reason is runtime-authored provenance such as "interrupted" or
	// "cancelled". It is never model-authored workflow state.
	Reason string `json:"reason"`
}

// PromotedToolProposalIDs returns proposal item IDs that have either an exact
// promotion or a valid, causally linked schema-normalized promotion in the
// same invocation scope.
//
// Provider call IDs are not globally unique: many providers reuse values such
// as "call_0" for each invocation. Invocation, source revision, name,
// canonical arguments, and the causal proposal edge are consequently all part
// of the match. Similar, rejected, and unresolved proposals are not hidden.
func PromotedToolProposalIDs(snapshot Snapshot) map[string]struct{} {
	type identity struct {
		invocationID string
		callID       string
	}
	type indexedProposal struct {
		index int
		item  Item
	}
	proposals := make(map[identity][]indexedProposal)
	for index, item := range snapshot.Items {
		if item.Kind != KindToolProposal || item.ToolCall == nil {
			continue
		}
		key := identity{invocationID: item.InvocationID, callID: item.ToolCall.CallID}
		proposals[key] = append(proposals[key], indexedProposal{index: index, item: item})
	}

	promoted := make(map[string]struct{})
	for index, item := range snapshot.Items {
		if item.Kind != KindToolCall || item.ToolCall == nil {
			continue
		}
		key := identity{invocationID: item.InvocationID, callID: item.ToolCall.CallID}
		for _, proposal := range proposals[key] {
			if proposal.index < index && isPromotedToolProposal(proposal.item, item) {
				promoted[proposal.item.ID] = struct{}{}
			}
		}
	}
	return promoted
}

func (placeholder ToolPlaceholder) validate() error {
	if strings.TrimSpace(placeholder.CallID) == "" || strings.TrimSpace(placeholder.Name) == "" {
		return errors.New("tool placeholder call ID and name are required")
	}
	if strings.TrimSpace(placeholder.Reason) == "" {
		return errors.New("tool placeholder requires a runtime-authored reason")
	}
	return nil
}

// UnresolvedToolCalls returns executable calls in the prefix that have neither
// a terminal result nor a placeholder. These are the calls a runtime must
// either await or explicitly place a placeholder for before it stops.
func UnresolvedToolCalls(snapshot Snapshot) []PendingToolCall {
	callScopes := toolCallScopes(snapshot)
	resolved := make(map[toolIdentity]struct{})
	for _, item := range snapshot.Items {
		switch item.Kind {
		case KindToolResult:
			if item.ToolResult != nil {
				if identity, found := snapshotToolIdentity(item.InvocationID,
					item.ToolResult.CallID, callScopes); found {
					resolved[identity] = struct{}{}
				}
			}
		case KindToolPlaceholder:
			if item.ToolPlaceholder != nil {
				if identity, found := snapshotToolIdentity(item.InvocationID,
					item.ToolPlaceholder.CallID, callScopes); found {
					resolved[identity] = struct{}{}
				}
			}
		}
	}
	var pending []PendingToolCall
	for _, item := range snapshot.Items {
		if item.Kind != KindToolCall || item.ToolCall == nil {
			continue
		}
		identity := toolIdentity{invocationID: item.InvocationID, callID: item.ToolCall.CallID}
		if _, done := resolved[identity]; done {
			continue
		}
		call := *item.ToolCall
		call.Arguments = slices.Clone(call.Arguments)
		pending = append(pending, PendingToolCall{
			ItemID: item.ID, SourceRevision: item.SourceRevision,
			InvocationID: item.InvocationID, Call: call,
		})
	}
	return pending
}

func toolCallScopes(snapshot Snapshot) map[string][]toolIdentity {
	result := make(map[string][]toolIdentity)
	for _, item := range snapshot.Items {
		if item.Kind != KindToolCall || item.ToolCall == nil {
			continue
		}
		identity := toolIdentity{invocationID: item.InvocationID, callID: item.ToolCall.CallID}
		result[identity.callID] = append(result[identity.callID], identity)
	}
	return result
}

func snapshotToolIdentity(
	invocationID, callID string, callScopes map[string][]toolIdentity,
) (toolIdentity, bool) {
	if invocationID != "" {
		identity := toolIdentity{invocationID: invocationID, callID: callID}
		for _, candidate := range callScopes[callID] {
			if candidate == identity {
				return identity, true
			}
		}
		return toolIdentity{}, false
	}
	candidates := callScopes[callID]
	if len(candidates) != 1 {
		return toolIdentity{}, false
	}
	return candidates[0], true
}
