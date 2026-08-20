package realtimegateway

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

func TestResponseUsagePreservesProviderReportedCachedInputTokens(t *testing.T) {
	t.Parallel()
	response := responseObject(
		"response", "completed", "conversation", nil,
		&continuation.Usage{
			InputTokens:               12,
			CachedInputTokens:         7,
			CachedInputTokensReported: true,
			OutputTokens:              3,
			TotalTokens:               15,
		},
		audioFormat{Type: "audio/pcm", Rate: 24000}, "alloy",
	)
	usage := response["usage"].(map[string]any)
	details := usage["input_token_details"].(map[string]any)
	cachedDetails := details["cached_tokens_details"].(map[string]any)
	if details["cached_tokens"] != int64(7) || cachedDetails["text_tokens"] != int64(7) {
		t.Fatalf("cached usage = %#v", details)
	}
}

func TestResponseUsageBoundsInvalidCachedInputCount(t *testing.T) {
	t.Parallel()
	response := responseObject(
		"response", "completed", "conversation", nil,
		&continuation.Usage{InputTokens: 4, CachedInputTokens: 9, OutputTokens: 1},
		audioFormat{Type: "audio/pcm", Rate: 24000}, "alloy",
	)
	usage := response["usage"].(map[string]any)
	details := usage["input_token_details"].(map[string]any)
	if details["cached_tokens"] != int64(4) {
		t.Fatalf("cached usage was not bounded by input: %#v", details)
	}
}
