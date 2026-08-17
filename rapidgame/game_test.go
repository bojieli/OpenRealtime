package rapidgame

import "testing"

func TestMicroturnMeetsDeadlineAtHigherCompute(t *testing.T) {
	t.Parallel()
	rounds := []Round{{ID: "one", CueAtMS: 1, DeadlineMS: 130, Prompt: "ONE", ExpectedAction: "tap-one"}}
	endpointed, err := (Condition{Kind: ConditionEndpointed, BaseReactionNS: 150_000_000, ComputeUnitsPerRound: 12}).Evaluate(rounds, 9)
	if err != nil {
		t.Fatal(err)
	}
	microturn, err := (Condition{Kind: ConditionMicroturn, BaseReactionNS: 60_000_000, ComputeUnitsPerRound: 20}).Evaluate(rounds, 9)
	if err != nil {
		t.Fatal(err)
	}
	if endpointed.FailureCount != 1 || microturn.FailureCount != 0 {
		t.Fatalf("unexpected deadline outcomes: endpointed=%+v microturn=%+v", endpointed, microturn)
	}
	if microturn.ComputeUnits <= endpointed.ComputeUnits {
		t.Fatal("microturn compute tradeoff was not accounted")
	}
}
