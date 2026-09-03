package fdbv3

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

type decodedCatalogTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type        json.RawMessage `json:"type"`
			Description string          `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"parameters"`
}

func TestCatalogExactlyMatchesPinnedCallableSignatures(t *testing.T) {
	type signature struct {
		properties map[string]string
		required   []string
	}
	want := map[string]signature{
		"search_flights": {
			properties: map[string]string{"destination": "string", "date": "string"},
			required:   []string{"destination", "date"},
		},
		"book_flight": {
			properties: map[string]string{"passenger_name": "string"}, required: []string{"passenger_name"},
		},
		"update_identity_doc": {
			properties: map[string]string{"doc_type": "string", "doc_number": "string"},
			required:   []string{"doc_type", "doc_number"},
		},
		"get_card_benefits": {
			properties: map[string]string{"card_type": "string"}, required: []string{"card_type"},
		},
		"get_exchange_rate": {
			properties: map[string]string{"amount": "number", "from_currency": "string", "to_currency": "string"},
			required:   []string{"amount", "from_currency", "to_currency"},
		},
		"modify_autopay": {
			properties: map[string]string{"bill_type": "string", "source_account": "string"},
			required:   []string{"bill_type", "source_account"},
		},
		"search_apartments": {
			properties: map[string]string{"city": "string", "bedrooms": "integer", "max_price": "number"},
			required:   []string{"city", "bedrooms", "max_price"},
		},
		"calculate_commute": {
			properties: map[string]string{"origin_address": "string", "destination_address": "string", "mode": "string"},
			required:   []string{"origin_address", "destination_address"},
		},
		"update_search_filter": {
			properties: map[string]string{"filter_name": "string", "value": "string"},
			required:   []string{"filter_name", "value"},
		},
		"track_order": {
			properties: map[string]string{"order_id": "string"}, required: []string{"order_id"},
		},
		"search_products": {
			properties: map[string]string{"query": "string", "max_price": "number"}, required: []string{"query"},
		},
		"add_to_cart": {
			properties: map[string]string{"product_id": "string", "quantity": "integer"},
			required:   []string{"product_id"},
		},
	}

	catalog := decodeCatalog(t, mustCatalog(t, nil))
	if len(catalog) != len(want) {
		t.Fatalf("catalog population = %d, want %d", len(catalog), len(want))
	}
	for name, expected := range want {
		tool, found := catalog[name]
		if !found {
			t.Errorf("catalog omitted pinned callable %q", name)
			continue
		}
		got := make(map[string]string, len(tool.Parameters.Properties))
		for argument, property := range tool.Parameters.Properties {
			var schemaType string
			if err := json.Unmarshal(property.Type, &schemaType); err != nil {
				t.Fatalf("%s.%s type = %s: %v", name, argument, property.Type, err)
			}
			got[argument] = schemaType
		}
		if !reflect.DeepEqual(got, expected.properties) {
			t.Errorf("%s properties = %v, want %v", name, got, expected.properties)
		}
		if !reflect.DeepEqual(tool.Parameters.Required, expected.required) {
			t.Errorf("%s required = %v, want %v", name, tool.Parameters.Required, expected.required)
		}
	}
}

func TestUpstreamToolCatalogIdentityMatchesPinnedDatasetManifest(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "datasets", "manifests", "full-duplex-bench-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Revision string `json:"upstream_revision"`
		Harness  struct {
			AgentSHA256 string `json:"agent_sha256"`
		} `json:"official_harness"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	want := "fdb-v3/lk-agent-tool@" + manifest.Revision + "/sha256:" + manifest.Harness.AgentSHA256
	if upstreamToolCatalogIdentity != want {
		t.Fatalf("upstream catalog identity = %q, want manifest-bound %q", upstreamToolCatalogIdentity, want)
	}
}

func decodeCatalog(t *testing.T, raw []json.RawMessage) map[string]decodedCatalogTool {
	t.Helper()
	decoded := make(map[string]decodedCatalogTool, len(raw))
	for _, item := range raw {
		var tool decodedCatalogTool
		if err := json.Unmarshal(item, &tool); err != nil {
			t.Fatalf("decode catalog entry %s: %v", item, err)
		}
		if _, exists := decoded[tool.Name]; exists {
			t.Fatalf("duplicate catalog tool %q", tool.Name)
		}
		decoded[tool.Name] = tool
	}
	return decoded
}

func TestCatalogUsesPinnedSemanticsAndDatasetWideContracts(t *testing.T) {
	tasks := []Task{
		{ID: "track", Expected: []ExpectedCall{{
			Function: "track_order", Args: json.RawMessage(`{"order_id":"LEAK-UNIQUE-931"}`),
		}}},
		{ID: "search-with-budget", Expected: []ExpectedCall{{
			Function: "search_products", Args: json.RawMessage(`{"query":"LEAK-HEADPHONES-271","max_price":200}`),
		}}},
		{ID: "search-without-budget", Expected: []ExpectedCall{{
			Function: "search_products", Args: json.RawMessage(`{"query":"LEAK-SPEAKERS-442"}`),
		}}},
		{ID: "filter-string", Expected: []ExpectedCall{{
			Function: "update_search_filter", Args: json.RawMessage(`{"filter_name":"neighborhood","value":"LEAK-DOWNTOWN-815"}`),
		}}},
	}

	raw, err := Catalog(tasks)
	if err != nil {
		t.Fatal(err)
	}
	catalog := decodeCatalog(t, raw)
	if len(catalog) != 12 {
		t.Fatalf("catalog tools = %d, want exact upstream population 12", len(catalog))
	}

	track := catalog["track_order"]
	if !strings.Contains(track.Description, "EXECUTE THIS TOOL IMMEDIATELY") {
		t.Fatalf("track_order description = %q", track.Description)
	}
	if got := track.Parameters.Properties["order_id"].Description; got != "Order identifier to track, e.g. 'BOB12'" {
		t.Fatalf("order_id description = %q", got)
	}
	if strings.Contains(benchmarkInstructions, "concatenate separately dictated") ||
		strings.Contains(benchmarkInstructions, "API-ready identifier") {
		t.Fatalf("benchmark instructions contain a non-upstream identifier hint: %q", benchmarkInstructions)
	}

	search := catalog["search_products"]
	if !reflect.DeepEqual(search.Parameters.Required, []string{"query"}) {
		t.Fatalf("search_products required = %v, want [query]", search.Parameters.Required)
	}
	if _, found := search.Parameters.Properties["max_price"]; !found {
		t.Fatal("search_products omitted optional max_price")
	}
	if _, found := search.Parameters.Properties["category"]; found {
		t.Fatal("search_products exposed dataset-only category absent from pinned callable")
	}

	filterType := catalog["update_search_filter"].Parameters.Properties["value"].Type
	var filterTypeName string
	if err := json.Unmarshal(filterType, &filterTypeName); err != nil {
		t.Fatalf("update_search_filter.value type = %s: %v", filterType, err)
	}
	if filterTypeName != "string" {
		t.Fatalf("update_search_filter.value type = %q, want pinned Python str", filterTypeName)
	}
	if got := catalog["calculate_commute"].Parameters.Required; !reflect.DeepEqual(got, []string{
		"origin_address", "destination_address",
	}) {
		t.Fatalf("calculate_commute required = %v, want upstream non-default arguments", got)
	}
	if got := catalog["add_to_cart"].Parameters.Required; !reflect.DeepEqual(got, []string{"product_id"}) {
		t.Fatalf("add_to_cart required = %v, want quantity default preserved", got)
	}

	var joined []byte
	for _, item := range raw {
		joined = append(joined, item...)
	}
	if bytes.Contains(joined, []byte("x-openrealtime-normalizer")) || bytes.Contains(joined, []byte(`"pattern"`)) {
		t.Fatalf("provider-facing schema contains deployment-only normalization controls: %s", joined)
	}
	for _, leaked := range []string{
		"LEAK-UNIQUE-931", "LEAK-HEADPHONES-271", "LEAK-SPEAKERS-442", "LEAK-DOWNTOWN-815", `"enum"`,
	} {
		if bytes.Contains(joined, []byte(leaked)) {
			t.Fatalf("catalog leaked expected-answer material %q: %s", leaked, joined)
		}
	}

	reversed := slices.Clone(tasks)
	slices.Reverse(reversed)
	reordered, err := Catalog(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw, reordered) {
		t.Fatalf("catalog depends on task order:\nfirst:  %s\nsecond: %s", raw, reordered)
	}
}

func TestCatalogPinsSearchApartmentsCallableContractAcrossAnnotationOmissions(t *testing.T) {
	tasks := []Task{
		{ID: "housing_21_66c4f3cb14cbfc4db836bd4e", Expected: []ExpectedCall{
			{Function: "search_apartments", Args: json.RawMessage(`{"bedrooms":3}`)},
			{Function: "calculate_commute", Args: json.RawMessage(`{"origin_address":"$RESULT_0.apartments[0].address","destination_address":"train station","mode":"walking"}`)},
		}},
		{ID: "housing_04_695bd157114f0d2317f88617", Expected: []ExpectedCall{{
			Function: "search_apartments", Args: json.RawMessage(`{"city":"Denver","bedrooms":1}`),
		}}},
		{ID: "housing_05_5ff07b5ee7a1d23e719e421e", Expected: []ExpectedCall{{
			Function: "search_apartments", Args: json.RawMessage(`{"city":"Miami","max_price":1500,"pets_allowed":true}`),
		}}},
	}

	raw, err := Catalog(tasks)
	if err != nil {
		t.Fatal(err)
	}
	search := decodeCatalog(t, raw)["search_apartments"]
	if !reflect.DeepEqual(search.Parameters.Required, []string{"city", "bedrooms", "max_price"}) {
		t.Fatalf("search_apartments required = %v", search.Parameters.Required)
	}
	for argument, want := range map[string]string{
		"city": "string", "bedrooms": "integer", "max_price": "number",
	} {
		property, found := search.Parameters.Properties[argument]
		if !found {
			t.Fatalf("search_apartments omitted property %q", argument)
		}
		var got string
		if err := json.Unmarshal(property.Type, &got); err != nil || got != want {
			t.Fatalf("search_apartments.%s type = %s (%q, %v), want %q", argument, property.Type, got, err, want)
		}
	}
	if _, found := search.Parameters.Properties["pets_allowed"]; found {
		t.Fatal("search_apartments exposed dataset-only pets_allowed absent from pinned callable")
	}

	// Runtime-only values needed by the pinned callable remain extra arguments
	// to the historical scorer; it still compares every annotated field and no
	// others, exactly like the pinned upstream evaluator.
	score := scorePinnedUpstreamFallback(
		[]ExpectedCall{{Function: "search_apartments", Args: json.RawMessage(`{"bedrooms":3}`)}},
		[]observedCall{{Name: "search_apartments", Arguments: json.RawMessage(`{"city":"Austin","bedrooms":3,"max_price":2000}`)}},
	)
	if !score.ExactPopulationMatch || score.Arguments != 1 {
		t.Fatalf("runtime-required compatibility arguments changed upstream-static scoring: %+v", score)
	}
}

func TestCatalogRejectsSearchApartmentsTypesThatPinnedCallableCannotExecute(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args string
		want string
	}{
		{name: "city", args: `{"city":7}`, want: `requires JSON Schema type "string"`},
		{name: "bedrooms string", args: `{"bedrooms":"3"}`, want: "requires JSON Schema type integer"},
		{name: "bedrooms fractional", args: `{"bedrooms":1.5}`, want: "requires JSON Schema type integer"},
		{name: "max price", args: `{"max_price":"cheap"}`, want: `requires JSON Schema type "number"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Catalog([]Task{{ID: "bad", Expected: []ExpectedCall{{
				Function: "search_apartments", Args: json.RawMessage(testCase.args),
			}}}})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Catalog error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestReleasedSearchApartmentsOmissionsMatchPinnedCompatibilityAudit(t *testing.T) {
	root := releasedDatasetRoot(t)
	tasks, err := loadDataset(root, 0, &pinnedReleasedInventory)
	if err != nil {
		t.Fatal(err)
	}
	var calls, missingCity, missingBedrooms, missingMaxPrice int
	for _, task := range tasks {
		for _, call := range task.Expected {
			if call.Function != "search_apartments" {
				continue
			}
			calls++
			arguments, err := decodeJSONObject(call.Args)
			if err != nil {
				t.Fatalf("%s search_apartments: %v", task.ID, err)
			}
			if _, found := arguments["city"]; !found {
				missingCity++
			}
			if _, found := arguments["bedrooms"]; !found {
				missingBedrooms++
			}
			if _, found := arguments["max_price"]; !found {
				missingMaxPrice++
			}
		}
	}
	if calls != 17 || missingCity != 1 || missingBedrooms != 6 || missingMaxPrice != 8 {
		t.Fatalf(
			"released search_apartments audit: calls=%d missing city=%d bedrooms=%d max_price=%d",
			calls, missingCity, missingBedrooms, missingMaxPrice,
		)
	}
	search := decodeCatalog(t, mustCatalog(t, tasks))["search_apartments"]
	if !reflect.DeepEqual(search.Parameters.Required, []string{"city", "bedrooms", "max_price"}) {
		t.Fatalf("released search_apartments required = %v", search.Parameters.Required)
	}
}

func mustCatalog(t *testing.T, tasks []Task) []json.RawMessage {
	t.Helper()
	catalog, err := Catalog(tasks)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestArgumentNormalizersAreDeploymentOwnedAndSnapshotIsolated(t *testing.T) {
	want := map[string]string{
		"track_order": "order_id", "add_to_cart": "product_id",
		"update_identity_doc": "doc_number",
	}
	for tool := range pinnedCallableContracts {
		argument, normalized := want[tool]
		normalizers := ArgumentNormalizers(tool)
		if !normalized {
			if normalizers != nil {
				t.Fatalf("%s normalizers = %+v, want nil", tool, normalizers)
			}
			continue
		}
		if !reflect.DeepEqual(normalizers, []ArgumentNormalizer{{
			Argument: argument, Normalizer: "compact-ascii-alphanumeric-v1",
		}}) {
			t.Fatalf("%s normalizers = %+v", tool, normalizers)
		}
		normalizers[0].Argument = "mutated"
		if got := ArgumentNormalizers(tool); got[0].Argument != argument {
			t.Fatalf("%s normalizer snapshot retained mutation: %+v", tool, got)
		}
	}
	if got := ArgumentNormalizers("future_tool"); got != nil {
		t.Fatalf("unknown tool normalizers = %+v, want nil", got)
	}
}

func TestCatalogRejectsMalformedAndNonCanonicalMetadata(t *testing.T) {
	for _, testCase := range []struct {
		name string
		call ExpectedCall
		want string
	}{
		{name: "invalid JSON", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`{"order_id":`)}, want: "arguments"},
		{name: "non-object", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`[]`)}, want: "arguments"},
		{name: "null object", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`null`)}, want: "must be an object"},
		{name: "duplicate key", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`{"order_id":"one","order_id":"two"}`)}, want: "duplicate JSON key"},
		{name: "empty function", call: ExpectedCall{Function: "", Args: json.RawMessage(`{}`)}, want: "non-canonical function"},
		{name: "spaced function", call: ExpectedCall{Function: " track_order", Args: json.RawMessage(`{}`)}, want: "non-canonical function"},
		{name: "empty argument", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`{"":"one"}`)}, want: "non-canonical argument"},
		{name: "spaced argument", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`{" order_id":"one"}`)}, want: "non-canonical argument"},
		{name: "unknown function", call: ExpectedCall{Function: "invented_tool", Args: json.RawMessage(`{}`)}, want: "absent from the pinned upstream callable catalog"},
		{name: "unknown argument", call: ExpectedCall{Function: "track_order", Args: json.RawMessage(`{"tracking_hint":"one"}`)}, want: "absent from the pinned callable"},
		{name: "bad annotation-only type", call: ExpectedCall{Function: "search_products", Args: json.RawMessage(`{"category":7}`)}, want: "exact artifact-bound fixture registry"},
		{name: "unsupported filter annotation type", call: ExpectedCall{Function: "update_search_filter", Args: json.RawMessage(`{"value":[]}`)}, want: "requires JSON Schema type"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Catalog([]Task{{ID: "bad", Expected: []ExpectedCall{testCase.call}}})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Catalog error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestFocusedTaskSelectionIsExactAndCannotMixWithLimit(t *testing.T) {
	all := []Task{{ID: "one"}, {ID: "two"}, {ID: "three"}}
	selected, err := selectTasks(all, []string{"three", "one"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{selected[0].ID, selected[1].ID}; !reflect.DeepEqual(got, []string{"three", "one"}) {
		t.Fatalf("selected IDs = %v", got)
	}
	limited, err := selectTasks(all, nil, 2)
	if err != nil || len(limited) != 2 || limited[0].ID != "one" || limited[1].ID != "two" {
		t.Fatalf("limited selection = %+v, %v", limited, err)
	}
	for _, testCase := range []struct {
		name     string
		selected []string
		limit    int
		want     string
	}{
		{name: "unknown", selected: []string{"missing"}, want: "not in the released dataset"},
		{name: "duplicate", selected: []string{"one", "one"}, want: "repeated"},
		{name: "noncanonical", selected: []string{" one"}, want: "not canonical"},
		{name: "mixed limit", selected: []string{"one"}, limit: 1, want: "mutually exclusive"},
		{name: "negative limit", limit: -1, want: "cannot be negative"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := selectTasks(all, testCase.selected, testCase.limit); err == nil ||
				!strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("selectTasks error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestDiagnosticNotesRetainHeardAndSpokenTurns(t *testing.T) {
	notes := map[string]string{}
	retainTranscriptNotes(notes, bench.Transcript{Moments: []bench.Moment{
		{Kind: bench.MomentTranscript, Text: "track order A B C"},
		{Kind: bench.MomentAgentText, Text: "Checking "},
		{Kind: bench.MomentAgentText, Text: "now."},
		{Kind: bench.MomentResponseDone},
	}})
	if notes["user_transcript"] != "track order A B C" || notes["agent_transcript"] != "Checking now." {
		t.Fatalf("transcript notes = %v", notes)
	}
}
