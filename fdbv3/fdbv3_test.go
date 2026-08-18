package fdbv3

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/livebench"
)

func TestCheckedProfilePinsOfficialToolCatalog(t *testing.T) {
	t.Parallel()
	profile, err := LoadProfile(filepath.Join("..", "benchmarks", "fdb-v3", "openrealtime-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Profile != "fdb-v3-openai-realtime-adapter-i1-qg-v1" || len(profile.Tools) != 12 || len(profile.SHA256) != 64 {
		t.Fatalf("unexpected checked profile: %+v", profile)
	}
}

func TestDiscoverReleasedLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "travel_01_5f4a4da1575d605c43bef871")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := livebench.WriteWAV(filepath.Join(directory, "input.wav"), livebench.Audio{SampleRateHz: 24_000, PCM16: make([]byte, 960)}); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"id":"travel_01","domain":"travel_identity","title":"test","difficulty":"easy","expected_tool_calls":[{"function":"search_flights","args":{"destination":"LHR","date":"2026-08-20"}}]}`)
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), metadata, 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].ExampleID != "travel_01" || samples[0].PID != "5f4a4da1575d605c43bef871" || len(samples[0].InputSHA256) != 64 {
		t.Fatalf("unexpected discovered samples: %+v", samples)
	}
}

func TestMockExecutorMatchesPinnedUpstreamSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments string
		want      map[string]any
	}{
		{"search_flights", `{"destination":"LHR","date":"2026-08-20"}`, map[string]any{"status": "success", "flights": []any{map[string]any{"flight_id": "FL123", "destination": "LHR", "date": "2026-08-20", "price": 450.0}}}},
		{"book_flight", `{"passenger_name":"Ada"}`, map[string]any{"status": "success", "booking_ref": "B789", "passenger": "Ada"}},
		{"update_identity_doc", `{"doc_type":"passport","doc_number":"ABC12345"}`, map[string]any{"status": "success", "updated_doc": "passport", "masked_number": "2345"}},
		{"get_card_benefits", `{"card_type":"gold"}`, map[string]any{"status": "success", "card_type": "gold", "benefits": []any{"2% Cashback", "No Foreign Transaction Fee"}}},
		{"get_exchange_rate", `{"amount":10,"from_currency":"EUR","to_currency":"USD"}`, map[string]any{"status": "success", "converted_amount": 11.0, "rate": 1.1}},
		{"modify_autopay", `{"bill_type":"utilities","source_account":"checking"}`, map[string]any{"status": "success", "autopay_enabled": true, "bill": "utilities", "source": "checking"}},
		{"search_apartments", `{"city":"Seattle","bedrooms":2,"max_price":2500}`, map[string]any{"status": "success", "city": "Seattle", "results": []any{map[string]any{"id": "APT1", "price": 2400.0, "beds": 2.0}}}},
		{"calculate_commute", `{"origin_address":"A","destination_address":"B"}`, map[string]any{"status": "success", "duration_mins": 25.0, "mode": "driving"}},
		{"update_search_filter", `{"filter_name":"pets","value":"allowed"}`, map[string]any{"status": "success", "filter_updated": "pets", "new_value": "allowed"}},
		{"track_order", `{"order_id":"BOB12"}`, map[string]any{"status": "success", "order_id": "BOB12", "shipping_status": "Out for delivery"}},
		{"search_products", `{"query":"headphones","max_price":100}`, map[string]any{"status": "success", "products": []any{map[string]any{"product_id": "PROD1", "name": "headphones Premium", "price": 90.0}}}},
		{"add_to_cart", `{"product_id":"PROD1"}`, map[string]any{"status": "success", "product_id": "PROD1", "quantity": 1.0, "cart_total": 99.99}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			output, err := MockExecutor(t.Context(), test.name, json.RawMessage(test.arguments))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatal(err)
			}
			if encodedGot, encodedWant := mustJSON(t, got), mustJSON(t, test.want); encodedGot != encodedWant {
				t.Fatalf("got %s, want %s", encodedGot, encodedWant)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
