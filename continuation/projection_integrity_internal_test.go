package continuation

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// A projection is what a provider adapter hands back after deciding which
// canonical items this model should see. validateProjection is the check that
// it is a *view* of the record and not a rewrite of it. Five of its refusals
// had no coverage, and each one is a way a projection could quietly change
// history before it reaches the model.
func projectionCanonical() trajectory.Snapshot {
	return trajectory.Snapshot{
		Version: 3,
		Items: []trajectory.Item{
			{
				ID: "user-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
			},
			{
				ID: "fast-1", Kind: trajectory.KindAssistant, MonotonicNS: 2,
				CausalParentIDs: []string{"user-1"}, InvocationID: "inv-1",
				Producer: trajectory.Producer{
					Phase: trajectory.PhaseFast, Provider: "local", Model: "qwen",
				},
				Content: "hi", Visibility: trajectory.VisibilityPrepared,
				ProviderStateType: "vendor/state", ProviderState: json.RawMessage(`{"cursor":1}`),
			},
			{
				ID: "state-1", Kind: trajectory.KindAssistantState, MonotonicNS: 3,
				CausalParentIDs: []string{"fast-1"},
				Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: "fast-1", Visibility: trajectory.VisibilityPlayed,
				},
			},
		},
	}
}

func projectionOf(items ...trajectory.Item) trajectory.Snapshot {
	return trajectory.Snapshot{Version: 3, Items: items}
}

func TestProjectionIsAViewOfTheRecordAndNotARewrite(t *testing.T) {
	t.Parallel()
	canonical := projectionCanonical()
	if err := validateProjection(canonical, canonical); err != nil {
		t.Fatalf("identity projection = %v, want accepted", err)
	}
	// Dropping the tail is a legitimate projection; dropping a parent is not.
	if err := validateProjection(canonical, projectionOf(canonical.Items[0])); err != nil {
		t.Fatalf("prefix projection = %v, want accepted", err)
	}

	for _, test := range []struct {
		name      string
		projected trajectory.Snapshot
		want      string
	}{
		{
			name: "projection invents an item the record never held",
			projected: projectionOf(canonical.Items[0], trajectory.Item{
				ID: "ghost", Kind: trajectory.KindObservation,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "never said",
			}),
			want: "fabricated item",
		},
		{
			name:      "projection repeats one item",
			projected: projectionOf(canonical.Items[0], canonical.Items[0]),
			want:      "duplicated item",
		},
		{
			name:      "projection reorders the record",
			projected: projectionOf(canonical.Items[1], canonical.Items[0]),
			want:      "changed canonical item order",
		},
		{
			// Provider state is opaque and belongs to the run that produced
			// it. Swapping it hands the model a state it never emitted.
			name: "projection replaces opaque provider state",
			projected: func() trajectory.Snapshot {
				item := canonical.Items[1]
				item.ProviderState = json.RawMessage(`{"cursor":99}`)
				return projectionOf(canonical.Items[0], item)
			}(),
			want: "replaced provider state",
		},
		{
			name: "projection relabels the provider state type",
			projected: func() trajectory.Snapshot {
				item := canonical.Items[1]
				item.ProviderStateType = "vendor/other"
				return projectionOf(canonical.Items[0], item)
			}(),
			want: "replaced provider state type",
		},
		{
			name: "projection changes an item's content",
			projected: func() trajectory.Snapshot {
				item := canonical.Items[0]
				item.Content = "something else"
				return projectionOf(item)
			}(),
			want: "changed semantic item",
		},
		{
			// A parent that is not in the projection leaves the model reading
			// an effect whose cause it cannot see.
			name: "projection keeps a child but drops its parent",
			projected: func() trajectory.Snapshot {
				item := canonical.Items[1]
				item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
				return projectionOf(item)
			}(),
			want: "dangling parent",
		},
		{
			name: "projection keeps assistant state but drops the turn it describes",
			projected: func() trajectory.Snapshot {
				item := canonical.Items[2]
				item.CausalParentIDs = nil
				return projectionOf(item)
			}(),
			want: "dangling assistant state",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateProjection(projectionCanonical(), test.projected)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("projection error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
