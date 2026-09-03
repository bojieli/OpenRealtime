package fdbv3

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

// The suite's whole reason for existing is that "did not know which tool to
// call" and "could not reassemble the identifier" are different failures. The
// scorer has to keep them different, and the second one is the easy one to
// score away by accident.

func TestSpelledIdentifiersAreReassembledOrTheyAreNot(t *testing.T) {
	for _, test := range []struct {
		name     string
		expected string
		observed string
		match    bool
	}{
		{
			name:     "exactly right",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BOB12"}`, match: true,
		},
		{
			name:     "a hyphen the recogniser added is not a different order",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BOB-12"}`, match: true,
		},
		{
			name:     "case is not a different order",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"bob12"}`, match: true,
		},
		{
			name: "letters never joined into a token is the failure this suite reports",
			// This is the one that matters. An agent that hears the caller
			// spell it out and sends the letters back unjoined has not got the
			// identifier, and a scorer that said otherwise would report the
			// project's own known failure mode as absent.
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"B O B 1 2"}`, match: false,
		},
		{
			name:     "a partly joined identifier is still wrong",
			expected: `{"order_id":"BOB12"}`, observed: `{"order_id":"BO B12"}`, match: false,
		},
		{
			name:     "a real word boundary survives normalisation",
			expected: `{"passenger_name":"Morgan Smith"}`, observed: `{"passenger_name":"morgan  smith"}`, match: true,
		},
		{
			name:     "two words are not one word",
			expected: `{"passenger_name":"Morgan Smith"}`, observed: `{"passenger_name":"MorganSmith"}`, match: false,
		},
		{
			name:     "an extra optional argument is not a wrong call",
			expected: `{"order_id":"GG5"}`, observed: `{"order_id":"GG5","verbose":true}`, match: true,
		},
		{
			name:     "a missing argument is a wrong call",
			expected: `{"origin":"Oak Street","destination":"Gym"}`, observed: `{"origin":"Oak Street"}`, match: false,
		},
		{
			name:     "a number is compared by value as written",
			expected: `{"max_price":150}`, observed: `{"max_price":150}`, match: true,
		},
		{
			name:     "number and numeric string remain different JSON types",
			expected: `{"max_price":150}`, observed: `{"max_price":"150"}`, match: false,
		},
		{
			name:     "boolean and boolean string remain different JSON types",
			expected: `{"value":true}`, observed: `{"value":"true"}`, match: false,
		},
		{
			name:     "punctuation is not globally erased from ordinary text",
			expected: `{"query":"USB-C cable"}`, observed: `{"query":"USBC cable"}`, match: false,
		},
		{
			name:     "underscore normalization is confined to identifier fields",
			expected: `{"doc_type":"driver_license"}`, observed: `{"doc_type":"driver license"}`, match: false,
		},
		{
			name:     "objects and arrays preserve nested JSON types",
			expected: `{"value":{"enabled":true,"levels":[1,"2"]}}`, observed: `{"value":{"enabled":true,"levels":[1,2]}}`, match: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := argumentsMatchHistorical(json.RawMessage(test.expected), json.RawMessage(test.observed))
			if got != test.match {
				t.Fatalf("expected match=%t for %s vs %s", test.match, test.expected, test.observed)
			}
		})
	}
}

func TestIdentifierNormalizationCannotConcealReleasedRequestDisagreement(t *testing.T) {
	// travel_02's released request contains P88990011 while its annotation
	// contains P99990011. Punctuation and ASR spacing are deployment concerns;
	// deleting them must not turn different digit sequences into a match.
	expected := json.RawMessage(`{"doc_type":"passport","doc_number":"P9-9-9-90011"}`)
	requested := json.RawMessage(`{"doc_type":"passport","doc_number":"P 8,8,9,9,0,0,1,1"}`)
	if argumentsMatchHistorical(expected, requested) {
		t.Fatal("identifier normalization concealed travel_02's different digit sequence")
	}

	// The same guard applies to the exact audio disagreement in the second
	// ecommerce_21 recording: B-O-P is not the annotated B-O-B.
	if argumentsMatchHistorical(
		json.RawMessage(`{"order_id":"BOB"}`), json.RawMessage(`{"order_id":"B O P"}`),
	) {
		t.Fatal("identifier normalization concealed ecommerce_21's different final letter")
	}
}

func TestPinnedUpstreamFallbackMatchesPythonTopLevelRulesExactly(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		expected string
		observed string
		match    bool
	}{
		{name: "lower and outer trim", expected: `{"value":"  Mixed Case  "}`, observed: `{"value":"mixed case"}`, match: true},
		{name: "underscore becomes space", expected: `{"value":"driver_license"}`, observed: `{"value":"driver license"}`, match: true},
		{name: "hyphen is retained", expected: `{"value":"BOB-12"}`, observed: `{"value":"BOB12"}`, match: false},
		{name: "internal whitespace is retained", expected: `{"value":"a  b"}`, observed: `{"value":"a b"}`, match: false},
		{name: "any top-level dollar string is skipped", expected: `{"value":"$NOT_A_RESULT"}`, observed: `{"value":false}`, match: true},
		{name: "nested string case is exact", expected: `{"value":{"name":"BOB"}}`, observed: `{"value":{"name":"bob"}}`, match: false},
		{name: "nested underscore is exact", expected: `{"value":["driver_license"]}`, observed: `{"value":["driver license"]}`, match: false},
		{name: "nested dollar is not skipped", expected: `{"value":["$RESULT_0.id"]}`, observed: `{"value":["PROD1"]}`, match: false},
		{name: "Python numeric equality", expected: `{"value":1}`, observed: `{"value":1.0}`, match: true},
		{name: "Python bool numeric equality", expected: `{"value":true}`, observed: `{"value":1}`, match: true},
		{name: "Python float parsing equality", expected: `{"value":0.1}`, observed: `{"value":0.10000000000000001}`, match: true},
		{name: "Python large integer float inequality", expected: `{"value":9007199254740993}`, observed: `{"value":9007199254740992.0}`, match: false},
		{name: "Python duplicate key keeps last value", expected: `{"value":"right"}`, observed: `{"value":"wrong","value":"right"}`, match: true},
		{name: "Python full unicode lowercase", expected: `{"value":"İ"}`, observed: `{"value":"i̇"}`, match: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := argumentsMatchUpstreamFallback(
				json.RawMessage(testCase.expected), json.RawMessage(testCase.observed),
			)
			if got != testCase.match {
				t.Fatalf("match = %t, want %t", got, testCase.match)
			}
		})
	}
}

// Which tool and which arguments are counted separately, so a report can say
// which of the two happened.
func TestScoreSeparatesTheCallFromItsArguments(t *testing.T) {
	expected := []ExpectedCall{{Function: "track_order", Args: json.RawMessage(`{"order_id":"BOB12"}`)}}

	result := scorePinnedUpstreamFallback(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if result.Names != 1 || result.Arguments != 1 || !result.ExactPopulationMatch {
		t.Fatalf("a correct call must score both: %+v", result)
	}

	result = scorePinnedUpstreamFallback(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"B O B 1 2"}`)},
	})
	if result.Names != 1 || result.Arguments != 0 || result.ExactPopulationMatch {
		t.Fatalf("the right tool with the wrong identifier scores the tool only: %+v", result)
	}

	result = scorePinnedUpstreamFallback(expected, []observedCall{
		{Name: "cancel_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if result.Names != 0 || result.Arguments != 0 || result.ExactPopulationMatch {
		t.Fatalf("the wrong tool scores nothing: %+v", result)
	}

	// Repeated calls are paired FIFO. A later correction cannot erase the
	// first wrong effect, and the exact-population pass gate separately rejects
	// the extra call.
	result = scorePinnedUpstreamFallback(expected, []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"WRONG"}`)},
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
	})
	if result.Names != 1 || result.Arguments != 0 || result.ExactPopulationMatch {
		t.Fatalf("a later correct retry must not replace the first paired call: %+v", result)
	}
}

func TestExactPopulationRejectsExtraCalls(t *testing.T) {
	expected := []ExpectedCall{{Function: "track_order", Args: json.RawMessage(`{"order_id":"BOB12"}`)}}
	for _, testCase := range []struct {
		name     string
		observed []observedCall
	}{
		{
			name: "wrong duplicate before expected call",
			observed: []observedCall{
				{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"WRONG"}`)},
				{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
			},
		},
		{
			name: "expected call plus unrelated effect",
			observed: []observedCall{
				{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"BOB12"}`)},
				{Name: "add_to_cart", Arguments: json.RawMessage(`{"product_id":"P1"}`)},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if scorePinnedUpstreamFallback(expected, testCase.observed).ExactPopulationMatch {
				t.Fatal("extra-call fixture passed the production exact-population predicate")
			}
		})
	}
}

func TestReleaseValidityRejectsFailedEffectsWithoutHidingAttempts(t *testing.T) {
	expected := []ExpectedCall{{
		Function: "search_apartments",
		Args:     json.RawMessage(`{"city":"Austin","bedrooms":2}`),
	}}
	arguments := json.RawMessage(`{"city":"Austin","bedrooms":2}`)
	result, succeeded := visibleSimulatorResult("search_apartments", arguments)
	if succeeded || len(result) == 0 {
		t.Fatalf("failed simulator result = %s, succeeded=%t", result, succeeded)
	}
	observed := []observedCall{{
		Name: "search_apartments", Arguments: arguments, Result: result,
	}}
	compatibility := scorePinnedUpstreamFallback(expected, observed)
	if !compatibility.ExactPopulationMatch || compatibility.ArgumentAccuracy != 1 {
		t.Fatalf("pinned fallback comparison = %+v", compatibility)
	}
	release := scoreReleaseValidity(compatibility, observed)
	if release.Passed || release.Observed != 1 || release.Successful != 0 ||
		release.Names != 1 || release.Arguments != 1 {
		t.Fatalf("release validity hid or passed failed attempt: %+v", release)
	}

	extra := append(append([]observedCall(nil), observed...), observedCall{
		Name: "track_order", Arguments: json.RawMessage(`{"order_id":"X"}`),
		Result: json.RawMessage(`{"order_id":"X","shipping_status":"Out for delivery","status":"success"}`),
	})
	release = scoreReleaseValidity(scorePinnedUpstreamFallback(expected, extra), extra)
	if release.Passed || release.Observed != 2 || release.Successful != 1 {
		t.Fatalf("extra failed attempt disappeared from release population: %+v", release)
	}
}

func TestUpstreamProjectionOmitsRejectedBindingButReleaseValidityRetainsAttempt(t *testing.T) {
	expected := []ExpectedCall{{
		Function: "track_order", Args: json.RawMessage(`{"order_id":"ABC1"}`),
	}}
	rejectedArguments := json.RawMessage(`{"city":"Austin","bedrooms":2}`)
	rejectedResult, succeeded := visibleSimulatorResult("search_apartments", rejectedArguments)
	if succeeded {
		t.Fatal("binding-rejected fixture unexpectedly succeeded")
	}
	accepted := simulatedObservedCall(t, "track_order", `{"order_id":"ABC1"}`)
	attempted := []observedCall{{
		Name: "search_apartments", ProposedArguments: rejectedArguments,
		Arguments: rejectedArguments, Result: rejectedResult,
	}, accepted}

	logged := upstreamLoggedCalls(attempted)
	if len(logged) != 1 || logged[0].Name != "track_order" {
		t.Fatalf("upstream-logged projection = %+v, want only accepted track_order", logged)
	}
	upstream := scorePinnedUpstreamFallback(expected, logged)
	if !upstream.ExactPopulationMatch || upstream.Observed != 1 {
		t.Fatalf("historical upstream projection score = %+v", upstream)
	}
	attemptedCompatibility := scorePinnedUpstreamFallback(expected, attempted)
	release := scoreReleaseValidity(attemptedCompatibility, attempted)
	if release.Passed || release.Observed != 2 || release.Expected != 1 || release.Successful != 1 {
		t.Fatalf("authoritative release validity hid rejected attempted effect: %+v", release)
	}
}

func TestReleaseValidityRejectsCrossFunctionReordering(t *testing.T) {
	expected := []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}
	search := simulatedObservedCall(t, "search_products", `{"query":"tablet"}`)
	add := simulatedObservedCall(t, "add_to_cart", `{"product_id":"PROD1","quantity":1}`)

	orderedCompatibility := scorePinnedUpstreamFallback(expected, []observedCall{search, add})
	ordered := scoreReleaseValidity(orderedCompatibility, []observedCall{search, add})
	if !orderedCompatibility.ExactPopulationMatch || !orderedCompatibility.ExpectedOrder || !ordered.Passed {
		t.Fatalf("ordered workflow = compatibility %+v, release %+v", orderedCompatibility, ordered)
	}

	reorderedCalls := []observedCall{add, search}
	reorderedCompatibility := scorePinnedUpstreamFallback(expected, reorderedCalls)
	reordered := scoreReleaseValidity(reorderedCompatibility, reorderedCalls)
	if !reorderedCompatibility.ExactPopulationMatch || reorderedCompatibility.ExpectedOrder ||
		reordered.Ordered || reordered.Passed {
		t.Fatalf("reordered workflow = compatibility %+v, release %+v", reorderedCompatibility, reordered)
	}
}

func TestPinnedUpstreamCompatibilityKeepsOfficialMetricsSeparate(t *testing.T) {
	expected := []ExpectedCall{
		{Function: "track_order", Args: json.RawMessage(`{"order_id":"A"}`)},
		{Function: "track_order", Args: json.RawMessage(`{"order_id":"B"}`)},
	}
	observed := []observedCall{
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"A"}`)},
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"WRONG"}`)},
		{Name: "add_to_cart", Arguments: json.RawMessage(`{}`)},
	}
	result := scorePinnedUpstreamFallback(expected, observed)
	if result.Names != 2 || result.Arguments != 1 ||
		result.ToolSelectionRecall != 1 || result.ToolSelectionPrecision != 0.667 ||
		result.ToolSelectionF1 != 0.8 || result.ArgumentAccuracy != 0.5 ||
		result.ExactPopulationMatch {
		t.Fatalf("pinned compatibility metrics = %+v", result)
	}
}

func TestResultReferencesUseExpectedSlotsAndRequireObservedCausality(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}}
	search := observedCall{
		Name: "search_products", Arguments: json.RawMessage(`{"query":"tablet"}`),
		Result: json.RawMessage(`{"status":"success","products":[{"product_id":"PROD1"}]}`),
	}
	add := observedCall{
		Name: "add_to_cart", Arguments: json.RawMessage(`{"product_id":"PROD1","quantity":1}`),
		Result: json.RawMessage(`{"status":"success"}`),
	}

	static := scorePinnedUpstreamFallback(task.Expected, []observedCall{add, search})
	if static.Names != 2 || static.Arguments != 2 || !static.ExactPopulationMatch {
		t.Fatalf("upstream static compatibility score = %+v, want pass with skipped dynamic value", static)
	}
	semantic := scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{add, search}), pinnedReleasedInventory,
	)
	if semantic.Passed || semantic.Determinate || semantic.Status != semanticStatusFailedClosed ||
		semantic.Arguments != 1 || semantic.Reason == "" {
		t.Fatalf("dependent call observed before its result producer = %+v", semantic)
	}

	// An unrelated extra call before the pair cannot shift $RESULT_0 onto raw
	// observed index zero. The repaired metric still resolves the expected
	// source slot, while the exact-population gate independently rejects the
	// extra effect.
	extra := observedCall{
		Name: "track_order", Arguments: json.RawMessage(`{"order_id":"X"}`),
		Result: json.RawMessage(`{"order_id":"X"}`),
	}
	semantic = scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{extra, search, add}), pinnedReleasedInventory,
	)
	if semantic.Names != 2 || semantic.Arguments != 2 {
		t.Fatalf("expected-slot score with leading extra = %+v, want both expected calls matched", semantic)
	}
	if semantic.Passed {
		t.Fatal("leading extra call passed the exact-population gate")
	}
}

func TestSemanticResultReferenceRequiresProducerResultBeforeDependentCall(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}}
	search := observedCall{
		Name: "search_products", Arguments: json.RawMessage(`{"query":"tablet"}`),
		Result:     json.RawMessage(`{"status":"success","products":[{"product_id":"PROD1"}]}`),
		CallMoment: 0, ResultMoment: 1, CallAtMS: 0, ResultAtMS: 1,
	}
	add := observedCall{
		Name: "add_to_cart", Arguments: json.RawMessage(`{"product_id":"PROD1","quantity":1}`),
		Result:     json.RawMessage(`{"status":"success"}`),
		CallMoment: 2, ResultMoment: 3, CallAtMS: 2, ResultAtMS: 3,
	}
	if result := scoreSemanticRepaired(
		task, []observedCall{search, add}, pinnedReleasedInventory,
	); !result.Passed || result.Status != semanticStatusPassed {
		t.Fatalf("causal producer/result sequence = %+v", result)
	}

	search.ResultMoment = 2
	search.ResultAtMS = 2
	add.CallMoment = 1
	add.CallAtMS = 1
	if result := scoreSemanticRepaired(
		task, []observedCall{search, add}, pinnedReleasedInventory,
	); result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed ||
		!strings.Contains(result.Reason, "producer") {
		t.Fatalf("dependent call before producer result = %+v", result)
	}
}

func TestSemanticConditionalActionRequiresConditionResultBeforeCall(t *testing.T) {
	task := travelConditionalTask()
	search := observedWithResult("search_flights", `{"destination":"Toronto","date":"July 20"}`,
		`{"status":"success","flights":[{"flight_id":"FL123","price":450}]}`)
	update := simulatedObservedCall(t, "update_identity_doc", `{"doc_type":"driver_license","doc_number":"DL88"}`)
	causal := withSequentialTiming([]observedCall{search, update})
	if result := scoreSemanticRepaired(task, causal, pinnedReleasedInventory); !result.Passed {
		t.Fatalf("causal conditional action = %+v", result)
	}

	crossing := withSequentialTiming([]observedCall{search, update})
	crossing[0].ResultMoment = 2
	crossing[0].ResultAtMS = 2
	crossing[1].CallMoment = 1
	crossing[1].CallAtMS = 1
	if result := scoreSemanticRepaired(
		task, crossing, pinnedReleasedInventory,
	); result.Passed || !result.Determinate || result.Status != semanticStatusFailed ||
		!strings.Contains(result.Reason, "condition-source result") {
		t.Fatalf("conditional action before condition result = %+v", result)
	}
}

func TestSemanticScoringFailsClosedWithoutMomentEvidence(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{{
		Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`),
	}}}
	for _, call := range []observedCall{
		simulatedObservedCall(t, "search_products", `{"query":"tablet"}`),
		func() observedCall {
			call := simulatedObservedCall(t, "search_products", `{"query":"tablet"}`)
			call.ResultMoment = 1
			return call
		}(),
	} {
		if result := scoreSemanticRepaired(
			task, []observedCall{call}, pinnedReleasedInventory,
		); result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed ||
			!strings.Contains(result.Reason, "timing evidence") {
			t.Fatalf("missing timing evidence = %+v", result)
		}
	}
}

func TestSemanticResultReferencesRequireSuccessfulProducerResults(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}}
	add := observedCall{
		Name: "add_to_cart", Arguments: json.RawMessage(`{"product_id":"PROD1","quantity":1}`),
		Result: json.RawMessage(`{"status":"success"}`),
	}
	for _, testCase := range []struct {
		name   string
		result json.RawMessage
	}{
		{name: "absent"},
		{name: "failed", result: json.RawMessage(`{"status":"error","products":[{"product_id":"PROD1"}]}`)},
		{name: "malformed", result: json.RawMessage(`{"status":"success","products":`)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			search := observedCall{
				Name: "search_products", Arguments: json.RawMessage(`{"query":"tablet"}`),
				Result: testCase.result,
			}
			result := scoreSemanticRepaired(
				task, withSequentialTiming([]observedCall{search, add}), pinnedReleasedInventory,
			)
			if result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed ||
				result.Arguments != 1 || result.Reason == "" {
				t.Fatalf("semantic score with unusable producer = %+v", result)
			}
		})
	}
}

func TestSemanticResultReferenceWrongEffectiveValueIsDeterminateFailure(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}}
	search := observedCall{
		Name: "search_products", Arguments: json.RawMessage(`{"query":"tablet"}`),
		Result: json.RawMessage(`{"status":"success","products":[{"product_id":"PROD1"}]}`),
	}
	add := observedCall{
		Name: "add_to_cart", Arguments: json.RawMessage(`{"product_id":"WRONG","quantity":1}`),
		Result: json.RawMessage(`{"status":"success"}`),
	}

	result := scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{search, add}), pinnedReleasedInventory,
	)
	if result.Passed || !result.Determinate || result.Status != semanticStatusFailed || result.Arguments != 1 {
		t.Fatalf("wrong effective value semantic score = %+v, want determinate failure", result)
	}
}

func TestSemanticScorerRejectsUnknownErratumPath(t *testing.T) {
	task := Task{ID: "housing_21_66c4f3cb14cbfc4db836bd4e", Expected: []ExpectedCall{
		{Function: "search_apartments", Args: json.RawMessage(`{"bedrooms":3}`)},
		{Function: "calculate_commute", Args: json.RawMessage(`{"origin_address":"$RESULT_0.apartments[0].id","destination_address":"train station","mode":"walking"}`)},
	}}
	search := observedCall{
		Name: "search_apartments", Arguments: json.RawMessage(`{"bedrooms":3}`),
		Result: json.RawMessage(`{"status":"success","city":"Austin","results":[{"id":"APT1","price":1900,"beds":3}]}`),
	}
	commute := observedCall{
		Name: "calculate_commute", Arguments: json.RawMessage(`{"origin_address":"APT1","destination_address":"train station","mode":"walking"}`),
		Result: json.RawMessage(`{"status":"success","duration_mins":12}`),
	}

	result := scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{search, commute}), pinnedReleasedInventory,
	)
	if result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed ||
		result.Reason == "" {
		t.Fatalf("unknown erratum path semantic score = %+v, want failed-closed result", result)
	}
}

func TestCrossFunctionReorderingDoesNotDefeatPerFunctionFIFO(t *testing.T) {
	expected := []ExpectedCall{
		{Function: "track_order", Args: json.RawMessage(`{"order_id":"FIRST"}`)},
		{Function: "get_card_benefits", Args: json.RawMessage(`{"card_type":"gold"}`)},
		{Function: "track_order", Args: json.RawMessage(`{"order_id":"SECOND"}`)},
	}
	observed := []observedCall{
		{Name: "get_card_benefits", Arguments: json.RawMessage(`{"card_type":"gold"}`)},
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"SECOND"}`)},
		{Name: "track_order", Arguments: json.RawMessage(`{"order_id":"FIRST"}`)},
	}
	result := scorePinnedUpstreamFallback(expected, observed)
	if result.Names != 3 || result.Arguments != 1 || result.ExactPopulationMatch {
		t.Fatalf("per-function FIFO score = %+v, want 3 names and only cross-function call", result)
	}
}

func TestScoreEvidenceNamesBothMetricIdentitiesAndPinsErrataVersion(t *testing.T) {
	task := Task{ID: "ecommerce_19_66f59c766e7e22e1f90d08f6", Expected: []ExpectedCall{
		{Function: "search_products", Args: json.RawMessage(`{"query":"tablet"}`)},
		{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":1}`)},
	}}
	observed := []observedCall{
		simulatedObservedCall(t, "search_products", `{"query":"tablet"}`),
		simulatedObservedCall(t, "add_to_cart", `{"product_id":"PROD1","quantity":1}`),
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true}
	attachScores(&outcome, task, withSequentialTiming(observed), pinnedReleasedInventory)
	if !outcome.Passed || outcome.Metrics["release_validity_pass"] != 1 ||
		outcome.Metrics["release_validity_successful_calls"] != 2 ||
		outcome.Metrics["upstream_fallback_argument_accuracy"] != 1 ||
		outcome.Metrics["openrealtime_historical_exact_match"] != 1 ||
		outcome.Metrics["semantic_repaired_pass"] != 1 ||
		outcome.Notes["authoritative_pass_metric"] != releaseValidityScorerIdentity ||
		outcome.Notes["release_validity_scorer"] != releaseValidityScorerIdentity ||
		outcome.Notes["upstream_fallback_scorer"] != upstreamFallbackScorerIdentity ||
		outcome.Notes["openrealtime_historical_scorer"] != openRealtimeHistoricalScorerIdentity ||
		outcome.Notes["semantic_repaired_scorer"] != semanticRepairedScorerIdentity ||
		outcome.Notes["semantic_repaired_errata"] != semanticReferenceErrataIdentity ||
		outcome.Notes["fixture_dispositions"] != fixtureDispositionRegistryIdentity ||
		outcome.Notes["fixture_disposition"] != string(fixtureStatusDeterminate) ||
		outcome.Notes["upstream_tool_catalog"] != upstreamToolCatalogIdentity ||
		outcome.Notes["semantic_repaired_status"] != semanticStatusPassed {
		t.Fatalf("scored outcome = %+v", outcome)
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{
		releaseValidityScorerIdentity, upstreamFallbackScorerIdentity,
		openRealtimeHistoricalScorerIdentity, semanticRepairedScorerIdentity,
		semanticReferenceErrataIdentity, fixtureDispositionRegistryIdentity,
		simulatorIdentity, harnessIdentity, upstreamToolCatalogIdentity,
	} {
		if !json.Valid(encoded) || !containsBytes(encoded, []byte(identity)) {
			t.Fatalf("serialized score evidence omits identity %q: %s", identity, encoded)
		}
	}
}

func TestSummariseUsesClosedFailureCategories(t *testing.T) {
	metrics := func(expected, observed, names, arguments, successful float64, ordered, passed bool) map[string]float64 {
		pass := 0.0
		if passed {
			pass = 1
		}
		order := 0.0
		if ordered {
			order = 1
		}
		return map[string]float64{
			"release_validity_expected_calls":   expected,
			"release_validity_observed_calls":   observed,
			"release_validity_name_matches":     names,
			"release_validity_argument_matches": arguments,
			"release_validity_successful_calls": successful,
			"release_validity_ordered":          order,
			"release_validity_pass":             pass,
		}
	}
	result := bench.Result{Tasks: []bench.TaskOutcome{
		{
			ID: "extra", Completed: true, Passed: false,
			Metrics: metrics(1, 2, 1, 1, 2, true, false),
		},
		{
			ID: "inconsistent", Completed: true, Passed: true,
			Metrics: metrics(1, 2, 1, 1, 2, true, true),
		},
		{
			ID: "exact", Completed: true, Passed: true,
			Metrics: metrics(1, 1, 1, 1, 1, true, true),
		},
		{ID: "wrong-arguments", Completed: true, Metrics: metrics(1, 1, 1, 0, 1, true, false)},
		{ID: "failed-call", Completed: true, Metrics: metrics(1, 1, 1, 1, 0, true, false)},
		{ID: "reordered", Completed: true, Metrics: metrics(2, 2, 2, 2, 2, false, false)},
		{ID: "wrong-tool", Completed: true, Metrics: metrics(1, 1, 0, 0, 1, true, false)},
		{ID: "missing", Completed: true, Metrics: metrics(2, 1, 1, 1, 1, false, false)},
		{ID: "none", Completed: true, Metrics: metrics(1, 0, 0, 0, 0, false, false)},
		{ID: "missing-metrics", Completed: true},
		{ID: "incomplete", Completed: false},
	}}
	breakdown := Summarise(result)
	if breakdown.Tasks != 10 || breakdown.CalledWithRightArguments != 1 ||
		breakdown.SpellingFailures != 1 || breakdown.WrongTool != 1 ||
		breakdown.MissingCalls != 1 || breakdown.NoCall != 1 ||
		breakdown.ExtraCalls != 1 || breakdown.FailedCalls != 1 || breakdown.ReorderedCalls != 1 ||
		breakdown.InvalidScore != 2 {
		t.Fatalf("breakdown = %+v", breakdown)
	}
	classified := breakdown.CalledWithRightArguments + breakdown.SpellingFailures +
		breakdown.WrongTool + breakdown.MissingCalls + breakdown.NoCall +
		breakdown.ExtraCalls + breakdown.FailedCalls + breakdown.ReorderedCalls + breakdown.InvalidScore
	if classified != breakdown.Tasks {
		t.Fatalf("classified completed rows = %d, want %d: %+v", classified, breakdown.Tasks, breakdown)
	}
	if breakdown.CalledRightTool != 4 {
		t.Fatalf("right-tool aggregate = %d, want exact, failed, reordered, and wrong-argument rows", breakdown.CalledRightTool)
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		match := true
		for offset := range needle {
			match = match && haystack[index+offset] == needle[offset]
		}
		if match {
			return true
		}
	}
	return false
}

// Annotation classification is kept separate from provider-facing schema
// construction. It audits known upstream metadata discrepancies without
// letting those discrepancies mutate the pinned callable contract.
func TestAnnotationJSONTypeClassification(t *testing.T) {
	for _, item := range []struct {
		value string
		want  string
	}{
		{`"K2"`, "string"}, {`200`, "number"}, {`2`, "number"}, {`1.5`, "number"},
		{`true`, "boolean"}, {`["a"]`, "array"}, {`{"a":1}`, "object"},
		{`null`, "null"},
	} {
		got, err := jsonTypeOf([]byte(item.value))
		if err != nil {
			t.Errorf("jsonTypeOf(%s): %v", item.value, err)
			continue
		}
		if got != item.want {
			t.Errorf("%s declared as %q, want %q", item.value, got, item.want)
		}
	}
	if _, err := jsonTypeOf(nil); err == nil {
		t.Fatal("empty JSON value was accepted")
	}
}
