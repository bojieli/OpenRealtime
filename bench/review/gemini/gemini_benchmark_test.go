package gemini

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

var benchmarkGeminiBytes []byte

func BenchmarkMarshalPinnedMultimodalRequest(b *testing.B) {
	_, prepared, _ := preparedMultimodalRequest(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		body, err := marshalRequest(prepared)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkGeminiBytes = body
	}
}

func BenchmarkCredentialScanOneMiBJSON(b *testing.B) {
	payload := append([]byte(`{"text":"`), bytes.Repeat([]byte("ordinary-review-text-"), 52_429)...)
	payload = append(payload, []byte(`"}`)...)
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(scanner.wipe)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if scanner.containsJSON(payload) {
			b.Fatal("ordinary bounded JSON was reported as credential-bearing")
		}
	}
}

func BenchmarkHermeticReviewAndOneUseVerification(b *testing.B) {
	_, prepared, _ := preparedMultimodalRequest(b)
	responseBody := successfulInteraction(b, testAssessment)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, responseBody), nil
		})})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = plugin.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		response, reviewErr := plugin.Review(b.Context(), prepared)
		if reviewErr != nil {
			b.Fatal(reviewErr)
		}
		if verifyErr := plugin.VerifyResponse(context.Background(), prepared, response); verifyErr != nil {
			b.Fatal(verifyErr)
		}
		benchmarkGeminiBytes = response.Raw
	}
}
