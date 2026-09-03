package fdbv3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const simulatorIdentity = "fdb-v3/callable-and-mock-apis-go-v3@" + pinnedReleasedRevision +
	"/agent-sha256:7928c110f2cb982b85ff3d77b6c81a4e826c381c20b35898404c0f5b53a2489c" +
	"/mock-sha256:475cc7e896f0c7d88fdf07f400de0f527c06f70ab452527eb82cf5a25c17292d"

// toolSimulator is the Go reproduction of mock_apis.py at the pinned FDB v3
// revision. It also retains each exact result so a later $RESULT_n.path
// annotation can be resolved against the bytes the agent actually received.
// Calls are serialized because providers may submit independent tool calls in
// parallel, while the official registry and $RESULT_n contract are ordered.
type toolSimulator struct {
	mu       sync.Mutex
	observed []observedCall
}

func (simulator *toolSimulator) Respond(name string, arguments json.RawMessage) (json.RawMessage, error) {
	simulator.mu.Lock()
	defer simulator.mu.Unlock()
	effective, result, err := invokeToolCall(name, arguments)
	retained := result
	if err != nil {
		retained = json.RawMessage(fmt.Sprintf("{%q:%q}", "error", err.Error()))
		effective = cloneRawMessage(arguments)
	}
	simulator.observed = append(simulator.observed, observedCall{
		Name: name, ProposedArguments: cloneRawMessage(arguments),
		Arguments: cloneRawMessage(effective), Result: cloneRawMessage(retained),
		CallMoment: -1, ResultMoment: -1,
	})
	return result, err
}

func (simulator *toolSimulator) Snapshot() []observedCall {
	simulator.mu.Lock()
	defer simulator.mu.Unlock()
	result := make([]observedCall, len(simulator.observed))
	for index, call := range simulator.observed {
		result[index] = observedCall{
			Name: call.Name, ProposedArguments: cloneRawMessage(call.ProposedArguments),
			Arguments: cloneRawMessage(call.Arguments), Result: cloneRawMessage(call.Result),
			CallMoment: call.CallMoment, ResultMoment: call.ResultMoment,
			CallAtMS: call.CallAtMS, ResultAtMS: call.ResultAtMS,
		}
	}
	return result
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

// visibleSimulatorResult reproduces the exact result bench.Session sends to
// the model and retains in a MomentToolResult. The session substitutes an
// empty object for syntactically invalid argument text before invoking the
// callback, and converts callback failures into a JSON error object.
func visibleSimulatorResult(
	name string, arguments json.RawMessage,
) (result json.RawMessage, succeeded bool) {
	invokedArguments := arguments
	if !json.Valid(invokedArguments) {
		invokedArguments = json.RawMessage(`{}`)
	}
	_, produced, err := invokeToolCall(name, invokedArguments)
	if err != nil {
		return json.RawMessage(fmt.Sprintf("{%q:%q}", "error", err.Error())), false
	}
	return produced, observedCallSucceeded(observedCall{Result: produced})
}

// observedCallsFromTranscript reconstructs the exact attempted-effect trace
// used for both live and recovered scoring. It refuses orphaned, duplicated,
// missing, reordered, or forged tool-result moments. Failed calls are returned
// in the population; their deterministic error result simply cannot satisfy a
// release or semantic pass predicate.
func observedCallsFromTranscript(transcript bench.Transcript) ([]observedCall, error) {
	type pendingCall struct {
		index int
		atMS  float64
	}
	observed := make([]observedCall, 0)
	pending := make(map[string]pendingCall)
	completed := make(map[string]struct{})
	for momentIndex, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentToolCall:
			if !finiteNonnegative(moment.AtMS) {
				return nil, fmt.Errorf("tool call moment %d has invalid timestamp", momentIndex)
			}
			if strings.TrimSpace(moment.CallID) == "" || moment.CallID != strings.TrimSpace(moment.CallID) {
				return nil, fmt.Errorf("tool call moment %d has a non-canonical call ID", momentIndex)
			}
			if strings.TrimSpace(moment.Name) == "" || moment.Name != strings.TrimSpace(moment.Name) {
				return nil, fmt.Errorf("tool call %q has a non-canonical function name", moment.CallID)
			}
			if _, exists := pending[moment.CallID]; exists {
				return nil, fmt.Errorf("tool call ID %q is duplicated before its result", moment.CallID)
			}
			if _, exists := completed[moment.CallID]; exists {
				return nil, fmt.Errorf("tool call ID %q is reused", moment.CallID)
			}
			proposed := json.RawMessage(moment.Arguments)
			effective, err := effectiveCallableArguments(moment.Name, proposed)
			if err != nil {
				effective = cloneRawMessage(proposed)
			}
			pending[moment.CallID] = pendingCall{index: len(observed), atMS: moment.AtMS}
			observed = append(observed, observedCall{
				Name: moment.Name, ProposedArguments: cloneRawMessage(proposed),
				Arguments:  cloneRawMessage(effective),
				CallMoment: momentIndex, ResultMoment: -1, CallAtMS: moment.AtMS,
			})

		case bench.MomentToolResult:
			if !finiteNonnegative(moment.AtMS) {
				return nil, fmt.Errorf("tool result moment %d has invalid timestamp", momentIndex)
			}
			call, exists := pending[moment.CallID]
			if !exists {
				return nil, fmt.Errorf("tool result %q has no unique pending call", moment.CallID)
			}
			if moment.AtMS <= call.atMS {
				return nil, fmt.Errorf("tool result %q has no timestamp after its call", moment.CallID)
			}
			if moment.Name != observed[call.index].Name {
				return nil, fmt.Errorf(
					"tool result %q names %q, want %q",
					moment.CallID, moment.Name, observed[call.index].Name,
				)
			}
			want, _ := visibleSimulatorResult(
				observed[call.index].Name, observed[call.index].ProposedArguments,
			)
			if !bytes.Equal([]byte(moment.Text), want) {
				return nil, fmt.Errorf(
					"tool result %q is inconsistent with %s",
					moment.CallID, simulatorIdentity,
				)
			}
			observed[call.index].Result = cloneRawMessage(want)
			observed[call.index].ResultMoment = momentIndex
			observed[call.index].ResultAtMS = moment.AtMS
			delete(pending, moment.CallID)
			completed[moment.CallID] = struct{}{}
		}
	}
	if len(pending) > 0 {
		ids := make([]string, 0, len(pending))
		for callID := range pending {
			ids = append(ids, callID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("tool calls have no retained results: %s", strings.Join(ids, ", "))
	}
	return observed, nil
}

// upstreamLoggedCalls projects the complete attempted-effect trace onto the
// calls that the pinned LiveKit callable could bind, invoke, and append to
// /tmp/agent_tool_calls.log. The authoritative release predicate deliberately
// does not use this projection: rejected attempts remain real attempted
// effects and must still make an exact-population release check fail.
func upstreamLoggedCalls(attempted []observedCall) []observedCall {
	logged := make([]observedCall, 0, len(attempted))
	for _, call := range attempted {
		proposed := call.ProposedArguments
		if len(proposed) == 0 {
			proposed = call.Arguments
		}
		// bench.Session passes an empty object to the callback when the model's
		// argument text is not JSON, while retaining the malformed proposal in
		// the tool-call moment.
		invoked := proposed
		if !json.Valid(invoked) {
			invoked = json.RawMessage(`{}`)
		}
		effective, result, err := invokeToolCall(call.Name, invoked)
		if err != nil || !bytes.Equal(result, call.Result) ||
			!observedCallSucceeded(observedCall{Result: result}) {
			continue
		}
		retained := call
		retained.ProposedArguments = cloneRawMessage(call.ProposedArguments)
		retained.Arguments = cloneRawMessage(effective)
		retained.Result = cloneRawMessage(call.Result)
		logged = append(logged, retained)
	}
	return logged
}

func finiteNonnegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// simulateToolCall implements the twelve deterministic functions in the
// pinned official mock_apis.py. Provider-visible results deliberately retain
// that file's exact structure. Released annotation paths that do not exist in
// those results are repaired only inside the separately identified semantic
// evaluator; exposing compatibility aliases here would change what the model
// observes and silently turn a fixture defect into an easier benchmark.
func simulateToolCall(name string, raw json.RawMessage) (json.RawMessage, error) {
	_, result, err := invokeToolCall(name, raw)
	return result, err
}

func invokeToolCall(
	name string, proposed json.RawMessage,
) (effective, result json.RawMessage, err error) {
	if !knownTool(name) {
		result, err := json.Marshal(map[string]any{
			"status": "error", "message": "Unknown function: " + name,
		})
		return cloneRawMessage(proposed), result, err
	}
	effective, err = effectiveCallableArguments(name, proposed)
	if err != nil {
		return nil, nil, err
	}
	result, err = simulateEffectiveToolCall(name, effective)
	return effective, result, err
}

func effectiveCallableArguments(name string, raw json.RawMessage) (json.RawMessage, error) {
	contract, found := pinnedCallableContracts[name]
	if !found {
		return nil, fmt.Errorf("unknown function: %s", name)
	}
	arguments, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("bind %s callable arguments: %w", name, err)
	}
	allowed := make(map[string]callableArgument, len(contract.arguments))
	for _, argument := range contract.arguments {
		allowed[argument.name] = argument
	}
	for argumentName := range arguments {
		if _, found := allowed[argumentName]; !found {
			// LiveKit constructs a default Pydantic model, whose extra policy is
			// ignore. The proposal remains in the transcript for audit, while the
			// effective callable and upstream logger never see this field.
			delete(arguments, argumentName)
		}
	}
	for _, argument := range contract.arguments {
		value, present := arguments[argument.name]
		if !present {
			if argument.required {
				return nil, fmt.Errorf("bind %s callable arguments: missing argument %s", name, argument.name)
			}
			switch name + "." + argument.name {
			case "calculate_commute.mode":
				arguments[argument.name] = "driving"
			case "search_products.max_price":
				arguments[argument.name] = nil
			case "add_to_cart.quantity":
				arguments[argument.name] = json.Number("1")
			default:
				return nil, fmt.Errorf("bind %s callable arguments: optional argument %s has no pinned default", name, argument.name)
			}
			continue
		}
		if value == nil && name == "search_products" && argument.name == "max_price" {
			continue
		}
		normalized, err := normalizeCallableValue(argument, value)
		if err != nil {
			return nil, fmt.Errorf("bind %s callable arguments: %w", name, err)
		}
		arguments[argument.name] = normalized
	}
	effective, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("bind %s callable arguments: %w", name, err)
	}
	return effective, nil
}

func normalizeCallableValue(argument callableArgument, value any) (any, error) {
	switch argument.schemaType {
	case "string":
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("argument %s must be a string, got %T", argument.name, value)
		}
		return value, nil
	case "number":
		var parsed float64
		var err error
		switch typed := value.(type) {
		case json.Number:
			parsed, err = typed.Float64()
		case string:
			parsed, err = strconv.ParseFloat(typed, 64)
		case bool:
			if typed {
				parsed = 1
			}
		default:
			return nil, fmt.Errorf("argument %s must be a number, got %T", argument.name, value)
		}
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return nil, fmt.Errorf("argument %s is not a finite number", argument.name)
		}
		return parsed, nil
	case "integer":
		switch typed := value.(type) {
		case string:
			value = json.Number(typed)
		case bool:
			if typed {
				return int64(1), nil
			}
			return int64(0), nil
		}
		integer, err := integerValue(value, argument.name)
		if err != nil {
			return nil, err
		}
		return integer, nil
	default:
		return nil, fmt.Errorf("argument %s has unsupported pinned schema type %q", argument.name, argument.schemaType)
	}
}

func simulateEffectiveToolCall(name string, raw json.RawMessage) (json.RawMessage, error) {
	arguments, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("simulate %s effective arguments: %w", name, err)
	}
	var result any
	switch name {
	case "search_flights":
		destination, err := requiredString(arguments, "destination")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		date, err := requiredString(arguments, "date")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success",
			"flights": []any{map[string]any{
				"flight_id": "FL123", "destination": destination, "date": date, "price": 450.0,
			}},
		}
	case "book_flight":
		passenger, err := requiredString(arguments, "passenger_name")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{"status": "success", "booking_ref": "B789", "passenger": passenger}
	case "update_identity_doc":
		documentType, err := requiredString(arguments, "doc_type")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		documentNumber, err := requiredString(arguments, "doc_number")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success", "updated_doc": documentType, "masked_number": lastRunes(documentNumber, 4),
		}
	case "get_card_benefits":
		cardType, err := requiredString(arguments, "card_type")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success", "card_type": cardType,
			"benefits": []any{"2% Cashback", "No Foreign Transaction Fee"},
		}
	case "get_exchange_rate":
		amount, err := requiredNumber(arguments, "amount")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		from, err := requiredString(arguments, "from_currency")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		_, err = requiredString(arguments, "to_currency")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		rate := 0.9
		if from == "EUR" {
			rate = 1.1
		}
		result = map[string]any{"status": "success", "converted_amount": amount * rate, "rate": rate}
	case "modify_autopay":
		billType, err := requiredString(arguments, "bill_type")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		source, err := requiredString(arguments, "source_account")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success", "autopay_enabled": true, "bill": billType, "source": source,
		}
	case "search_apartments":
		city, err := requiredString(arguments, "city")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		bedrooms, err := requiredInteger(arguments, "bedrooms")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		maxPrice, err := requiredNumber(arguments, "max_price")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		item := map[string]any{"id": "APT1", "price": maxPrice - 100, "beds": bedrooms}
		result = map[string]any{
			"status": "success", "city": city, "results": []any{item},
		}
	case "calculate_commute":
		if _, err := requiredString(arguments, "origin_address"); err != nil {
			return nil, toolArgumentError(name, err)
		}
		if _, err := requiredString(arguments, "destination_address"); err != nil {
			return nil, toolArgumentError(name, err)
		}
		mode, err := optionalString(arguments, "mode", "driving")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{"status": "success", "duration_mins": 25, "mode": mode}
	case "update_search_filter":
		filter, err := requiredString(arguments, "filter_name")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		value, found := arguments["value"]
		if !found {
			return nil, toolArgumentError(name, errors.New("missing argument value"))
		}
		result = map[string]any{
			"status": "success", "filter_updated": filter, "new_value": value,
		}
	case "track_order":
		orderID, err := requiredString(arguments, "order_id")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success", "order_id": orderID, "shipping_status": "Out for delivery",
		}
	case "search_products":
		query, err := requiredString(arguments, "query")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		maxPrice, found, err := presentNumber(arguments, "max_price")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		price := 99.99
		if found && maxPrice != 0 {
			price = maxPrice - 10
		}
		result = map[string]any{
			"status": "success",
			"products": []any{map[string]any{
				"product_id": "PROD1", "name": query + " Premium", "price": price,
			}},
		}
	case "add_to_cart":
		productID, err := requiredString(arguments, "product_id")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		quantity, err := requiredInteger(arguments, "quantity")
		if err != nil {
			return nil, toolArgumentError(name, err)
		}
		result = map[string]any{
			"status": "success", "product_id": productID, "quantity": quantity,
			"cart_total": 99.99 * float64(quantity),
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode simulated %s result: %w", name, err)
	}
	return encoded, nil
}

func knownTool(name string) bool {
	switch name {
	case "search_flights", "book_flight", "update_identity_doc", "get_card_benefits",
		"get_exchange_rate", "modify_autopay", "search_apartments", "calculate_commute",
		"update_search_filter", "track_order", "search_products", "add_to_cart":
		return true
	default:
		return false
	}
}

func decodeJSONObject(raw json.RawMessage) (map[string]any, error) {
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}

func decodeJSONValue(raw json.RawMessage) (any, error) {
	if err := strictjson.Validate(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func requiredString(arguments map[string]any, name string) (string, error) {
	value, found := arguments[name]
	if !found {
		return "", fmt.Errorf("missing argument %s", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("argument %s must be a string, got %T", name, value)
	}
	return text, nil
}

func optionalString(arguments map[string]any, name, fallback string) (string, error) {
	if _, found := arguments[name]; !found {
		return fallback, nil
	}
	return requiredString(arguments, name)
}

func requiredNumber(arguments map[string]any, name string) (float64, error) {
	value, found := arguments[name]
	if !found {
		return 0, fmt.Errorf("missing argument %s", name)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("argument %s must be a number, got %T", name, value)
	}
	result, err := number.Float64()
	if err != nil || math.IsInf(result, 0) || math.IsNaN(result) {
		return 0, fmt.Errorf("argument %s is not a finite number", name)
	}
	return result, nil
}

func presentNumber(arguments map[string]any, name string) (float64, bool, error) {
	value, found := arguments[name]
	if !found || value == nil {
		return 0, false, nil
	}
	number, err := requiredNumber(arguments, name)
	return number, true, err
}

func integerValue(value any, name string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("argument %s must be an integer, got %T", name, value)
	}
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, fmt.Errorf("argument %s must be an integer", name)
	}
	return rational.Num().Int64(), nil
}

func requiredInteger(arguments map[string]any, name string) (int64, error) {
	value, found := arguments[name]
	if !found {
		return 0, fmt.Errorf("missing argument %s", name)
	}
	return integerValue(value, name)
}

func toolArgumentError(tool string, err error) error {
	return fmt.Errorf("simulate %s: %w", tool, err)
}

func lastRunes(value string, count int) string {
	if count <= 0 || value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) <= count {
		return value
	}
	runes := []rune(value)
	return string(runes[len(runes)-count:])
}
