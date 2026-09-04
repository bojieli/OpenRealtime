package continuation

import (
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// TerminalToolProposalNotice is the portable, model-visible projection of a
// validated proposal disposition. The disposition itself deliberately carries
// no arguments, and this notice likewise reveals no control payload.
const TerminalToolProposalNotice = "Runtime status: a prior tool proposal became terminal without execution and caused no action. Re-evaluate the current observations before deciding what to do."

// PendingToolProposalContent projects canonical working state without making
// it look like assistant-authored tool-call JSON. A pending proposal is still
// composable: another policy or model may promote it, so its exact name and raw
// arguments must remain visible until an ordered promotion or disposition is
// committed.
func PendingToolProposalContent(call *trajectory.ToolCall) (string, bool) {
	if call == nil || strings.TrimSpace(call.Name) == "" || len(call.Arguments) == 0 {
		return "", false
	}
	return fmt.Sprintf(
		"Runtime context: a model proposed a tool operation that is still pending; it has not executed and no terminal policy decision has been recorded.\nPending proposal tool name: %s\nPending proposal arguments (exact JSON bytes): %s",
		call.Name, string(call.Arguments),
	), true
}

// ProviderRun is one canonical item, or a run of adjacent observations that
// all carry user authority.
//
// Speech recognisers are allowed to commit more than one final fragment for a
// single human turn. Presenting those fragments as consecutive chat turns is
// not equivalent to presenting what the person said: some chat templates
// assign special meaning to every role boundary. A provider therefore sees an
// adjacent run as one user turn while the canonical trajectory remains an
// append-only record of every observation and its provenance.
type ProviderRun struct {
	Items            []trajectory.Item
	UserObservations bool
}

// HeardPreamble opens the runtime note that follows a turn the user did not
// hear all of. It is exported so a caller can tell the note apart from
// anything a model wrote.
const HeardPreamble = "[runtime: "

// AssistantHeardContent renders an assistant turn as the user received it.
//
// An interrupted turn is two facts, and a projection that carries only one of
// them is wrong in a specific, reproducible way. Carry only the whole text and
// the agent believes it said words that were cut off before they were audible:
// asked to read a long list and interrupted partway, it resumes after the last
// item it wrote rather than after the last one anybody heard. Carry only the
// heard part and the words it had prepared are gone, so it either invents a
// different continuation or starts the list again.
//
// So the turn becomes what was heard, and what was not heard follows it as a
// runtime note that is unmistakably not speech. The words are still there for
// the model to continue from; they are simply no longer presented as something
// the user was told.
func AssistantHeardContent(content string, mark spoken.Mark) string {
	pending := strings.TrimSpace(mark.Pending)
	if pending == "" {
		return content
	}
	var note strings.Builder
	spokenText := strings.TrimSpace(mark.Spoken)
	if spokenText != "" {
		note.WriteString(spokenText)
		note.WriteString("\n\n")
	}
	note.WriteString(HeardPreamble)
	switch {
	case spokenText == "":
		note.WriteString("this turn was prepared and never became audible; the user heard none of it")
	case mark.Cut != "":
		note.WriteString("playback stopped here, in the middle of " + quote(mark.Cut) +
			". The user heard everything above and nothing after it")
	default:
		note.WriteString("playback stopped here. The user heard everything above and nothing after it")
	}
	note.WriteString(". Prepared but never spoken: " + quote(pending) + "]")
	return note.String()
}

func quote(text string) string { return "\"" + strings.TrimSpace(text) + "\"" }

// ProviderRuns groups only adjacent user-authority observations.
//
// Assistant, tool, observer, instruction, and runtime-state items are hard
// boundaries. The returned values are a projection over copies of the item
// structs; this function never rewrites the supplied canonical slice.
//
// It is also where an assistant turn becomes what the user heard of it. That
// belongs here rather than in each provider adapter for the same reason
// superseded partials do: it is one rule about what a conversation looked like,
// three adapters would implement it three times, and the one that forgot would
// be the one whose agent talked past words nobody heard.
func ProviderRuns(items []trajectory.Item) []ProviderRun {
	heard := trajectory.AssistantHeard(trajectory.Snapshot{Items: items})
	runs := make([]ProviderRun, 0, len(items))
	starts := make([]int, 0, len(items))
	for itemIndex, item := range items {
		if item.Kind == trajectory.KindAssistant {
			if mark, known := heard[item.ID]; known {
				item.Content = AssistantHeardContent(item.Content, mark)
			}
		}
		userObservation := item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser
		if userObservation && len(runs) > 0 && runs[len(runs)-1].UserObservations {
			runs[len(runs)-1].Items = append(runs[len(runs)-1].Items, item)
			continue
		}
		starts = append(starts, itemIndex)
		runs = append(runs, ProviderRun{
			Items:            []trajectory.Item{item},
			UserObservations: userObservation,
		})
	}
	for index := range runs {
		if runs[index].UserObservations {
			start := starts[index]
			runs[index].Items = currentUserObservations(items, start, start+len(runs[index].Items))
		}
	}
	return runs
}

// currentUserObservations removes only revisions whose explicit replacement
// is present in the same uninterrupted user-observation run. The canonical
// trajectory remains append-only; this is the smaller conversation prefix a
// provider should reason over.
//
// A model-visible boundary is also an action boundary. Once an assistant,
// tool, observer, instruction, or runtime-state item intervenes, an eventual
// revision must remain visible as a replacement instead of rewriting what the
// model had already seen. ProviderRuns calls this helper separately for each
// run to enforce that distinction structurally.
func currentUserObservations(items []trajectory.Item, start, end int) []trajectory.Item {
	run := items[start:end]
	if len(run) < 2 {
		// ProviderRuns promises a projection over item-struct copies. Returning
		// this subslice directly would let a provider-side rewrite of even a
		// scalar field mutate the canonical snapshot whenever a user run held a
		// single observation.
		return append([]trajectory.Item(nil), run...)
	}

	superseded := make([]bool, len(run))
	consumedReplacement := make([]bool, len(run))
	for localLaterIndex, later := range run {
		laterIndex := start + localLaterIndex
		earlierIndex, err := trajectory.ResolveObservationSupersession(items[:laterIndex], later)
		if err != nil || earlierIndex < start || earlierIndex >= end {
			// A non-canonical edge cannot authorize hiding user input. This is
			// the same full-prefix resolver Store uses. A valid edge across a
			// provider boundary also stays explicit because the model may have
			// acted on the target already.
			continue
		}
		superseded[earlierIndex-start] = true
		consumedReplacement[localLaterIndex] = true
	}

	projected := make([]trajectory.Item, 0, len(run))
	for index, item := range run {
		if superseded[index] {
			continue
		}
		if consumedReplacement[index] {
			// The target was projected away in this same user turn, so the
			// surviving text is now a complete current observation. Clear
			// only the projected copy's marker to avoid telling the provider
			// to replace context it was deliberately never shown.
			event := *item.Event
			event.SupersedesRevision = 0
			item.Event = &event
		}
		projected = append(projected, item)
	}
	return projected
}

// ObservationRunContent renders a user-observation run as one textual turn.
// Each surviving observation is rendered independently before joining, so
// elapsed-time notes and cross-boundary supersession remain attached to the
// fragment they describe.
func ObservationRunContent(run ProviderRun, elapsed map[string]string) string {
	contents := make([]string, 0, len(run.Items))
	for _, item := range run.Items {
		contents = append(contents, ObservationContent(item, elapsed[item.ID]))
	}
	return strings.Join(contents, "\n")
}
