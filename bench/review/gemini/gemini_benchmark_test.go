package gemini

import (
	"bytes"
	"context"
	"net/http"
	"strings"
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

func BenchmarkCredentialScanRelevantShortTokenCardinality(b *testing.B) {
	const tokens = 100_000
	var payload strings.Builder
	payload.Grow(tokens*7 + 2)
	payload.WriteByte('[')
	for index := 0; index < tokens; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`"test"`)
	}
	payload.WriteByte(']')
	encoded := []byte(payload.String())
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(scanner.wipe)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if scanner.containsJSON(encoded) {
			b.Fatal("relevant-fragment fixture synthesized credential material")
		}
	}
}

func BenchmarkCredentialScanEscapedShortTokenCardinality(b *testing.B) {
	const tokens = 100_000
	var payload strings.Builder
	payload.Grow(tokens*27 + 2)
	payload.WriteByte('[')
	for index := 0; index < tokens; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`"\u0074\u0065\u0073\u0074"`)
	}
	payload.WriteByte(']')
	encoded := []byte(payload.String())
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(scanner.wipe)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if scanner.containsJSON(encoded) {
			b.Fatal("escaped relevant-fragment fixture synthesized credential material")
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

func BenchmarkValidateRetainedFilesTransportEvidence(b *testing.B) {
	_, prepared, _ := preparedMultimodalRequest(b)
	raw := successfulInteraction(b, testAssessment)
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(scanner.wipe)
	transport := newFilesFixtureTransport(prepared, raw)
	client := &http.Client{Transport: transport}
	state, interactionBody, err := beginFilesTransport(
		b.Context(), prepared, testAPIKey, scanner, client,
	)
	if err != nil {
		b.Fatal(err)
	}
	cleanup := state.cleanup(b.Context(), prepared, testAPIKey, scanner, client)
	evidence, err := state.retainedEvidence(
		interactionBody, raw, http.StatusOK, "application/json", cleanup,
	)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(evidence)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		expected, validateErr := validateRetainedFilesTransport(
			b.Context(), prepared, evidence, raw,
		)
		if validateErr != nil {
			b.Fatal(validateErr)
		}
		benchmarkGeminiBytes = expected
	}
}
