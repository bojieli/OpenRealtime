package fdbv3

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/testgate"
)

func TestReleasedEcommerceSingletonReferenceRepairIsNarrow(t *testing.T) {
	task := Task{
		ID: "ecommerce_18_62a885d5b6af18b3d4579e1b",
		Expected: []ExpectedCall{
			{Function: "search_products", Args: json.RawMessage(`{"query":"gaming mouse"}`)},
			{Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"$RESULT_0.cheapest_product_id","quantity":1}`)},
			{Function: "track_order", Args: json.RawMessage(`{"order_id":"PO999"}`)},
		},
	}
	observed := withSequentialTiming([]observedCall{
		simulatedObservedCall(t, "search_products", `{"query":"gaming mouse"}`),
		simulatedObservedCall(t, "add_to_cart", `{"product_id":"PROD1","quantity":1}`),
		simulatedObservedCall(t, "track_order", `{"order_id":"PO999"}`),
	})
	result := scoreSemanticRepaired(task, observed, pinnedReleasedInventory)
	if !result.Passed || result.Status != semanticStatusPassed ||
		result.Names != len(task.Expected) || result.Arguments != len(task.Expected) {
		t.Fatalf("semantic score = %+v", result)
	}

	for _, products := range []string{`[]`,
		`[{"product_id":"PROD1"},{"product_id":"PROD2"}]`} {
		resultJSON := json.RawMessage(`{"status":"success","products":` + products + `}`)
		prior := []observedCall{{Result: resultJSON}}
		if value, err := resolveSemanticResultReference(
			task.ID, "$RESULT_0.cheapest_product_id", prior, pinnedReleasedInventory,
		); err == nil {
			t.Fatalf("non-singleton products %s resolved to %#v", products, value)
		}
		nonSingleton := withSequentialTiming([]observedCall{
			observedWithResult("search_products", `{"query":"gaming mouse"}`, string(resultJSON)),
			simulatedObservedCall(t, "add_to_cart", `{"product_id":"PROD1","quantity":1}`),
			simulatedObservedCall(t, "track_order", `{"order_id":"PO999"}`),
		})
		semantic := scoreSemanticRepaired(task, nonSingleton, pinnedReleasedInventory)
		if semantic.Passed || semantic.Determinate || semantic.Status != semanticStatusFailedClosed ||
			!strings.Contains(semantic.Reason, "exactly one result item") {
			t.Fatalf("non-singleton semantic result for %s = %+v", products, semantic)
		}
	}
}

func TestReleasedHousingAddressReferencesAreNeverRewrittenToApartmentIDs(t *testing.T) {
	for _, testCase := range []struct {
		task      string
		reference string
	}{
		{"housing_18_66c4f3cb14cbfc4db836bd4e", "$RESULT_0.cheapest_apartment_address"},
		{"housing_20_62a885d5b6af18b3d4579e1b", "$RESULT_0.apartments[0].address"},
		{"housing_21_66c4f3cb14cbfc4db836bd4e", "$RESULT_0.apartments[0].address"},
		{"housing_22_695bd157114f0d2317f88617", "$RESULT_1.apartments[0].address"},
		{"housing_24_65e8cf8f4c7424fa062e54a3", "$RESULT_1.apartments[0].address"},
		{"housing_24_69a9cf80f4d7668d5c815038", "$RESULT_1.apartments[0].address"},
	} {
		t.Run(testCase.task, func(t *testing.T) {
			disposition, err := pinnedFixtureDisposition(testCase.task, pinnedReleasedInventory)
			if err != nil || disposition.Status != fixtureStatusContractInvalid {
				t.Fatalf("fixture disposition = %+v, %v", disposition, err)
			}
			index, err := resultReferenceIndex(testCase.reference)
			if err != nil {
				t.Fatal(err)
			}
			prior := make([]observedCall, index+1)
			prior[index] = simulatedObservedCall(
				t, "search_apartments", `{"city":"Austin","bedrooms":2,"max_price":2000}`,
			)
			value, err := resolveSemanticResultReference(
				testCase.task, testCase.reference, prior, pinnedReleasedInventory,
			)
			if err == nil || value == "APT1" {
				t.Fatalf("housing address reference resolved to %#v, err=%v", value, err)
			}
			key := pinnedReferenceErratumKey(testCase.task, testCase.reference)
			if _, found := pinnedReferenceErrata[key]; found {
				t.Fatalf("indefensible housing ID rewrite remains registered: %+v", key)
			}
		})
	}
}

func TestEveryReleasedReferenceIsEitherCanonicalOrExactPinnedErratum(t *testing.T) {
	root := releasedDatasetRoot(t)
	tasks, err := loadDataset(root, 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	references, repaired, canonical, contractInvalid := 0, 0, 0, 0
	for _, task := range tasks {
		for _, call := range task.Expected {
			arguments, err := decodeJSONObject(call.Args)
			if err != nil {
				t.Fatalf("%s %s: %v", task.ID, call.Function, err)
			}
			for _, reference := range resultReferences(arguments) {
				references++
				index, err := resultReferenceIndex(reference)
				if err != nil {
					t.Fatalf("%s reference %q: %v", task.ID, reference, err)
				}
				prior := make([]observedCall, index+1)
				if strings.Contains(reference, "product") {
					prior[index] = simulatedObservedCall(t, "search_products", `{"query":"fixture"}`)
				} else {
					prior[index] = simulatedObservedCall(t, "search_apartments", `{"city":"Austin","bedrooms":2,"max_price":2000}`)
				}
				value, resolveErr := resolveSemanticResultReference(
					task.ID, reference, prior, pinnedReleasedInventory,
				)
				key := pinnedReferenceErratumKey(task.ID, reference)
				if _, found := pinnedReferenceErrata[key]; found {
					if resolveErr != nil || value != "PROD1" {
						t.Fatalf("%s exact erratum %q resolved to %#v: %v", task.ID, reference, value, resolveErr)
					}
					repaired++
				} else if resolveErr == nil {
					if value != "PROD1" && value != "APT1" {
						t.Fatalf("%s canonical reference %q resolved to %#v", task.ID, reference, value)
					}
					canonical++
				} else {
					disposition, err := pinnedFixtureDisposition(task.ID, pinnedReleasedInventory)
					if err != nil || disposition.Status != fixtureStatusContractInvalid ||
						!dispositionContainsReference(disposition, reference) {
						t.Fatalf("%s unresolved reference %q lacks exact disposition: %+v, %v; resolve=%v",
							task.ID, reference, disposition, err, resolveErr)
					}
					contractInvalid++
				}
			}
		}
	}
	if references != 13 || repaired != 1 || canonical != 6 || contractInvalid != 6 ||
		len(pinnedReferenceErrata) != 1 {
		t.Fatalf("reference audit: total=%d repaired=%d canonical=%d contract-invalid=%d errata=%d",
			references, repaired, canonical, contractInvalid, len(pinnedReferenceErrata))
	}
}

func dispositionContainsReference(disposition fixtureDisposition, reference string) bool {
	for _, defect := range disposition.Defects {
		if defect.Code == fixtureDefectMissingResultPath && defect.Reference == reference {
			return true
		}
	}
	return false
}

func TestSemanticErrataFailClosedOutsideExactPinnedIdentity(t *testing.T) {
	prior := []observedCall{simulatedObservedCall(
		t, "search_apartments", `{"city":"Austin","bedrooms":2,"max_price":2000}`,
	)}
	for _, testCase := range []struct {
		name      string
		task      string
		reference string
		inventory releasedDatasetInventory
	}{
		{
			name:      "same legacy path on another pinned task",
			task:      "housing_19_62a885d5b6af18b3d4579e1b",
			reference: "$RESULT_0.apartments[0].address", inventory: pinnedReleasedInventory,
		},
		{
			name:      "unknown legacy projection on errata task",
			task:      "housing_21_66c4f3cb14cbfc4db836bd4e",
			reference: "$RESULT_0.apartments[0].id", inventory: pinnedReleasedInventory,
		},
		{
			name:      "wrong artifact digest",
			task:      "housing_21_66c4f3cb14cbfc4db836bd4e",
			reference: "$RESULT_0.apartments[0].address",
			inventory: releasedDatasetInventory{
				Revision: pinnedReleasedRevision, ArtifactDigest: strings.Repeat("0", 64),
				TaskNames: pinnedReleasedInventory.TaskNames,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := resolveSemanticResultReference(
				testCase.task, testCase.reference, prior, testCase.inventory,
			); err == nil {
				t.Fatal("unknown or unpinned erratum was accepted")
			}
		})
	}
}

func TestResultReferencesRejectMissingPathsIndexesFutureCallsAndCorruptResults(t *testing.T) {
	prior := []observedCall{{Result: json.RawMessage(`{"products":[{"product_id":"PROD1"}]}`)}}
	for _, testCase := range []struct {
		name      string
		reference string
		prior     []observedCall
		want      string
	}{
		{name: "missing field", reference: "$RESULT_0.products[0].missing", prior: prior, want: "no field"},
		{name: "missing index", reference: "$RESULT_0.products[1].product_id", prior: prior, want: "exceeds length"},
		{name: "future call", reference: "$RESULT_1.products[0].product_id", prior: prior, want: "not an earlier"},
		{name: "negative index", reference: "$RESULT_-1.products", prior: prior, want: "invalid"},
		{name: "malformed path", reference: "$RESULT_0.products[x]", prior: prior, want: "invalid"},
		{name: "corrupt result", reference: "$RESULT_0.products", prior: []observedCall{{Result: json.RawMessage(`{"products":`)}}, want: "decode"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := resolveResultReference(testCase.reference, testCase.prior); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("resolve error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestPinnedSimulatorReturnsExactOfficialValues(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args string
		want string
	}{
		{name: "search_flights", args: `{"destination":"LHR","date":"2026-08-20"}`, want: `{"flights":[{"date":"2026-08-20","destination":"LHR","flight_id":"FL123","price":450}],"status":"success"}`},
		{name: "book_flight", args: `{"passenger_name":"Morgan Smith"}`, want: `{"booking_ref":"B789","passenger":"Morgan Smith","status":"success"}`},
		{name: "update_identity_doc", args: `{"doc_type":"passport","doc_number":"AB123456"}`, want: `{"masked_number":"3456","status":"success","updated_doc":"passport"}`},
		{name: "get_card_benefits", args: `{"card_type":"platinum"}`, want: `{"benefits":["2% Cashback","No Foreign Transaction Fee"],"card_type":"platinum","status":"success"}`},
		{name: "get_exchange_rate", args: `{"amount":100,"from_currency":"EUR","to_currency":"USD"}`, want: `{"converted_amount":110.00000000000001,"rate":1.1,"status":"success"}`},
		{name: "modify_autopay", args: `{"bill_type":"utilities","source_account":"checking"}`, want: `{"autopay_enabled":true,"bill":"utilities","source":"checking","status":"success"}`},
		{name: "search_apartments", args: `{"city":"Austin","bedrooms":2,"max_price":2000}`, want: `{"city":"Austin","results":[{"beds":2,"id":"APT1","price":1900}],"status":"success"}`},
		{name: "calculate_commute", args: `{"origin_address":"APT1","destination_address":"office"}`, want: `{"duration_mins":25,"mode":"driving","status":"success"}`},
		{name: "update_search_filter", args: `{"filter_name":"pets_allowed","value":"true"}`, want: `{"filter_updated":"pets_allowed","new_value":"true","status":"success"}`},
		{name: "track_order", args: `{"order_id":"ABC1"}`, want: `{"order_id":"ABC1","shipping_status":"Out for delivery","status":"success"}`},
		{name: "search_products", args: `{"query":"coffee maker","max_price":50}`, want: `{"products":[{"name":"coffee maker Premium","price":40,"product_id":"PROD1"}],"status":"success"}`},
		{name: "search_products", args: `{"query":"coffee maker","max_price":null}`, want: `{"products":[{"name":"coffee maker Premium","price":99.99,"product_id":"PROD1"}],"status":"success"}`},
		{name: "add_to_cart", args: `{"product_id":"PROD1","quantity":2}`, want: `{"cart_total":199.98,"product_id":"PROD1","quantity":2,"status":"success"}`},
		{name: "unknown_tool", args: `{not-json`, want: `{"message":"Unknown function: unknown_tool","status":"error"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := simulateToolCall(testCase.name, json.RawMessage(testCase.args))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != testCase.want {
				t.Fatalf("result = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestPinnedCallableBindingAppliesDefaultsCoercionAndExtraIgnore(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		proposed string
		want     string
	}{
		{
			name: "calculate_commute", proposed: `{"origin_address":"A","destination_address":"B"}`,
			want: `{"destination_address":"B","mode":"driving","origin_address":"A"}`,
		},
		{
			name: "search_products", proposed: `{"query":"gift","category":"electronics"}`,
			want: `{"max_price":null,"query":"gift"}`,
		},
		{
			name: "search_products", proposed: `{"query":"mouse","max_price":"50"}`,
			want: `{"max_price":50,"query":"mouse"}`,
		},
		{
			name: "add_to_cart", proposed: `{"product_id":"P"}`,
			want: `{"product_id":"P","quantity":1}`,
		},
		{
			name: "add_to_cart", proposed: `{"product_id":"P","quantity":"2"}`,
			want: `{"product_id":"P","quantity":2}`,
		},
	} {
		t.Run(testCase.name+"/"+testCase.proposed, func(t *testing.T) {
			effective, err := effectiveCallableArguments(testCase.name, json.RawMessage(testCase.proposed))
			if err != nil {
				t.Fatal(err)
			}
			if string(effective) != testCase.want {
				t.Fatalf("effective arguments = %s, want %s", effective, testCase.want)
			}
		})
	}
}

func TestTranscriptRetainsProposalButScoresEffectiveCallableArguments(t *testing.T) {
	proposal := json.RawMessage(`{"product_id":"P"}`)
	result, succeeded := visibleSimulatorResult("add_to_cart", proposal)
	if !succeeded {
		t.Fatalf("defaulted call failed: %s", result)
	}
	observed, err := observedCallsFromTranscript(bench.Transcript{Moments: []bench.Moment{
		{AtMS: 1, Kind: bench.MomentToolCall, CallID: "call", Name: "add_to_cart", Arguments: string(proposal)},
		{AtMS: 2, Kind: bench.MomentToolResult, CallID: "call", Name: "add_to_cart", Text: string(result)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || string(observed[0].ProposedArguments) != string(proposal) ||
		string(observed[0].Arguments) != `{"product_id":"P","quantity":1}` {
		t.Fatalf("proposal/effective evidence = %+v", observed)
	}
	score := scorePinnedUpstreamFallback([]ExpectedCall{{
		Function: "add_to_cart", Args: json.RawMessage(`{"product_id":"P","quantity":1}`),
	}}, observed)
	if !score.ExactPopulationMatch {
		t.Fatalf("defaulted effective call did not match upstream logger semantics: %+v", score)
	}
}

func TestTranscriptReconstructionRetainsSuccessfulAndFailedAttempts(t *testing.T) {
	goodArgs := json.RawMessage(`{"order_id":"ABC1"}`)
	goodResult, good := visibleSimulatorResult("track_order", goodArgs)
	badArgs := json.RawMessage(`{"city":"Austin","bedrooms":2}`)
	badResult, bad := visibleSimulatorResult("search_apartments", badArgs)
	if !good || bad {
		t.Fatalf("fixture success flags: good=%t bad=%t", good, bad)
	}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 1, Kind: bench.MomentToolCall, CallID: "good", Name: "track_order", Arguments: string(goodArgs)},
		{AtMS: 2, Kind: bench.MomentToolResult, CallID: "good", Name: "track_order", Text: string(goodResult)},
		{AtMS: 3, Kind: bench.MomentToolCall, CallID: "failed", Name: "search_apartments", Arguments: string(badArgs)},
		{AtMS: 4, Kind: bench.MomentToolResult, CallID: "failed", Name: "search_apartments", Text: string(badResult)},
	}}
	observed, err := observedCallsFromTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || !observedCallSucceeded(observed[0]) || observedCallSucceeded(observed[1]) ||
		observed[1].Name != "search_apartments" || string(observed[1].Arguments) != string(badArgs) ||
		string(observed[1].Result) != string(badResult) ||
		observed[0].CallMoment != 0 || observed[0].ResultMoment != 1 ||
		observed[0].CallAtMS != 1 || observed[0].ResultAtMS != 2 ||
		observed[1].CallMoment != 2 || observed[1].ResultMoment != 3 ||
		observed[1].CallAtMS != 3 || observed[1].ResultAtMS != 4 {
		t.Fatalf("reconstructed attempts = %+v", observed)
	}
}

func TestTranscriptReconstructionRejectsMissingOrInconsistentEvidence(t *testing.T) {
	args := `{"order_id":"ABC1"}`
	result, _ := visibleSimulatorResult("track_order", json.RawMessage(args))
	call := bench.Moment{AtMS: 1, Kind: bench.MomentToolCall, CallID: "call-1", Name: "track_order", Arguments: args}
	validResult := bench.Moment{AtMS: 2, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: string(result)}
	for _, testCase := range []struct {
		name    string
		moments []bench.Moment
	}{
		{name: "missing result", moments: []bench.Moment{call}},
		{name: "orphan result", moments: []bench.Moment{validResult}},
		{name: "duplicate call id", moments: []bench.Moment{call, call}},
		{name: "reused call id", moments: []bench.Moment{call, validResult, call}},
		{name: "wrong result name", moments: []bench.Moment{call, {
			AtMS: 2, Kind: bench.MomentToolResult, CallID: "call-1", Name: "add_to_cart", Text: string(result),
		}}},
		{name: "forged result", moments: []bench.Moment{call, {
			AtMS: 2, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: `{"status":"success"}`,
		}}},
		{name: "result before call time", moments: []bench.Moment{call, {
			AtMS: 0.5, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: string(result),
		}}},
		{name: "result has no elapsed timing evidence", moments: []bench.Moment{call, {
			AtMS: 1, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: string(result),
		}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if observed, err := observedCallsFromTranscript(bench.Transcript{Moments: testCase.moments}); err == nil {
				t.Fatalf("accepted inconsistent evidence: %+v", observed)
			}
		})
	}
}

func TestTranscriptReconstructionPreservesMalformedAttemptArguments(t *testing.T) {
	arguments := `{"order_id":`
	result, succeeded := visibleSimulatorResult("track_order", json.RawMessage(arguments))
	if succeeded {
		t.Fatal("malformed argument call unexpectedly succeeded")
	}
	observed, err := observedCallsFromTranscript(bench.Transcript{Moments: []bench.Moment{
		{AtMS: 1, Kind: bench.MomentToolCall, CallID: "call-1", Name: "track_order", Arguments: arguments},
		{AtMS: 2, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: string(result)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || string(observed[0].Arguments) != arguments || observedCallSucceeded(observed[0]) {
		t.Fatalf("malformed attempt was hidden: %+v", observed)
	}
}

func TestProviderSearchResultsContainNoEvaluatorAliases(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		args      string
		forbidden []string
	}{
		{
			name: "search_apartments", args: `{"city":"Austin","bedrooms":2,"max_price":2000}`,
			forbidden: []string{"apartments", "cheapest_apartment_address"},
		},
		{
			name: "search_products", args: `{"query":"mouse"}`,
			forbidden: []string{"cheapest_product_id"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := simulateToolCall(testCase.name, json.RawMessage(testCase.args))
			if err != nil {
				t.Fatal(err)
			}
			object, err := decodeJSONObject(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range testCase.forbidden {
				if _, found := object[key]; found {
					t.Fatalf("provider-visible result contains evaluator alias %q: %s", key, result)
				}
			}
		})
	}
}

func TestToolSimulatorRetainsExactRawProviderResult(t *testing.T) {
	simulator := &toolSimulator{}
	returned, err := simulator.Respond(
		"search_products", json.RawMessage(`{"query":"mouse","max_price":50}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := simulator.Snapshot()
	if len(snapshot) != 1 || !bytes.Equal(returned, snapshot[0].Result) {
		t.Fatalf("returned=%s retained=%s", returned, snapshot[0].Result)
	}
	returned[0] = '!'
	snapshot[0].Result[0] = '?'
	again := simulator.Snapshot()
	if len(again) != 1 || again[0].Result[0] != '{' {
		t.Fatalf("retained result aliases caller or snapshot memory: %q", again[0].Result)
	}
}

func TestPinnedSimulatorRejectsMissingRequiredAndInvalidArguments(t *testing.T) {
	for _, testCase := range []struct{ name, args string }{
		{name: "search_apartments", args: `{"bedrooms":2,"max_price":2000}`},
		{name: "search_apartments", args: `{"city":"Austin","max_price":2000}`},
		{name: "search_apartments", args: `{"city":"Austin","bedrooms":2}`},
		{name: "track_order", args: `{"order_id":3}`},
		{name: "add_to_cart", args: `{"product_id":"P","quantity":1.5}`},
		{name: "search_products", args: `{"query":"mouse","max_price":{}}`},
		{name: "update_search_filter", args: `{"filter_name":"pets_allowed","value":true}`},
		{name: "track_order", args: `{}`},
	} {
		t.Run(testCase.name+"/"+testCase.args, func(t *testing.T) {
			if _, err := simulateToolCall(testCase.name, json.RawMessage(testCase.args)); err == nil {
				t.Fatalf("simulateToolCall(%s, %s) accepted invalid arguments", testCase.name, testCase.args)
			}
		})
	}
}

func TestSemanticConditionalBranchesUseActualSuccessfulResults(t *testing.T) {
	t.Run("ecommerce", func(t *testing.T) {
		task := ecommerceConditionalTask()
		cheapSearch := observedWithResult("search_products", `{"query":"coffee maker"}`,
			`{"status":"success","products":[{"product_id":"PROD1","price":40}]}`)
		expensiveSearch := observedWithResult("search_products", `{"query":"coffee maker"}`,
			`{"status":"success","products":[{"product_id":"PROD1","price":99.99}]}`)
		add := simulatedObservedCall(t, "add_to_cart", `{"product_id":"PROD1","quantity":2}`)
		track := simulatedObservedCall(t, "track_order", `{"order_id":"WWW"}`)
		assertSemanticPass(t, task, []observedCall{cheapSearch, add})
		assertSemanticPass(t, task, []observedCall{expensiveSearch, track})
		assertSemanticFail(t, task, []observedCall{cheapSearch, track})
		assertSemanticFail(t, task, []observedCall{expensiveSearch, add})
		assertSemanticFail(t, task, []observedCall{cheapSearch, add, track})
		assertSemanticFailedClosed(t, task, []observedCall{add, cheapSearch})
	})

	t.Run("housing", func(t *testing.T) {
		task := housingConditionalTask()
		search := simulatedObservedCall(t, "search_apartments", `{"city":"Austin","bedrooms":2,"max_price":2000}`)
		longCommute := observedWithResult("calculate_commute",
			`{"origin_address":"APT1","destination_address":"my office","mode":"driving"}`,
			`{"status":"success","duration_mins":25,"mode":"driving"}`)
		update := observedWithResult("update_search_filter", `{"filter_name":"max_price","value":2500}`,
			`{"status":"success","filter_updated":"max_price","new_value":2500}`)
		result := scoreSemanticRepaired(
			task, withSequentialTiming([]observedCall{search, longCommute, update}), pinnedReleasedInventory,
		)
		if result.Passed || result.Determinate || result.Status != semanticStatusContractInvalid ||
			!strings.Contains(result.Reason, string(fixtureDefectMissingRequiredArgument)) ||
			!strings.Contains(result.Reason, string(fixtureDefectMissingResultPath)) {
			t.Fatalf("invented apartment arguments converted invalid fixture into %+v", result)
		}
	})

	t.Run("travel", func(t *testing.T) {
		task := travelConditionalTask()
		cheapSearch := observedWithResult("search_flights", `{"destination":"Toronto","date":"July 20"}`,
			`{"status":"success","flights":[{"flight_id":"FL123","price":250}]}`)
		expensiveSearch := observedWithResult("search_flights", `{"destination":"Toronto","date":"July 20"}`,
			`{"status":"success","flights":[{"flight_id":"FL123","price":450}]}`)
		book := simulatedObservedCall(t, "book_flight", `{"passenger_name":"Riley Kim"}`)
		update := simulatedObservedCall(t, "update_identity_doc", `{"doc_type":"driver_license","doc_number":"DL88"}`)
		assertSemanticPass(t, task, []observedCall{cheapSearch, book})
		assertSemanticPass(t, task, []observedCall{expensiveSearch, update})
		assertSemanticFail(t, task, []observedCall{cheapSearch, update})
		assertSemanticFail(t, task, []observedCall{expensiveSearch, book})
		assertSemanticFail(t, task, []observedCall{cheapSearch, book, update})
		assertSemanticFail(t, task, []observedCall{book, cheapSearch})
	})
}

func TestStaticAndSemanticConditionalMetricIdentitiesDoNotCollapse(t *testing.T) {
	task := travelConditionalTask()
	search := observedWithResult("search_flights", `{"destination":"Toronto","date":"July 20"}`,
		`{"status":"success","flights":[{"flight_id":"FL123","price":450}]}`)
	update := simulatedObservedCall(t, "update_identity_doc", `{"doc_type":"driver_license","doc_number":"DL88"}`)
	semantic := scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{search, update}), pinnedReleasedInventory,
	)
	static := scorePinnedUpstreamFallback(task.Expected, []observedCall{search, update})
	if !semantic.Passed || static.ExactPopulationMatch {
		t.Fatalf("semantic=%+v upstream-static=%+v", semantic, static)
	}

	book := simulatedObservedCall(t, "book_flight", `{"passenger_name":"Riley Kim"}`)
	static = scorePinnedUpstreamFallback(task.Expected, []observedCall{search, book, update})
	semantic = scoreSemanticRepaired(
		task, withSequentialTiming([]observedCall{search, book, update}), pinnedReleasedInventory,
	)
	if !static.ExactPopulationMatch || semantic.Passed {
		t.Fatalf("contradictory static fixture must diverge: semantic=%+v upstream-static=%+v", semantic, static)
	}
}

func TestSemanticConditionalEvaluationFailsClosedOnBadProducerResults(t *testing.T) {
	task := ecommerceConditionalTask()
	for _, testCase := range []struct {
		name   string
		result string
	}{
		{name: "absent", result: ""},
		{name: "failed", result: `{"status":"error","products":[{"price":40}]}`},
		{name: "malformed", result: `{"status":"success","products":`},
		{name: "missing price", result: `{"status":"success","products":[{}]}`},
		{name: "wrong price type", result: `{"status":"success","products":[{"price":"40"}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			search := observedWithResult("search_products", `{"query":"coffee maker"}`, testCase.result)
			result := scoreSemanticRepaired(
				task, withSequentialTiming([]observedCall{search}), pinnedReleasedInventory,
			)
			if result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed || result.Reason == "" {
				t.Fatalf("semantic result = %+v", result)
			}
		})
	}
}

func TestFinanceConditionalFixtureIsExplicitlyIndeterminate(t *testing.T) {
	task := Task{
		ID: "finance_20_66c4f3cb14cbfc4db836bd4e",
		Expected: []ExpectedCall{
			{Function: "get_exchange_rate", Args: json.RawMessage(`{"amount":1000,"from_currency":"USD","to_currency":"EUR"}`)},
			{Function: "modify_autopay", Args: json.RawMessage(`{"bill_type":"mortgage","source_account":"checking"}`), Note: "Only if rate is favorable"},
		},
	}
	observed := []observedCall{
		simulatedObservedCall(t, "get_exchange_rate", `{"amount":1000,"from_currency":"USD","to_currency":"EUR"}`),
		simulatedObservedCall(t, "modify_autopay", `{"bill_type":"mortgage","source_account":"checking"}`),
	}
	result := scoreSemanticRepaired(task, observed, pinnedReleasedInventory)
	if result.Passed || result.Determinate || result.Status != semanticStatusIndeterminateFixture ||
		!strings.Contains(result.Reason, string(fixtureDefectUndefinedCondition)) {
		t.Fatalf("semantic result = %+v", result)
	}
}

func TestReleasedConditionalAnnotationInventoryIsExact(t *testing.T) {
	root := releasedDatasetRoot(t)
	tasks, err := loadDataset(root, 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	notes, directories := 0, map[string]int{}
	for _, task := range tasks {
		for _, call := range task.Expected {
			if call.Note != "" {
				notes++
				directories[task.ID]++
			}
		}
	}
	if notes != 5 || len(directories) != 4 ||
		directories["ecommerce_20_66c4f3cb14cbfc4db836bd4e"] != 1 ||
		directories["finance_20_66c4f3cb14cbfc4db836bd4e"] != 1 ||
		directories["housing_20_62a885d5b6af18b3d4579e1b"] != 1 ||
		directories["travel_20_62a885d5b6af18b3d4579e1b"] != 2 {
		t.Fatalf("conditional inventory: notes=%d directories=%v", notes, directories)
	}
}

func ecommerceConditionalTask() Task {
	return Task{
		ID: "ecommerce_20_66c4f3cb14cbfc4db836bd4e",
		Expected: []ExpectedCall{
			{Function: "search_products", Args: json.RawMessage(`{"query":"coffee maker"}`)},
			{
				Function: "add_to_cart",
				Args:     json.RawMessage(`{"product_id":"$RESULT_0.products[0].product_id","quantity":2}`),
				Note:     "Only if price < 50, otherwise track order WWW",
			},
		},
	}
}

func housingConditionalTask() Task {
	return Task{
		ID: "housing_20_62a885d5b6af18b3d4579e1b",
		Expected: []ExpectedCall{
			{Function: "search_apartments", Args: json.RawMessage(`{"city":"Austin","max_price":2000}`)},
			{Function: "calculate_commute", Args: json.RawMessage(`{"origin_address":"$RESULT_0.apartments[0].address","destination_address":"my office","mode":"driving"}`)},
			{
				Function: "update_search_filter", Args: json.RawMessage(`{"filter_name":"max_price","value":2500}`),
				Note: "Only if commute > 15 min",
			},
		},
	}
}

func travelConditionalTask() Task {
	return Task{
		ID: "travel_20_62a885d5b6af18b3d4579e1b",
		Expected: []ExpectedCall{
			{Function: "search_flights", Args: json.RawMessage(`{"destination":"Toronto","date":"July 20"}`)},
			{Function: "book_flight", Args: json.RawMessage(`{"passenger_name":"Riley Kim"}`), Note: "Only if flight < $300"},
			{Function: "update_identity_doc", Args: json.RawMessage(`{"doc_type":"driver_license","doc_number":"DL88"}`), Note: "Only if no cheap flight"},
		},
	}
}

func assertSemanticPass(t *testing.T, task Task, observed []observedCall) {
	t.Helper()
	result := scoreSemanticRepaired(task, withSequentialTiming(observed), pinnedReleasedInventory)
	if !result.Passed || !result.Determinate || result.Status != semanticStatusPassed {
		t.Fatalf("semantic result = %+v", result)
	}
}

func assertSemanticFail(t *testing.T, task Task, observed []observedCall) {
	t.Helper()
	result := scoreSemanticRepaired(task, withSequentialTiming(observed), pinnedReleasedInventory)
	if result.Passed || !result.Determinate || result.Status != semanticStatusFailed {
		t.Fatalf("semantic result = %+v", result)
	}
}

func assertSemanticFailedClosed(t *testing.T, task Task, observed []observedCall) {
	t.Helper()
	result := scoreSemanticRepaired(task, withSequentialTiming(observed), pinnedReleasedInventory)
	if result.Passed || result.Determinate || result.Status != semanticStatusFailedClosed || result.Reason == "" {
		t.Fatalf("semantic result = %+v", result)
	}
}

func observedWithResult(name, arguments, result string) observedCall {
	return observedCall{
		Name: name, Arguments: json.RawMessage(arguments), Result: json.RawMessage(result),
	}
}

func simulatedObservedCall(t *testing.T, name, arguments string) observedCall {
	t.Helper()
	result, err := simulateToolCall(name, json.RawMessage(arguments))
	if err != nil {
		t.Fatal(err)
	}
	return observedCall{Name: name, Arguments: json.RawMessage(arguments), Result: result}
}

func withSequentialTiming(observed []observedCall) []observedCall {
	result := append([]observedCall(nil), observed...)
	for index := range result {
		result[index].CallMoment = index * 2
		result[index].ResultMoment = index*2 + 1
		result[index].CallAtMS = float64(index * 2)
		result[index].ResultAtMS = float64(index*2 + 1)
	}
	return result
}

func resultReferences(value any) []string {
	var references []string
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case string:
			if strings.HasPrefix(typed, "$RESULT_") {
				references = append(references, typed)
			}
		case []any:
			for _, item := range typed {
				visit(item)
			}
		case map[string]any:
			for _, item := range typed {
				visit(item)
			}
		}
	}
	visit(value)
	return references
}

func releasedDatasetRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", ".runtime", "full-duplex-bench-v3", "dataset", "fdb_v3_data_released")
	if _, err := os.Stat(root); err != nil {
		testgate.Unprepared(t, "the prepared pinned FDB v3 dataset", err)
	}
	return root
}
