package fdbv3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	// openRealtimeHistoricalScorerIdentity preserves the project's original
	// field-aware normalization metric. It is comparison-only: in particular,
	// it does not establish that a simulator call succeeded.
	openRealtimeHistoricalScorerIdentity = "fdb-v3/openrealtime-identifier-normalized-fifo-v1"
	// upstreamFallbackScorerIdentity reproduces the deterministic fallback in
	// the exact pinned evaluate_tool_calls.py. The official tool-selection and
	// argument-accuracy metrics are comparison-only and remain separate from
	// the release-validity predicate below.
	upstreamFallbackScorerIdentity = "fdb-v3/upstream-evaluate-tool-calls-fallback-v1@" +
		pinnedReleasedRevision + "/sha256:5320e223b9cda55ec19255af7464d905788fdd3b09c206ff8aa29ede87753877"
	// releaseValidityScorerIdentity owns TaskOutcome.Passed. It deliberately
	// counts every attempted call, uses the pinned fallback's argument matches,
	// and additionally requires an exact call population and successful result
	// for every observed effect.
	releaseValidityScorerIdentity = "fdb-v3/release-validity-exact-ordered-successful-attempted-effects-v3"
	// semanticRepairedScorerIdentity names a separate diagnostic. It must not
	// be compared with published upstream pass rates: it resolves actual
	// successful results causally and materializes objective conditional notes.
	semanticRepairedScorerIdentity  = "fdb-v3/semantic-repaired-result-causal-fixture-aware-v3"
	semanticReferenceErrataIdentity = "fdb-v3/reference-errata-singleton-only-v2@" +
		pinnedReleasedRevision + "/sha256:" + pinnedReleasedArtifactDigest

	semanticStatusPassed               = "passed"
	semanticStatusFailed               = "failed"
	semanticStatusFailedClosed         = "failed_closed"
	semanticStatusIndeterminateFixture = "indeterminate_fixture"
	semanticStatusContractInvalid      = "contract_invalid_fixture"
)

type callScore struct {
	Expected  int
	Observed  int
	Names     int
	Arguments int
	Passed    bool
}

type comparisonScore struct {
	Expected               int
	Observed               int
	Names                  int
	Arguments              int
	ToolSelectionRecall    float64
	ToolSelectionPrecision float64
	ToolSelectionF1        float64
	ArgumentAccuracy       float64
	ExactPopulationMatch   bool
	ExpectedOrder          bool
}

// scoreOpenRealtimeHistorical preserves the pre-audit OpenRealtime score.
// Its identifier-aware matcher remains useful for longitudinal comparison,
// but it is not the pinned evaluator fallback and does not own Passed.
func scoreOpenRealtimeHistorical(expected []ExpectedCall, observed []observedCall) comparisonScore {
	return scoreComparison(expected, observed, argumentsMatchHistorical)
}

// scorePinnedUpstreamFallback reproduces the deterministic portions of the
// pinned evaluate_tool_calls.py: multiset tool-selection F1 and per-function
// FIFO argument accuracy. A top-level expected string beginning with "$" is
// skipped by the argument matcher, exactly as in upstream.
func scorePinnedUpstreamFallback(expected []ExpectedCall, observed []observedCall) comparisonScore {
	return scoreComparison(expected, observed, argumentsMatchUpstreamFallback)
}

func scoreComparison(
	expected []ExpectedCall, observed []observedCall,
	match func(json.RawMessage, json.RawMessage) bool,
) comparisonScore {
	result := comparisonScore{Expected: len(expected), Observed: len(observed)}
	pairs := pairExpectedCalls(expected, observed)
	for expectedIndex, observedIndex := range pairs {
		if observedIndex < 0 {
			continue
		}
		result.Names++
		if match(expected[expectedIndex].Args, observed[observedIndex].Arguments) {
			result.Arguments++
		}
	}
	result.ToolSelectionRecall = 1
	if result.Expected > 0 {
		result.ToolSelectionRecall = float64(result.Names) / float64(result.Expected)
	}
	result.ToolSelectionPrecision = 1
	if result.Observed > 0 {
		result.ToolSelectionPrecision = float64(result.Names) / float64(result.Observed)
	}
	if result.ToolSelectionRecall+result.ToolSelectionPrecision > 0 {
		result.ToolSelectionF1 = 2 * result.ToolSelectionRecall * result.ToolSelectionPrecision /
			(result.ToolSelectionRecall + result.ToolSelectionPrecision)
	}
	result.ArgumentAccuracy = 1
	if result.Expected > 0 {
		result.ArgumentAccuracy = float64(result.Arguments) / float64(result.Expected)
	}
	result.ToolSelectionRecall = roundUpstreamMetric(result.ToolSelectionRecall)
	result.ToolSelectionPrecision = roundUpstreamMetric(result.ToolSelectionPrecision)
	result.ToolSelectionF1 = roundUpstreamMetric(result.ToolSelectionF1)
	result.ArgumentAccuracy = roundUpstreamMetric(result.ArgumentAccuracy)
	result.ExactPopulationMatch = result.Observed == result.Expected &&
		result.Names == result.Expected && result.Arguments == result.Expected
	result.ExpectedOrder = expectedCallOrderMatches(pairs)
	return result
}

func expectedCallOrderMatches(pairs []int) bool {
	prior := -1
	for _, observedIndex := range pairs {
		if observedIndex < 0 || observedIndex <= prior {
			return false
		}
		prior = observedIndex
	}
	return true
}

func roundUpstreamMetric(value float64) float64 {
	return math.RoundToEven(value*1000) / 1000
}

type releaseValidityScore struct {
	Expected   int
	Observed   int
	Names      int
	Arguments  int
	Successful int
	Ordered    bool
	Passed     bool
}

func scoreReleaseValidity(
	compatibility comparisonScore, observed []observedCall,
) releaseValidityScore {
	result := releaseValidityScore{
		Expected: compatibility.Expected, Observed: compatibility.Observed,
		Names: compatibility.Names, Arguments: compatibility.Arguments,
		Ordered: compatibility.ExpectedOrder,
	}
	for _, call := range observed {
		if observedCallSucceeded(call) {
			result.Successful++
		}
	}
	result.Passed = result.Observed == result.Expected &&
		result.Names == result.Expected && result.Arguments == result.Expected &&
		result.Successful == result.Observed && result.Ordered
	return result
}

func observedCallSucceeded(call observedCall) bool {
	_, err := decodeSuccessfulResult(call.Result)
	return err == nil
}

// pairExpectedCalls mirrors the pinned evaluator's per-function FIFO queues.
// The returned observed indexes also bridge an expected $RESULT_n slot to the
// actual result produced by the call paired with that slot.
func pairExpectedCalls(expected []ExpectedCall, observed []observedCall) []int {
	used := make([]bool, len(observed))
	pairs := make([]int, len(expected))
	for index := range pairs {
		pairs[index] = -1
	}
	for expectedIndex, want := range expected {
		for observedIndex, got := range observed {
			if used[observedIndex] || got.Name != want.Function {
				continue
			}
			used[observedIndex] = true
			pairs[expectedIndex] = observedIndex
			break
		}
	}
	return pairs
}

func argumentsMatchUpstreamFallback(expected, observed json.RawMessage) bool {
	want, err := decodeUpstreamJSONObject(expected)
	if err != nil {
		return false
	}
	got, err := decodeUpstreamJSONObject(observed)
	if err != nil {
		return false
	}
	for name, value := range want {
		other, present := got[name]
		if !present {
			return false
		}
		if reference, dynamic := value.(string); dynamic && strings.HasPrefix(reference, "$") {
			continue
		}
		if !argumentValueMatchesUpstreamFallback(value, other) {
			return false
		}
	}
	return true
}

func decodeUpstreamJSONObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("arguments contain more than one JSON value")
		}
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}

// argumentValueMatchesUpstreamFallback mirrors exact_match_args.normalize:
// normalization applies only to a top-level string value. Lists, objects, and
// their nested strings use Python JSON-value equality without normalization.
func argumentValueMatchesUpstreamFallback(expected, observed any) bool {
	if want, ok := expected.(string); ok {
		got, ok := observed.(string)
		return ok && normalizeUpstreamFallbackString(want) == normalizeUpstreamFallbackString(got)
	}
	return pythonJSONEqual(expected, observed)
}

func normalizeUpstreamFallbackString(value string) string {
	return strings.ReplaceAll(cases.Lower(language.Und).String(strings.TrimSpace(value)), "_", " ")
}

// pythonJSONEqual covers the equality semantics relevant to decoded JSON.
// Python considers integral/float numeric representations equal by value and
// also treats bool as numeric for equality; nested strings remain exact.
func pythonJSONEqual(expected, observed any) bool {
	switch want := expected.(type) {
	case nil:
		return observed == nil
	case bool:
		switch got := observed.(type) {
		case bool:
			return got == want
		case json.Number:
			return pythonBoolNumberEqual(want, got)
		default:
			return false
		}
	case json.Number:
		switch got := observed.(type) {
		case json.Number:
			return pythonNumbersEqual(want, got)
		case bool:
			return pythonBoolNumberEqual(got, want)
		default:
			return false
		}
	case string:
		got, ok := observed.(string)
		return ok && got == want
	case []any:
		got, ok := observed.([]any)
		if !ok || len(got) != len(want) {
			return false
		}
		for index := range want {
			if !pythonJSONEqual(want[index], got[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		got, ok := observed.(map[string]any)
		if !ok || len(got) != len(want) {
			return false
		}
		for name, value := range want {
			other, found := got[name]
			if !found || !pythonJSONEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func pythonBoolNumberEqual(boolean bool, number json.Number) bool {
	want := "0"
	if boolean {
		want = "1"
	}
	return pythonNumbersEqual(json.Number(want), number)
}

type decodedPythonNumber struct {
	integer  *big.Int
	floating float64
	isFloat  bool
}

func decodePythonNumber(number json.Number) (decodedPythonNumber, bool) {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		integer, ok := new(big.Int).SetString(text, 10)
		return decodedPythonNumber{integer: integer}, ok
	}
	floating, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return decodedPythonNumber{}, false
	}
	return decodedPythonNumber{floating: floating, isFloat: true}, true
}

func pythonNumbersEqual(left, right json.Number) bool {
	decodedLeft, leftOK := decodePythonNumber(left)
	decodedRight, rightOK := decodePythonNumber(right)
	if !leftOK || !rightOK {
		return false
	}
	switch {
	case !decodedLeft.isFloat && !decodedRight.isFloat:
		return decodedLeft.integer.Cmp(decodedRight.integer) == 0
	case decodedLeft.isFloat && decodedRight.isFloat:
		return decodedLeft.floating == decodedRight.floating
	case !decodedLeft.isFloat:
		return pythonIntegerFloatEqual(decodedLeft.integer, decodedRight.floating)
	default:
		return pythonIntegerFloatEqual(decodedRight.integer, decodedLeft.floating)
	}
}

func pythonIntegerFloatEqual(integer *big.Int, floating float64) bool {
	if integer == nil || math.IsInf(floating, 0) || math.IsNaN(floating) {
		return false
	}
	floatRational := new(big.Rat).SetFloat64(floating)
	if floatRational == nil {
		return false
	}
	return new(big.Rat).SetInt(integer).Cmp(floatRational) == 0
}

type referenceErratumKey struct {
	Revision       string
	ArtifactDigest string
	Task           string
	Reference      string
}

func pinnedReferenceErratumKey(task, reference string) referenceErratumKey {
	return referenceErratumKey{
		Revision: pinnedReleasedRevision, ArtifactDigest: pinnedReleasedArtifactDigest,
		Task: task, Reference: reference,
	}
}

type referenceErratum struct {
	Replacement          string
	SingletonResultArray string
}

// pinnedReferenceErrata is evaluator-only. The one retained correction is
// defensible because the deterministic result contains exactly one product;
// validation below still fails closed if that cardinality changes. The six
// housing annotations ask for addresses that do not exist in the retained
// results and are intentionally classified as contract-invalid fixtures
// instead of being rewritten from an address to the unrelated APT1 ID.
var pinnedReferenceErrata = map[referenceErratumKey]referenceErratum{
	pinnedReferenceErratumKey(
		"ecommerce_18_62a885d5b6af18b3d4579e1b", "$RESULT_0.cheapest_product_id",
	): {Replacement: "$RESULT_0.products[0].product_id", SingletonResultArray: ".products"},
}

type semanticScore struct {
	callScore
	Status      string
	Reason      string
	Determinate bool
}

type semanticPlan struct {
	Expected                []ExpectedCall
	ConditionSourceObserved int
	ConditionalExpected     int
}

// scoreSemanticRepaired is a diagnostic with a different identity from the
// primary upstream-static metric. It is available only for the exact pinned
// release whose errata and conditional semantics were audited.
func scoreSemanticRepaired(
	task Task, observed []observedCall, inventory releasedDatasetInventory,
) semanticScore {
	if err := validateSemanticIdentity(task.ID, inventory); err != nil {
		return semanticScore{
			callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
			Status:    semanticStatusFailedClosed, Reason: err.Error(),
		}
	}
	disposition, err := pinnedFixtureDisposition(task.ID, inventory)
	if err != nil {
		return semanticScore{
			callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
			Status:    semanticStatusFailedClosed, Reason: err.Error(),
		}
	}
	if disposition.Status != fixtureStatusDeterminate {
		if err := validateFixtureDispositionTask(task, disposition); err != nil {
			return semanticScore{
				callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
				Status:    semanticStatusFailedClosed,
				Reason:    "validate exact fixture disposition: " + err.Error(),
			}
		}
		return semanticScore{
			callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
			Status:    string(disposition.Status), Reason: disposition.reason(),
		}
	}
	if err := validateObservedCallTiming(observed); err != nil {
		return semanticScore{
			callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
			Status:    semanticStatusFailedClosed, Reason: err.Error(),
		}
	}
	plan, status, reason := materializeSemanticPlan(task, observed)
	if status != "" {
		return semanticScore{
			callScore: callScore{Expected: len(task.Expected), Observed: len(observed)},
			Status:    status, Reason: reason,
		}
	}

	result := semanticScore{
		callScore:   callScore{Expected: len(plan.Expected), Observed: len(observed)},
		Determinate: true,
	}
	pairs := pairExpectedCalls(plan.Expected, observed)
	for expectedIndex, observedIndex := range pairs {
		if observedIndex < 0 {
			continue
		}
		result.Names++
		prior := pairedPriorResults(expectedIndex, observedIndex, pairs, observed)
		matched, err := argumentsMatchSemantic(
			task.ID, plan.Expected[expectedIndex].Args, observed[observedIndex].Arguments,
			prior, inventory,
		)
		if err != nil {
			result.Passed = false
			result.Determinate = false
			result.Status = semanticStatusFailedClosed
			result.Reason = fmt.Sprintf(
				"resolve semantic arguments for expected call %d (%s): %v",
				expectedIndex, plan.Expected[expectedIndex].Function, err,
			)
			return result
		}
		if matched {
			result.Arguments++
		}
	}
	causalBranch := true
	if plan.ConditionalExpected >= 0 {
		branchObserved := pairs[plan.ConditionalExpected]
		causalBranch = branchObserved >= 0 && plan.ConditionSourceObserved >= 0 &&
			resultPrecedesCall(observed[plan.ConditionSourceObserved], observed[branchObserved])
		if branchObserved >= 0 && !causalBranch {
			result.Reason = "conditional action call did not occur after its successful condition-source result"
		}
	}
	successful := 0
	for _, call := range observed {
		if observedCallSucceeded(call) {
			successful++
		}
	}
	result.Passed = result.Observed == result.Expected &&
		result.Arguments == result.Expected && successful == result.Observed && causalBranch
	if result.Passed {
		result.Status = semanticStatusPassed
	} else {
		result.Status = semanticStatusFailed
	}
	return result
}

func validateObservedCallTiming(observed []observedCall) error {
	priorCallMoment := -1
	for index, call := range observed {
		if call.CallMoment < 0 || call.ResultMoment <= call.CallMoment ||
			!finiteNonnegative(call.CallAtMS) || !finiteNonnegative(call.ResultAtMS) ||
			call.ResultAtMS <= call.CallAtMS {
			return fmt.Errorf("observed call %d (%s) has missing or invalid call/result timing evidence", index, call.Name)
		}
		if call.CallMoment <= priorCallMoment {
			return fmt.Errorf("observed call %d (%s) is not ordered by its retained call moment", index, call.Name)
		}
		priorCallMoment = call.CallMoment
	}
	return nil
}

func resultPrecedesCall(producer, dependent observedCall) bool {
	return producer.ResultMoment < dependent.CallMoment && producer.ResultAtMS <= dependent.CallAtMS
}

func validateSemanticIdentity(taskID string, inventory releasedDatasetInventory) error {
	if inventory.Revision != pinnedReleasedRevision ||
		inventory.ArtifactDigest != pinnedReleasedArtifactDigest {
		return fmt.Errorf(
			"semantic repair is unavailable for revision %q artifact %q",
			inventory.Revision, inventory.ArtifactDigest,
		)
	}
	index := sort.SearchStrings(pinnedReleasedInventory.TaskNames, taskID)
	if index >= len(pinnedReleasedInventory.TaskNames) || pinnedReleasedInventory.TaskNames[index] != taskID {
		return fmt.Errorf("semantic repair is unavailable for unpinned task %q", taskID)
	}
	return nil
}

func materializeSemanticPlan(task Task, observed []observedCall) (semanticPlan, string, string) {
	plan := semanticPlan{
		Expected:                append([]ExpectedCall(nil), task.Expected...),
		ConditionSourceObserved: -1, ConditionalExpected: -1,
	}
	hasConditionalNote := false
	for _, call := range task.Expected {
		hasConditionalNote = hasConditionalNote || strings.TrimSpace(call.Note) != ""
	}

	switch task.ID {
	case "ecommerce_20_66c4f3cb14cbfc4db836bd4e":
		if err := validateConditionalFixture(task, []string{"search_products", "add_to_cart"}, map[int]string{
			1: "Only if price < 50, otherwise track order WWW",
		}); err != nil {
			return plan, semanticStatusFailedClosed, err.Error()
		}
		producer, observedIndex, err := pairedSuccessfulResult(task.Expected, observed, 0)
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate product-price condition: " + err.Error()
		}
		price, err := resultNumber(producer, ".products[0].price")
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate product-price condition: " + err.Error()
		}
		plan.ConditionSourceObserved = observedIndex
		plan.ConditionalExpected = 1
		if price >= 50 {
			plan.Expected[1] = ExpectedCall{
				Function: "track_order", Args: json.RawMessage(`{"order_id":"WWW"}`),
			}
		}
		return plan, "", ""

	case "housing_20_62a885d5b6af18b3d4579e1b":
		if err := validateConditionalFixture(task,
			[]string{"search_apartments", "calculate_commute", "update_search_filter"},
			map[int]string{2: "Only if commute > 15 min"},
		); err != nil {
			return plan, semanticStatusFailedClosed, err.Error()
		}
		producer, observedIndex, err := pairedSuccessfulResult(task.Expected, observed, 1)
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate commute-duration condition: " + err.Error()
		}
		duration, err := resultNumber(producer, ".duration_mins")
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate commute-duration condition: " + err.Error()
		}
		if duration > 15 {
			plan.ConditionSourceObserved = observedIndex
			plan.ConditionalExpected = 2
		} else {
			plan.Expected = plan.Expected[:2]
		}
		return plan, "", ""

	case "travel_20_62a885d5b6af18b3d4579e1b":
		if err := validateConditionalFixture(task,
			[]string{"search_flights", "book_flight", "update_identity_doc"},
			map[int]string{1: "Only if flight < $300", 2: "Only if no cheap flight"},
		); err != nil {
			return plan, semanticStatusFailedClosed, err.Error()
		}
		producer, observedIndex, err := pairedSuccessfulResult(task.Expected, observed, 0)
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate flight-price condition: " + err.Error()
		}
		price, err := resultNumber(producer, ".flights[0].price")
		if err != nil {
			return plan, semanticStatusFailedClosed, "evaluate flight-price condition: " + err.Error()
		}
		plan.ConditionSourceObserved = observedIndex
		plan.ConditionalExpected = 1
		if price < 300 {
			plan.Expected = plan.Expected[:2]
		} else {
			plan.Expected = []ExpectedCall{plan.Expected[0], plan.Expected[2]}
		}
		return plan, "", ""
	default:
		if hasConditionalNote {
			return plan, semanticStatusFailedClosed,
				fmt.Sprintf("unpinned conditional annotation on task %q", task.ID)
		}
		return plan, "", ""
	}
}

func validateConditionalFixture(task Task, functions []string, notes map[int]string) error {
	if len(task.Expected) != len(functions) {
		return fmt.Errorf("conditional fixture %q has %d calls, want %d", task.ID, len(task.Expected), len(functions))
	}
	for index, function := range functions {
		if task.Expected[index].Function != function {
			return fmt.Errorf(
				"conditional fixture %q call %d is %q, want %q",
				task.ID, index, task.Expected[index].Function, function,
			)
		}
		wantNote := notes[index]
		if task.Expected[index].Note != wantNote {
			return fmt.Errorf(
				"conditional fixture %q call %d note is %q, want %q",
				task.ID, index, task.Expected[index].Note, wantNote,
			)
		}
	}
	return nil
}

func pairedSuccessfulResult(
	expected []ExpectedCall, observed []observedCall, expectedIndex int,
) (map[string]any, int, error) {
	if expectedIndex < 0 || expectedIndex >= len(expected) {
		return nil, -1, fmt.Errorf("condition source slot %d is out of range", expectedIndex)
	}
	pairs := pairExpectedCalls(expected, observed)
	observedIndex := pairs[expectedIndex]
	if observedIndex < 0 {
		return nil, -1, fmt.Errorf("condition source %s was not observed", expected[expectedIndex].Function)
	}
	result, err := decodeSuccessfulResult(observed[observedIndex].Result)
	if err != nil {
		return nil, -1, fmt.Errorf("condition source %s: %w", expected[expectedIndex].Function, err)
	}
	return result, observedIndex, nil
}

func decodeSuccessfulResult(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("has no retained result")
	}
	result, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("decode retained result: %w", err)
	}
	status, ok := result["status"].(string)
	if !ok || status != "success" {
		return nil, fmt.Errorf("retained result status is %v, want success", result["status"])
	}
	return result, nil
}

func resultNumber(result map[string]any, path string) (float64, error) {
	value, err := selectResultPath(result, path, path)
	if err != nil {
		return 0, err
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("result path %q is %T, want number", path, value)
	}
	parsed, err := number.Float64()
	if err != nil {
		return 0, fmt.Errorf("result path %q is not a finite number", path)
	}
	return parsed, nil
}

// pairedPriorResults keeps the reference namespace in expected-call order,
// while admitting a result only when its paired call completed before the
// dependent observation. A dependent call cannot borrow a future result just
// because function names were reordered.
func pairedPriorResults(
	expectedIndex, observedIndex int, pairs []int, observed []observedCall,
) []observedCall {
	prior := make([]observedCall, expectedIndex)
	for sourceExpected := 0; sourceExpected < expectedIndex; sourceExpected++ {
		sourceObserved := pairs[sourceExpected]
		if sourceObserved >= 0 && sourceObserved < observedIndex &&
			resultPrecedesCall(observed[sourceObserved], observed[observedIndex]) {
			prior[sourceExpected] = observed[sourceObserved]
		}
	}
	return prior
}

// argumentsMatchHistorical is the project-specific field-aware matcher used
// by the historical OpenRealtime metric. It is deliberately separate from
// argumentsMatchUpstreamFallback.
func argumentsMatchHistorical(expected, observed json.RawMessage) bool {
	want, err := decodeJSONObject(expected)
	if err != nil {
		return false
	}
	got, err := decodeJSONObject(observed)
	if err != nil {
		return false
	}
	for name, value := range want {
		other, present := got[name]
		if !present {
			return false
		}
		if reference, dynamic := value.(string); dynamic && strings.HasPrefix(reference, "$RESULT_") {
			continue
		}
		if !argumentValueMatches(name, value, other) {
			return false
		}
	}
	return true
}

func argumentsMatchSemantic(
	taskID string, expected, observed json.RawMessage, prior []observedCall,
	inventory releasedDatasetInventory,
) (bool, error) {
	want, err := decodeJSONObject(expected)
	if err != nil {
		return false, fmt.Errorf("decode expected arguments: %w", err)
	}
	got, err := decodeJSONObject(observed)
	if err != nil {
		return false, nil
	}
	for name, value := range want {
		other, present := got[name]
		if !present {
			return false, nil
		}
		resolved, err := resolveSemanticExpectedValue(taskID, value, prior, inventory)
		if err != nil {
			return false, err
		}
		if !argumentValueMatches(name, resolved, other) {
			return false, nil
		}
	}
	return true, nil
}

func resolveSemanticExpectedValue(
	taskID string, value any, prior []observedCall, inventory releasedDatasetInventory,
) (any, error) {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "$RESULT_") {
			return resolveSemanticResultReference(taskID, typed, prior, inventory)
		}
		return typed, nil
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			resolved, err := resolveSemanticExpectedValue(taskID, item, prior, inventory)
			if err != nil {
				return nil, err
			}
			result[index] = resolved
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for name, item := range typed {
			resolved, err := resolveSemanticExpectedValue(taskID, item, prior, inventory)
			if err != nil {
				return nil, err
			}
			result[name] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

func resolveSemanticResultReference(
	taskID, reference string, prior []observedCall, inventory releasedDatasetInventory,
) (any, error) {
	if err := validateSemanticIdentity(taskID, inventory); err != nil {
		return nil, err
	}
	key := referenceErratumKey{
		Revision: inventory.Revision, ArtifactDigest: inventory.ArtifactDigest,
		Task: taskID, Reference: reference,
	}
	if erratum, found := pinnedReferenceErrata[key]; found {
		callIndex, err := resultReferenceIndex(reference)
		if err != nil {
			return nil, err
		}
		if callIndex >= len(prior) {
			return nil, fmt.Errorf("FDB v3 result reference %q is not an earlier observed call", reference)
		}
		producer, err := decodeSuccessfulResult(prior[callIndex].Result)
		if err != nil {
			return nil, fmt.Errorf("FDB v3 result reference %q producer: %w", reference, err)
		}
		items, err := selectResultPath(producer, erratum.SingletonResultArray, reference)
		if err != nil {
			return nil, err
		}
		array, ok := items.([]any)
		if !ok || len(array) != 1 {
			return nil, fmt.Errorf(
				"FDB v3 result reference %q repair requires exactly one result item, got %T with length %d",
				reference, items, resultArrayLength(items),
			)
		}
		reference = erratum.Replacement
	}
	callIndex, err := resultReferenceIndex(reference)
	if err != nil {
		return nil, err
	}
	if callIndex >= len(prior) {
		return nil, fmt.Errorf("FDB v3 result reference %q is not an earlier observed call", reference)
	}
	if _, err := decodeSuccessfulResult(prior[callIndex].Result); err != nil {
		return nil, fmt.Errorf("FDB v3 result reference %q producer: %w", reference, err)
	}
	return resolveResultReference(reference, prior)
}

func resultArrayLength(value any) int {
	if array, ok := value.([]any); ok {
		return len(array)
	}
	return -1
}

func resultReferenceIndex(reference string) (int, error) {
	const prefix = "$RESULT_"
	if !strings.HasPrefix(reference, prefix) {
		return 0, fmt.Errorf("invalid FDB v3 result reference %q", reference)
	}
	remainder := strings.TrimPrefix(reference, prefix)
	delimiter := strings.IndexAny(remainder, ".[")
	if delimiter <= 0 {
		return 0, fmt.Errorf("invalid FDB v3 result reference %q", reference)
	}
	callIndex, err := strconv.Atoi(remainder[:delimiter])
	if err != nil || callIndex < 0 {
		return 0, fmt.Errorf("invalid FDB v3 result index in %q", reference)
	}
	return callIndex, nil
}

// resolveResultReference resolves only paths that are literally present. It
// contains no aliases or errata; those are applied by the pinned semantic
// wrapper above.
func resolveResultReference(reference string, prior []observedCall) (any, error) {
	callIndex, err := resultReferenceIndex(reference)
	if err != nil {
		return nil, err
	}
	remainder := strings.TrimPrefix(reference, "$RESULT_")
	delimiter := strings.IndexAny(remainder, ".[")
	if callIndex >= len(prior) {
		return nil, fmt.Errorf("FDB v3 result reference %q is not an earlier observed call", reference)
	}
	if len(prior[callIndex].Result) == 0 {
		return nil, fmt.Errorf("FDB v3 result reference %q names an absent result", reference)
	}
	current, err := decodeJSONValue(prior[callIndex].Result)
	if err != nil {
		return nil, fmt.Errorf("decode FDB v3 result %d: %w", callIndex, err)
	}
	return selectResultPath(current, remainder[delimiter:], reference)
}

func selectResultPath(current any, path, reference string) (any, error) {
	for path != "" {
		switch path[0] {
		case '.':
			path = path[1:]
			end := strings.IndexAny(path, ".[")
			if end < 0 {
				end = len(path)
			}
			name := path[:end]
			if name == "" {
				return nil, fmt.Errorf("invalid empty result field in %q", reference)
			}
			object, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("result path %q selects field %q from %T", reference, name, current)
			}
			var found bool
			current, found = object[name]
			if !found {
				return nil, fmt.Errorf("result path %q has no field %q", reference, name)
			}
			path = path[end:]
		case '[':
			closeIndex := strings.IndexByte(path, ']')
			if closeIndex <= 1 {
				return nil, fmt.Errorf("invalid result array index in %q", reference)
			}
			index, err := strconv.Atoi(path[1:closeIndex])
			if err != nil || index < 0 {
				return nil, fmt.Errorf("invalid result array index in %q", reference)
			}
			array, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("result path %q selects index %d from %T", reference, index, current)
			}
			if index >= len(array) {
				return nil, fmt.Errorf("result path %q index %d exceeds length %d", reference, index, len(array))
			}
			current = array[index]
			path = path[closeIndex+1:]
		default:
			return nil, fmt.Errorf("invalid result path syntax in %q", reference)
		}
	}
	return current, nil
}

func argumentValueMatches(field string, expected, observed any) bool {
	switch want := expected.(type) {
	case nil:
		return observed == nil
	case bool:
		got, ok := observed.(bool)
		return ok && got == want
	case json.Number:
		got, ok := observed.(json.Number)
		return ok && numbersEqual(want, got)
	case string:
		got, ok := observed.(string)
		if !ok {
			return false
		}
		return normalizeStringField(field, want) == normalizeStringField(field, got)
	case []any:
		got, ok := observed.([]any)
		if !ok || len(want) != len(got) {
			return false
		}
		for index := range want {
			if !argumentValueMatches(field, want[index], got[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		got, ok := observed.(map[string]any)
		if !ok || len(want) != len(got) {
			return false
		}
		for name, value := range want {
			other, found := got[name]
			if !found || !argumentValueMatches(name, value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func numbersEqual(left, right json.Number) bool {
	leftRational, leftOK := new(big.Rat).SetString(left.String())
	rightRational, rightOK := new(big.Rat).SetString(right.String())
	return leftOK && rightOK && leftRational.Cmp(rightRational) == 0
}

var identifierArguments = map[string]struct{}{
	"booking_ref": {}, "doc_number": {}, "flight_id": {}, "order_id": {}, "product_id": {},
}

func normalizeStringField(field, value string) string {
	if _, identifier := identifierArguments[field]; identifier {
		return normalizeIdentifier(value)
	}
	return normalizeText(value)
}

// normalizeIdentifier preserves whitespace boundaries while ignoring the
// punctuation ASR commonly inserts into a dictated identifier. It is used only
// on explicitly enumerated identifier fields, never globally.
func normalizeIdentifier(value string) string {
	var builder strings.Builder
	space := false
	for _, symbol := range strings.ToLower(value) {
		switch {
		case symbol == '-' || symbol == '.' || symbol == ',' || symbol == '_' ||
			symbol == '\'' || symbol == '"':
			continue
		case unicode.IsSpace(symbol):
			space = builder.Len() > 0
			continue
		}
		if space {
			builder.WriteRune(' ')
			space = false
		}
		builder.WriteRune(symbol)
	}
	return builder.String()
}

// normalizeText is deliberately conservative. It admits case and repeated
// whitespace differences without deleting punctuation or word boundaries from
// ordinary names, addresses, dates, currencies, modes, and search text.
func normalizeText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
