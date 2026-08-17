package translation

import "testing"

func TestPoliciesExposeLatencyQualityComputeTradeoff(t *testing.T) {
	t.Parallel()
	segments := []Segment{
		{EndMS: 200, SourceDelta: "A", TargetDelta: "1", EarlyTargetDelta: "X"},
		{EndMS: 400, SourceDelta: "B", TargetDelta: "2", EarlyTargetDelta: "2"},
	}
	results := make(map[PolicyKind]Evaluation)
	for _, policy := range DefaultPolicies() {
		result, err := policy.Evaluate(segments, 7)
		if err != nil {
			t.Fatal(err)
		}
		results[policy.Kind] = result
	}
	if results[PolicyEndpointed].MeanLagNS <= results[PolicyStableIncrement].MeanLagNS {
		t.Fatal("endpointed policy did not lag incremental output")
	}
	if results[PolicyStableIncrement].QualityScore != 100 || results[PolicyAggressive].QualityScore >= 100 {
		t.Fatal("quality accounting did not distinguish authored aggressive error")
	}
	if !results[PolicyAggressive].AppendOnlyOutput {
		t.Fatal("translation output must remain append-only")
	}
	for index, emission := range results[PolicyEndpointed].Emissions[1:] {
		if emission.EmittedAtNS <= results[PolicyEndpointed].Emissions[index].EmittedAtNS {
			t.Fatal("endpointed deltas were not emitted in append-only segment order")
		}
	}
}
