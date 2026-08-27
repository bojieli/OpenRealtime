package continuation

import (
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

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

// ProviderRuns groups only adjacent user-authority observations.
//
// Assistant, tool, observer, instruction, and runtime-state items are hard
// boundaries. The returned values are a projection over copies of the item
// structs; this function never rewrites the supplied canonical slice.
func ProviderRuns(items []trajectory.Item) []ProviderRun {
	runs := make([]ProviderRun, 0, len(items))
	for _, item := range items {
		userObservation := item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser
		if userObservation && len(runs) > 0 && runs[len(runs)-1].UserObservations {
			runs[len(runs)-1].Items = append(runs[len(runs)-1].Items, item)
			continue
		}
		runs = append(runs, ProviderRun{
			Items:            []trajectory.Item{item},
			UserObservations: userObservation,
		})
	}
	return runs
}

// ObservationRunContent renders a user-observation run as one textual turn.
// Each observation is rendered independently before joining, so elapsed-time
// notes and typed supersession remain attached to the fragment they describe.
func ObservationRunContent(run ProviderRun, elapsed map[string]string) string {
	contents := make([]string, 0, len(run.Items))
	for _, item := range run.Items {
		contents = append(contents, ObservationContent(item, elapsed[item.ID]))
	}
	return strings.Join(contents, "\n")
}
