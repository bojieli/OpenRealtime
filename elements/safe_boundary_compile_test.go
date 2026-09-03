package elements

import (
	"strings"
	"testing"

	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestRawCognitionOutputsCannotBypassControlSerializationQuarantine(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name:   "prepared text to segmentation",
			source: rawTextToSegmentGraph,
			want: []string{
				"E_TYPE_MISMATCH",
				"model.text produces Segmented<text.PreparedDelta, flow.RunID>",
				"segment.text accepts Segmented<interaction.SafePreparedTextDelta, flow.RunID>",
			},
		},
		{
			name:   "prepared text to arbitration",
			source: rawTextToArbiterGraph,
			want: []string{
				"E_TYPE_MISMATCH",
				"model.text produces Segmented<text.PreparedDelta, flow.RunID>",
				"arbiter.text accepts Segmented<interaction.SafePreparedTextDelta, flow.RunID>",
			},
		},
		{
			name:   "result to trajectory commit",
			source: rawResultToCommitGraph,
			want: []string{
				"E_TYPE_MISMATCH",
				"model.result produces Event<cognition.Result>",
				"commit.result accepts Event<interaction.SafeModelResult>",
			},
		},
		{
			name:   "result to provenance join",
			source: rawResultToProvenanceGraph,
			want: []string{
				"E_TYPE_MISMATCH",
				"model.result produces Event<cognition.Result>",
				"join.result accepts Event<interaction.SafeModelResult>",
			},
		},
		{
			name:   "result to action arbitration",
			source: rawResultToActionArbiterGraph,
			want: []string{
				"E_TYPE_MISMATCH",
				"first.result produces Event<cognition.Result>",
				"arbiter.result accepts Event<interaction.SafeModelResult>",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := syntax.Parse(test.name+".ortg", []byte(test.source))
			if err != nil {
				t.Fatalf("parse bypass graph: %v", err)
			}
			_, err = graphcompiler.Compile(parsed, graphcompiler.Options{
				Catalog: productionDescriptorCatalog(t), ResolutionMode: resolve.Update,
			})
			if err == nil {
				t.Fatal("raw cognition output compiled into a safe-only consumer")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("compile error = %v, want diagnostic containing %q", err, want)
				}
			}
		})
	}
}

func TestControlSerializationQuarantineBridgesRawAndSafeTypes(t *testing.T) {
	parsed, err := syntax.Parse("quarantined-safe-boundary.ortg", []byte(quarantinedSafeBoundaryGraph))
	if err != nil {
		t.Fatalf("parse quarantined graph: %v", err)
	}
	if _, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: productionDescriptorCatalog(t), ResolutionMode: resolve.Update,
	}); err != nil {
		t.Fatalf("compile quarantined graph: %v", err)
	}
}

func productionDescriptorCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptors(catalog); err != nil {
		t.Fatalf("register production descriptors: %v", err)
	}
	return catalog
}

const rawTextToSegmentGraph = `graph raw_text_to_segment {
    cognition.TextModel :: model;
    interaction.SegmentPreparedText :: segment;

    model.text -> segment.text;
    model.outcome -> segment.terminal;

    input context = model.context;
    input trigger = model.trigger;
    input model_cancel = model.cancel;
    input segment_timeout = segment.timeout;
    input segment_cancel = segment.cancel;

    output model_result = model.result;
    output model_tools = model.tools;
    output model_resolution = model.resolved;
    output segments = segment.segments;
    output segment_model_cancel = segment.model_cancel;
    output speech_cancel = segment.speech_cancel;
    output segment_outcome = segment.outcome;
}`

const rawTextToArbiterGraph = `graph raw_text_to_arbiter {
    cognition.TextModel :: model;
    interaction.SpeechArbiter :: arbiter;

    model.text -> arbiter.text;
    model.outcome -> arbiter.terminal;

    input context = model.context;
    input trigger = model.trigger;
    input model_cancel = model.cancel;
    input selection = arbiter.selection;
    input arbiter_timeout = arbiter.timeout;
    input arbiter_cancel = arbiter.cancel;

    output model_result = model.result;
    output model_tools = model.tools;
    output model_resolution = model.resolved;
    output selected = arbiter.selected;
    output cancel_upstream = arbiter.cancel_upstream;
    output arbiter_outcome = arbiter.outcome;
}`

const rawResultToCommitGraph = `graph raw_result_to_commit {
    cognition.TextModel :: model;
    interaction.ModelResultCommit :: commit;

    model.result -> commit.result;

    input context = model.context;
    input trigger = model.trigger;
    input model_cancel = model.cancel;
    input committed = commit.committed;
    input rejected = commit.rejected;

    output model_text = model.text;
    output model_tools = model.tools;
    output model_outcome = model.outcome;
    output model_resolution = model.resolved;
    output append = commit.append;
    output commit_outcome = commit.outcome;
}`

const rawResultToProvenanceGraph = `graph raw_result_to_provenance {
    cognition.TextModel :: model;
    authority.ProvenanceJoin :: join;

    model.result -> join.result;
    model.tools -> join.proposal;

    input context = model.context;
    input trigger = model.trigger;
    input model_cancel = model.cancel;
    input candidate = join.candidate;
    input join_cancel = join.cancel;
    input join_timeout = join.timeout;

    output model_text = model.text;
    output model_outcome = model.outcome;
    output model_resolution = model.resolved;
    output provenance = join.provenance;
    output join_outcome = join.outcome;
    output join_resolution = join.resolved;
}`

const rawResultToActionArbiterGraph = `graph raw_result_to_action_arbiter {
    cognition.TextModel :: first;
    cognition.TextModel :: second;
    authority.ActionArbiter :: arbiter;

    first.result -> arbiter.result;
    second.result -> arbiter.result;

    input first_context = first.context;
    input first_trigger = first.trigger;
    input first_cancel = first.cancel;
    input second_context = second.context;
    input second_trigger = second.trigger;
    input second_cancel = second.cancel;
    input first_candidate = arbiter.candidate;
    input second_candidate = arbiter.candidate;
    input first_proposal = arbiter.proposal;
    input second_proposal = arbiter.proposal;

    output first_text = first.text;
    output first_tools = first.tools;
    output first_outcome = first.outcome;
    output first_resolution = first.resolved;
    output second_text = second.text;
    output second_tools = second.tools;
    output second_outcome = second.outcome;
    output second_resolution = second.resolved;
    output selected = arbiter.selected;
    output cancel_upstream = arbiter.cancel_upstream;
    output arbiter_outcome = arbiter.outcome;
    output arbiter_resolution = arbiter.resolved;
}`

const quarantinedSafeBoundaryGraph = `graph quarantined_safe_boundary {
    cognition.TextModel :: model;
    interaction.ControlSerializationQuarantine :: quarantine;
    interaction.SegmentPreparedText :: segment;
    interaction.ModelResultCommit :: commit;

    model.text -> quarantine.text;
    model.result -> quarantine.result;
    quarantine.safe_text -> segment.text;
    quarantine.safe_result -> commit.result;
    model.outcome -> segment.terminal;

    input context = model.context;
    input trigger = model.trigger;
    input model_cancel = model.cancel;
    input segment_timeout = segment.timeout;
    input segment_cancel = segment.cancel;
    input committed = commit.committed;
    input rejected = commit.rejected;

    output model_tools = model.tools;
    output model_resolution = model.resolved;
    output quarantined = quarantine.quarantined;
    output segments = segment.segments;
    output segment_model_cancel = segment.model_cancel;
    output speech_cancel = segment.speech_cancel;
    output segment_outcome = segment.outcome;
    output append = commit.append;
    output commit_outcome = commit.outcome;
}`
