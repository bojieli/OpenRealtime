package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench/review"
)

func clonePrepared(source review.PreparedRequest) review.PreparedRequest {
	result := source
	result.Schema = slices.Clone(source.Schema)
	result.Context = slices.Clone(source.Context)
	result.Media = make([]review.PreparedMedia, len(source.Media))
	for index := range source.Media {
		result.Media[index] = source.Media[index]
		result.Media[index].Bytes = slices.Clone(source.Media[index].Bytes)
	}
	return result
}

func TestPluginRejectsEveryPreparedMutationBeforeTransport(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	mutations := []struct {
		name   string
		mutate func(*review.PreparedRequest)
	}{
		{"attempt ID", func(value *review.PreparedRequest) { value.AttemptID += "-other" }},
		{"suite", func(value *review.PreparedRequest) { value.Suite += "-other" }},
		{"case", func(value *review.PreparedRequest) { value.Case += "-other" }},
		{"trial", func(value *review.PreparedRequest) { value.Trial++ }},
		{"prompt", func(value *review.PreparedRequest) { value.Prompt += "\nforged" }},
		{"schema", func(value *review.PreparedRequest) {
			value.Schema = bytes.Replace(value.Schema, []byte(`"boolean"`), []byte(`"string"`), 1)
		}},
		{"context", func(value *review.PreparedRequest) {
			value.Context = json.RawMessage(`{"deterministic_pass":false,"reportable":true}`)
		}},
		{"media role", func(value *review.PreparedRequest) { value.Media[0].Role += "_other" }},
		{"media kind", func(value *review.PreparedRequest) { value.Media[0].Kind = "image" }},
		{"media order", func(value *review.PreparedRequest) {
			value.Media[0], value.Media[1] = value.Media[1], value.Media[0]
		}},
		{"media path", func(value *review.PreparedRequest) { value.Media[0].Path += "-other" }},
		{"media validation", func(value *review.PreparedRequest) { value.Media[0].Validation += "-other" }},
		{"media size", func(value *review.PreparedRequest) { value.Media[0].SizeBytes++ }},
		{"media bytes", func(value *review.PreparedRequest) { value.Media[0].Bytes[0] ^= 1 }},
		{"fingerprint", func(value *review.PreparedRequest) { value.RequestFingerprint = digest([]byte("forged")) }},
		{"sanitization", func(value *review.PreparedRequest) { value.Sanitization += "-other" }},
		{"sensitive count", func(value *review.PreparedRequest) { value.SensitiveValueCount++ }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			candidate := clonePrepared(prepared)
			test.mutate(&candidate)
			if _, err := plugin.Review(t.Context(), candidate); err == nil ||
				!strings.Contains(err.Error(), "prepared review") {
				t.Fatalf("Review() error = %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("mutated input crossed transport %d times", calls.Load())
			}
		})
	}
}

func TestPluginRejectsActiveCredentialInOutgoingContextBeforeTransport(t *testing.T) {
	request, _, _ := preparedMultimodalRequest(t)
	request.Context = json.RawMessage(`{"credential":"` + testAPIKey + `"}`)
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if _, err := plugin.Review(t.Context(), prepared); err == nil ||
		!strings.Contains(err.Error(), "credential material") {
		t.Fatalf("Review() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("credential-bearing input crossed transport %d times", calls.Load())
	}
}

func TestPluginRejectsCredentialSplitAcrossOutgoingJSONTokensBeforeTransport(t *testing.T) {
	middle := len(testAPIKey) / 2
	half1, half2 := testAPIKey[:middle], testAPIKey[middle:]
	escapeASCII := func(value string) string {
		var result strings.Builder
		for _, character := range []byte(value) {
			_, _ = fmt.Fprintf(&result, `\u%04x`, character)
		}
		return result.String()
	}
	escaped := strings.Replace(testAPIKey, "-", `\u002d`, 1)
	nested, err := json.Marshal(map[string]string{
		"outer": `prefix {"key":"` + escaped + `"} suffix`,
	})
	if err != nil {
		t.Fatal(err)
	}
	nestedSplit, err := json.Marshal(map[string]string{
		"outer": `prefix {"a":"` + half1 + `","b":"` + half2 + `"} suffix`,
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedThenPlain, err := json.Marshal(map[string]string{
		"a": escapeASCII(half1), "b": half2,
	})
	if err != nil {
		t.Fatal(err)
	}
	plainThenEncoded, err := json.Marshal(map[string]string{
		"a": half1, "b": escapeASCII(half2),
	})
	if err != nil {
		t.Fatal(err)
	}
	doubleEncodedThenPlain, err := json.Marshal(map[string]string{
		"a": strings.ReplaceAll(escapeASCII(half1), `\`, `\\`), "b": half2,
	})
	if err != nil {
		t.Fatal(err)
	}
	heterogeneousEscapedDepths, err := json.Marshal(map[string]string{
		"a": escapeASCII(half1),
		"b": strings.ReplaceAll(escapeASCII(half2), `\`, `\\`),
	})
	if err != nil {
		t.Fatal(err)
	}
	reverseHeterogeneousDepths, err := json.Marshal(map[string]string{
		"a": strings.ReplaceAll(escapeASCII(half1), `\`, `\\`),
		"b": escapeASCII(half2),
	})
	if err != nil {
		t.Fatal(err)
	}
	atDepth := func(value string, depth int) string {
		encoded := escapeASCII(value)
		for level := 1; level < depth; level++ {
			encoded = strings.ReplaceAll(encoded, `\`, `\\`)
		}
		return encoded
	}
	marshalString := func(value string) string {
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return string(payload)
	}
	third1, third2 := len(testAPIKey)/3, 2*len(testAPIKey)/3
	threeAlternatingDepths := json.RawMessage(
		`{"a":` + marshalString(atDepth(testAPIKey[:third1], 1)) +
			`,"b":` + marshalString(atDepth(testAPIKey[third1:third2], 3)) +
			`,"c":` + marshalString(atDepth(testAPIKey[third2:], 2)) + `}`,
	)
	keyThenValueDepths := json.RawMessage(
		`{` + marshalString(atDepth(half1, 1)) + `:` + marshalString(atDepth(half2, 2)) + `}`,
	)
	valueThenKeyDepths := json.RawMessage(
		`{"!":` + marshalString(atDepth(half1, 1)) + `,` +
			marshalString(atDepth(half2, 2)) + `:1}`,
	)
	noisyEscapedHalves, err := json.Marshal(map[string]string{
		"a": "noise:" + atDepth(half1, 1) + ":noise",
		"b": "noise:" + atDepth(half2, 2) + ":noise",
	})
	if err != nil {
		t.Fatal(err)
	}
	parentAndEmbedded, err := json.Marshal(map[string]string{
		"outer": "noise:" + half1 + `:{"x":"` + half2 + `"}:noise`,
	})
	if err != nil {
		t.Fatal(err)
	}
	contexts := map[string]json.RawMessage{
		"values":                    json.RawMessage(`{"a":"` + half1 + `","b":"` + half2 + `"}`),
		"keys":                      json.RawMessage(`{"` + half1 + `":1,"` + half2 + `":2}`),
		"key value":                 json.RawMessage(`{"` + half1 + `":"` + half2 + `"}`),
		"prefixed nested escape":    nested,
		"prefixed nested split":     nestedSplit,
		"encoded then plain":        encodedThenPlain,
		"plain then encoded":        plainThenEncoded,
		"double encoded then plain": doubleEncodedThenPlain,
		"heterogeneous depths":      heterogeneousEscapedDepths,
		"reverse heterogeneous":     reverseHeterogeneousDepths,
		"three alternating depths":  threeAlternatingDepths,
		"key then value depths":     keyThenValueDepths,
		"value then key depths":     valueThenKeyDepths,
		"noisy plain halves": json.RawMessage(
			`{"a":"noise:` + half1 + `:noise","b":"noise:` + half2 + `:noise"}`),
		"noisy escaped halves":   noisyEscapedHalves,
		"parent embedded halves": parentAndEmbedded,
		"string numeric value": json.RawMessage(
			`{"a":"` + strings.TrimSuffix(testAPIKey, "123456789") + `","b":123456789}`),
	}
	for name, contextJSON := range contexts {
		t.Run(name, func(t *testing.T) {
			request, _, _ := preparedMultimodalRequest(t)
			request.Context = contextJSON
			prepared, err := review.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			if _, err := plugin.Review(t.Context(), prepared); err == nil ||
				!strings.Contains(err.Error(), "credential material") ||
				strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("Review() error = %v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("split credential crossed transport %d times", calls.Load())
			}
		})
	}
}

func TestCredentialScannerAcceptsNearCapPlainJSON(t *testing.T) {
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	prefix, suffix := []byte(`{"value":"`), []byte(`"}`)
	plainBytes := maximumInlineRequestBytes - len(prefix) - len(suffix) - 1024
	payload := make([]byte, 0, len(prefix)+plainBytes+len(suffix))
	payload = append(payload, prefix...)
	payload = append(payload, bytes.Repeat([]byte{'a'}, plainBytes)...)
	payload = append(payload, suffix...)
	contains, err := scanner.containsJSONContext(t.Context(), payload)
	if err != nil || contains {
		t.Fatalf("near-cap plain scan = %t, %v", contains, err)
	}
}

func TestCredentialFragmentCoverageDoesNotReuseOneRepeatedOccurrence(t *testing.T) {
	const repeatedCredential = "aaaaaaaaaaaaaaaa"
	scanner, err := newJSONCredentialScanner(repeatedCredential)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	if scanner.containsJSON([]byte(`{"value":"aaaa"}`)) {
		t.Fatal("one short repeated fragment falsely covered the complete credential")
	}
}

func TestCredentialFragmentEvidenceDoesNotReuseOverlappingBytes(t *testing.T) {
	const credential = "abcdbcdeabcdbcde"
	scanner, err := newJSONCredentialScanner(credential)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	if scanner.containsJSON([]byte(`{"a":"abcde","b":"abcde"}`)) {
		t.Fatal("two five-byte fragments reused overlapping bytes to synthesize a 16-byte credential")
	}
	evidence := newCredentialFragmentEvidence(len(credential))
	for occurrence := 1; occurrence <= 4; occurrence++ {
		contains := scanner.markCredentialFragment([]byte("abcde"), evidence)
		if contains != (occurrence == 4) {
			t.Fatalf("fragment %d contains = %t, capacity=%d", occurrence, contains, evidence.capacity)
		}
	}
}

func TestCredentialFragmentEvidenceRetainsAlternativeRepeatedAssignments(t *testing.T) {
	const credential = "abcdWXYZefghWXYZ"
	scanner, err := newJSONCredentialScanner(credential)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	evidence := newCredentialFragmentEvidence(len(credential))
	fragments := []string{"WXYZ", "abcd", "dWXY", "YZef", "efgh"}
	for index, fragment := range fragments {
		contains := scanner.markCredentialFragment([]byte(fragment), evidence)
		if contains != (index == len(fragments)-1) {
			t.Fatalf("fragment %d %q contains = %t, capacity=%d",
				index, fragment, contains, evidence.capacity)
		}
	}
}

func TestCredentialUnorderedFragmentPolicyHasFourByteMinimum(t *testing.T) {
	const credential = "abcdefghijklmnop"
	scanner, err := newJSONCredentialScanner(credential)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	payload := []byte(`{"a":"abc#","b":"def#","c":"ghi#","d":"jkl#","e":"mno#","f":"p#"}`)
	if scanner.containsJSON(payload) {
		t.Fatal("unordered fragments shorter than four bytes crossed the documented policy boundary")
	}
	if !scanner.containsJSON([]byte(`{"a":"mnopijkl","b":"efghabcd"}`)) {
		t.Fatal("four-byte unordered fragment evidence was incorrectly made stream-order dependent")
	}
}

func TestCredentialScannerShortTokenCardinalityIsAllocationBounded(t *testing.T) {
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	const tokens = 100_000
	var payload strings.Builder
	payload.Grow(tokens*7 + 2)
	payload.WriteByte('[')
	for index := 0; index < tokens; index++ {
		if index > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`"safe"`)
	}
	payload.WriteByte(']')
	encoded := []byte(payload.String())
	if scanner.containsJSON(encoded) {
		t.Fatal("safe short-token fixture contained credential material")
	}
	allocations := testing.AllocsPerRun(3, func() {
		if scanner.containsJSON(encoded) {
			t.Fatal("safe short-token fixture contained credential material")
		}
	})
	if allocations > 128 {
		t.Fatalf("short-token scan allocations = %.0f, want <= 128", allocations)
	}
}

func TestCredentialScannerRelevantFragmentCardinalityIsAllocationBounded(t *testing.T) {
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	// "test" is a real four-byte atom from testAPIKey. Repeating it in many
	// independent JSON values exercises the conservative unordered-fragment
	// path without providing enough distinct physical bytes or credential
	// coverage to reconstruct the credential.
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
	if scanner.containsJSON(encoded) {
		t.Fatal("relevant-fragment fixture synthesized credential material")
	}
	allocations := testing.AllocsPerRun(3, func() {
		if scanner.containsJSON(encoded) {
			t.Fatal("relevant-fragment fixture synthesized credential material")
		}
	})
	if allocations > 160 {
		t.Fatalf("relevant-fragment scan allocations = %.0f, want <= 160", allocations)
	}
}

func TestCredentialScannerEscapedRelevantCardinalityIsAllocationBounded(t *testing.T) {
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
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
	if scanner.containsJSON(encoded) {
		t.Fatal("escaped relevant-fragment fixture synthesized credential material")
	}
	allocations := testing.AllocsPerRun(3, func() {
		if scanner.containsJSON(encoded) {
			t.Fatal("escaped relevant-fragment fixture synthesized credential material")
		}
	})
	if allocations > 160 {
		t.Fatalf("escaped relevant-fragment scan allocations = %.0f, want <= 160", allocations)
	}
}

func TestCredentialScannerDoesNotReplayOneNestedRepeatedHalf(t *testing.T) {
	const credential = "abcdefghabcdefgh"
	payload, err := json.Marshal(map[string]string{
		"outer": `{"x":"abcdefgh"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := newJSONCredentialScanner(credential)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	if scanner.containsJSON(payload) {
		t.Fatal("one nested repeated half was counted as two independent credential fragments")
	}
}

func TestCredentialNormalizedStreamCrossesDepthAndRoleBoundaries(t *testing.T) {
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	escape := func(value string, depth int) []byte {
		var encoded strings.Builder
		for _, character := range []byte(value) {
			_, _ = fmt.Fprintf(&encoded, `\u%04x`, character)
		}
		result := encoded.String()
		for level := 1; level < depth; level++ {
			result = strings.ReplaceAll(result, `\`, `\\`)
		}
		return []byte(result)
	}
	middle := len(testAPIKey) / 2
	normalized, work := 0, int64(0)
	for index, part := range []struct {
		payload []byte
		depth   int
	}{
		{payload: escape(testAPIKey[:middle], 1), depth: 1},
		{payload: escape(testAPIKey[middle:], 2), depth: 2},
	} {
		matched, active := make([]int, 32), make([]bool, 32)
		evidence := newCredentialFragmentEvidence(len(testAPIKey))
		contains, scanErr := scanner.containsEscapedLayers(
			t.Context(), part.payload, matched, active, 0, &normalized, evidence, &work, 1<<20,
		)
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		if contains != (index == 1) {
			t.Fatalf("part %d depth %d contains = %t, normalized state=%d",
				index, part.depth, contains, normalized)
		}
	}
}

func TestPluginRejectsEscapedCredentialInDeepThoughtSummary(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	escaped := strings.ReplaceAll(testAPIKey, "-", `\u002d`)
	responseBody := bytes.Replace(
		successfulInteraction(t, testAssessment), []byte("reviewed evidence"), []byte(escaped), 1,
	)
	if bytes.Contains(responseBody, []byte(testAPIKey)) {
		t.Fatal("fixture contains the literal credential; escaped-path coverage is invalid")
	}
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, responseBody), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if _, err := plugin.Review(t.Context(), prepared); err == nil ||
		!strings.Contains(err.Error(), "credential material") || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("Review() error = %v", err)
	}
}

func TestPluginRejectsBase64CredentialInSuccessAndHTTPError(t *testing.T) {
	encodings := map[string]string{
		"standard": base64.StdEncoding.EncodeToString([]byte(testAPIKey)),
		"raw URL":  base64.RawURLEncoding.EncodeToString([]byte(testAPIKey)),
	}
	for encodingName, transformed := range encodings {
		for _, response := range []struct {
			name   string
			status int
			body   func() []byte
		}{
			{name: "success", status: http.StatusOK, body: func() []byte {
				return bytes.Replace(
					successfulInteraction(t, testAssessment),
					[]byte("reviewed evidence"), []byte(transformed), 1,
				)
			}},
			{name: "HTTP error", status: http.StatusBadRequest, body: func() []byte {
				payload, err := json.Marshal(map[string]any{
					"error": map[string]string{
						"message": transformed, "status": "INVALID_ARGUMENT",
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return payload
			}},
		} {
			t.Run(encodingName+"/"+response.name, func(t *testing.T) {
				body := response.body()
				if bytes.Contains(body, []byte(testAPIKey)) {
					t.Fatal("base64 fixture contains the literal credential")
				}
				_, prepared, _ := preparedMultimodalRequest(t)
				plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
					func(*http.Request) (*http.Response, error) {
						return jsonResponse(response.status, body), nil
					})})
				if err != nil {
					t.Fatal(err)
				}
				defer plugin.Close()
				_, reviewErr := plugin.Review(t.Context(), prepared)
				if reviewErr == nil || strings.Contains(reviewErr.Error(), testAPIKey) ||
					strings.Contains(reviewErr.Error(), transformed) {
					t.Fatalf("Review() error = %v", reviewErr)
				}
				if !strings.Contains(reviewErr.Error(), "credential material") {
					t.Fatalf("Review() error = %v", reviewErr)
				}
			})
		}
	}
}

func TestPluginRejectsPrefixedNestedEscapedCredentialInSuccessAndHTTPError(t *testing.T) {
	escaped := strings.Replace(testAPIKey, "-", `\u002d`, 1)
	injected := `prefix {"key":"` + escaped + `"} suffix`
	encoded, err := json.Marshal(injected)
	if err != nil {
		t.Fatal(err)
	}
	encoded = encoded[1 : len(encoded)-1]
	success := bytes.Replace(
		successfulInteraction(t, testAssessment), []byte("reviewed evidence"), encoded, 1,
	)
	errorEnvelope, err := json.Marshal(map[string]any{
		"error": map[string]string{"message": injected, "status": "INVALID_ARGUMENT"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(success, []byte(testAPIKey)) || bytes.Contains(errorEnvelope, []byte(testAPIKey)) {
		t.Fatal("prefixed nested fixture contains the literal credential")
	}
	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "success", status: http.StatusOK, body: success},
		{name: "HTTP error", status: http.StatusBadRequest, body: errorEnvelope},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, prepared, _ := preparedMultimodalRequest(t)
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					return jsonResponse(test.status, test.body), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			_, err = plugin.Review(t.Context(), prepared)
			if err == nil || strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("Review() error = %v", err)
			}
		})
	}
}

func TestHTTPErrorEnvelopeHighCardinalityFailsBeforeUnmarshal(t *testing.T) {
	fields := make(map[string]any, 512)
	for index := 0; index < 512; index++ {
		fields[fmt.Sprintf("field_%03d", index)] = index
	}
	fields["error"] = map[string]string{"message": "safe", "status": "INVALID_ARGUMENT"}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	err = interactionHTTPError(
		t.Context(), http.StatusBadRequest, raw, testAPIKey, scanner,
	)
	if err == nil || err.Error() != "Gemini Interactions API returned HTTP 400" {
		t.Fatalf("interactionHTTPError() = %v", err)
	}
	allocations := testing.AllocsPerRun(20, func() {
		_ = interactionHTTPError(
			context.Background(), http.StatusBadRequest, raw, testAPIKey, scanner,
		)
	})
	if allocations > 96 {
		t.Fatalf("high-cardinality error allocations = %.1f, want <= 96", allocations)
	}
}

func TestCredentialScanPrecedesSemanticErrorsForEscapedKeyAlphabet(t *testing.T) {
	const credential = "key-\"quoted\"\\backslash-123456"
	_, prepared, _ := preparedMultimodalRequest(t)
	var envelope map[string]any
	if err := json.Unmarshal(successfulInteraction(t, testAssessment), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["model"] = credential
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(credential)) {
		t.Fatal("fixture contains the literal credential instead of a JSON-escaped form")
	}
	plugin, err := newWithHTTPClient(credential, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, raw), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	_, err = plugin.Review(t.Context(), prepared)
	if err == nil || !strings.Contains(err.Error(), "credential material") ||
		strings.Contains(err.Error(), credential) {
		t.Fatalf("Review() escaped semantic credential error = %v", err)
	}
}

func TestDirectPluginReviewRedactsDeclaredSecretsFromEveryResponsePath(t *testing.T) {
	const secret = "provider-declared-secret-123"
	request, _, _ := preparedMultimodalRequest(t)
	request.SensitiveValues = []string{secret}
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	valid := successfulInteraction(t, testAssessment)
	escapedSecret := strings.ReplaceAll(secret, "-", `\u002d`)
	escapedAssessment := bytes.Replace(
		testAssessment,
		[]byte("The audiovisual evidence agrees with the deterministic scorer."),
		[]byte(escapedSecret), 1,
	)
	nestedOutput := successfulInteraction(t, escapedAssessment)
	var thoughtEnvelope map[string]any
	if err := json.Unmarshal(valid, &thoughtEnvelope); err != nil {
		t.Fatal(err)
	}
	thoughtSteps := thoughtEnvelope["steps"].([]any)
	thoughtSummary := thoughtSteps[0].(map[string]any)["summary"].([]any)
	thoughtSummary[0].(map[string]any)["text"] = `{"value":"` + escapedSecret + `"}`
	nestedThought, err := json.Marshal(thoughtEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	nestedHTTPError, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": `{"value":"` + escapedSecret + `"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for label, payload := range map[string][]byte{
		"nested output": nestedOutput, "nested thought": nestedThought,
		"nested HTTP error": nestedHTTPError,
	} {
		if bytes.Contains(payload, []byte(secret)) {
			t.Fatalf("%s fixture contains the literal secret", label)
		}
	}
	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"HTTP error message", http.StatusBadRequest,
			[]byte(`{"error":{"message":"` + secret + `"}}`)},
		{"reported model", http.StatusOK,
			bytes.Replace(valid, []byte(ModelID), []byte(secret), 1)},
		{"interaction status", http.StatusOK,
			bytes.Replace(valid, []byte(`"completed"`), []byte(`"`+secret+`"`), 1)},
		{"strict JSON duplicate key", http.StatusOK,
			[]byte(`{"provider\u002ddeclared\u002dsecret\u002d123":1,"provider\u002ddeclared-secret-123":2}`)},
		{"thought-only raw", http.StatusOK,
			bytes.Replace(valid, []byte("reviewed evidence"), []byte(secret), 1)},
		{"nested escaped model output", http.StatusOK, nestedOutput},
		{"nested escaped thought", http.StatusOK, nestedThought},
		{"nested escaped HTTP error", http.StatusBadRequest, nestedHTTPError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return jsonResponse(test.status, test.body), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			_, err = plugin.Review(t.Context(), prepared)
			if err == nil || strings.Contains(err.Error(), secret) ||
				!strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Review() error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("response guard transport calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestDirectPluginVerifyResponseRedactsDeclaredSecrets(t *testing.T) {
	const secret = "provider-declared-secret-123"
	request, _, _ := preparedMultimodalRequest(t)
	request.SensitiveValues = []string{secret}
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	valid := successfulInteraction(t, testAssessment)
	escapedSecret := strings.ReplaceAll(secret, "-", `\u002d`)
	var nestedEnvelope map[string]any
	if err := json.Unmarshal(valid, &nestedEnvelope); err != nil {
		t.Fatal(err)
	}
	nestedSteps := nestedEnvelope["steps"].([]any)
	nestedSummary := nestedSteps[0].(map[string]any)["summary"].([]any)
	nestedSummary[0].(map[string]any)["text"] = `{"value":"` + escapedSecret + `"}`
	nestedRaw, err := json.Marshal(nestedEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, valid), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*review.ProviderResponse)
	}{
		{"reported model field", func(value *review.ProviderResponse) { value.ReportedModel = secret }},
		{"status in raw", func(value *review.ProviderResponse) {
			value.Raw = bytes.Replace(value.Raw, []byte(`"completed"`), []byte(`"`+secret+`"`), 1)
		}},
		{"strict JSON in raw", func(value *review.ProviderResponse) {
			value.Raw = []byte(`{"provider\u002ddeclared\u002dsecret\u002d123":1,"provider\u002ddeclared-secret-123":2}`)
		}},
		{"thought-only raw", func(value *review.ProviderResponse) {
			value.Raw = bytes.Replace(value.Raw, []byte("reviewed evidence"), []byte(secret), 1)
		}},
		{"nested escaped thought raw", func(value *review.ProviderResponse) {
			value.Raw = nestedRaw
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := response
			candidate.Raw = slices.Clone(response.Raw)
			candidate.Output = slices.Clone(response.Output)
			candidate.Request = slices.Clone(response.Request)
			test.mutate(&candidate)
			err := plugin.VerifyResponse(context.Background(), prepared, candidate)
			if err == nil || strings.Contains(err.Error(), secret) ||
				!strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("VerifyResponse() error = %v", err)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("verification fixture transport calls = %d, want 1", calls.Load())
	}
}

func TestPluginDistinguishesNullMissingAndEmptyRequestIDs(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	valid := successfulInteraction(t, testAssessment)
	for _, test := range []struct {
		name      string
		body      []byte
		wantState string
		wantErr   string
	}{
		{
			name: "null", body: bytes.Replace(
				valid, []byte(`"id":"interaction-request-1"`), []byte(`"id":null`), 1,
			), wantState: review.ProviderRequestIDNull,
		},
		{
			name: "missing", body: bytes.Replace(
				valid, []byte(`"id":"interaction-request-1",`), nil, 1,
			), wantState: review.ProviderRequestIDMissing,
		},
		{
			name: "empty", body: bytes.Replace(
				valid, []byte(`"id":"interaction-request-1"`), []byte(`"id":""`), 1,
			), wantErr: "invalid request ID",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusOK, test.body), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			response, err := plugin.Review(t.Context(), prepared)
			if test.wantErr == "" {
				if err != nil || response.RequestID != "" || response.RequestIDState != test.wantState {
					t.Fatalf("Review() = %+v, %v", response, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Review() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

type cancelingBody struct {
	payload []byte
	cancel  func()
	read    bool
}

func (body *cancelingBody) Read(target []byte) (int, error) {
	if body.read {
		return 0, io.EOF
	}
	body.read = true
	body.cancel()
	count := copy(target, body.payload)
	return count, io.EOF
}

func (*cancelingBody) Close() error { return nil }

type cancelingErrorBody struct {
	cancel func()
}

func (body *cancelingErrorBody) Read([]byte) (int, error) {
	body.cancel()
	return 0, errors.New("response body failed after cancellation")
}

func (*cancelingErrorBody) Close() error { return nil }

func TestPluginCannotTurnTransportOrBodyCancellationIntoSuccess(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	for _, cancelAt := range []string{"transport", "body", "body error"} {
		t.Run(cancelAt, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			responseBody := successfulInteraction(t, testAssessment)
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					if cancelAt == "transport" {
						cancel()
						return jsonResponse(http.StatusOK, responseBody), nil
					}
					response := jsonResponse(http.StatusOK, responseBody)
					if cancelAt == "body error" {
						response.Body = &cancelingErrorBody{cancel: cancel}
					} else {
						response.Body = &cancelingBody{payload: responseBody, cancel: cancel}
					}
					return response, nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			if _, err := plugin.Review(ctx, prepared); !errors.Is(err, context.Canceled) {
				t.Fatalf("Review() error = %v, want cancellation", err)
			}
		})
	}
}

func TestCredentialBearingCancellationCausesAreCanonicalAndRedacted(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	assertCanceledWithoutCredential := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), testAPIKey) {
			t.Fatalf("error = %v, want canonical cancellation without credential", err)
		}
	}

	t.Run("initial review", func(t *testing.T) {
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				t.Fatal("pre-canceled request crossed the transport boundary")
				return nil, errors.New("unreachable")
			})})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errors.New(testAPIKey))
		_, err = plugin.Review(ctx, prepared)
		assertCanceledWithoutCredential(t, err)
	})

	t.Run("transport", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				cancel(errors.New(testAPIKey))
				return nil, errors.New("transport reflected " + testAPIKey)
			})})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		_, err = plugin.Review(ctx, prepared)
		assertCanceledWithoutCredential(t, err)
	})

	t.Run("response body", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		responseBody := successfulInteraction(t, testAssessment)
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				response := jsonResponse(http.StatusOK, responseBody)
				response.Body = &cancelingBody{
					payload: responseBody,
					cancel:  func() { cancel(errors.New(testAPIKey)) },
				}
				return response, nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		_, err = plugin.Review(ctx, prepared)
		assertCanceledWithoutCredential(t, err)
	})

	t.Run("credential source", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		registration := registrationWithHTTPClient(func(context.Context) (string, error) {
			cancel(errors.New(testAPIKey))
			return testAPIKey, nil
		}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("canceled credential source opened a transport")
			return nil, errors.New("unreachable")
		})})
		registry, err := review.NewRegistry([]review.Registration{registration})
		if err != nil {
			t.Fatal(err)
		}
		_, err = registry.Open(ctx, RegistrationName)
		assertCanceledWithoutCredential(t, err)
	})
}

func TestPluginRejectsCredentialSynthesizedOnlyByBase64Encoding(t *testing.T) {
	const encodedCredential = "QUJDREVGR0hJSktM" // base64 of ABCDEFGHIJKL
	request, _, _ := preparedMultimodalRequest(t)
	payload := slices.Clone(geminiTestWAV()[:44])
	payload = append(payload, 0) // align the following bytes to a base64 quantum
	payload = append(payload, []byte("ABCDEFGHIJKL")...)
	payload = append(payload, 0) // retain PCM16 block alignment
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
	if bytes.Contains(payload, []byte(encodedCredential)) {
		t.Fatal("raw fixture contains the credential; encoding-boundary coverage is invalid")
	}
	path := filepath.Join(request.RootDirectory, request.Media[0].Path)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(payload)
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if preparedContainsCredential(prepared, encodedCredential) {
		t.Fatal("credential unexpectedly existed before wire encoding")
	}
	wire, err := marshalRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(encodedCredential)) {
		t.Fatal("fixture did not synthesize the credential in the base64 wire request")
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(encodedCredential, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if _, err := plugin.Review(t.Context(), prepared); err == nil ||
		!strings.Contains(err.Error(), "encoded review request contains credential material") ||
		strings.Contains(err.Error(), encodedCredential) {
		t.Fatalf("Review() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("encoded credential crossed transport %d times", calls.Load())
	}
}

func TestPluginRejectsDeclaredSecretSynthesizedOnlyByBase64Encoding(t *testing.T) {
	const declaredSecret = "QUJDREVGR0hJSktM" // base64 of ABCDEFGHIJKL
	request, _, _ := preparedMultimodalRequest(t)
	payload := slices.Clone(geminiTestWAV()[:44])
	payload = append(payload, 0)
	payload = append(payload, []byte("ABCDEFGHIJKL")...)
	payload = append(payload, 0)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
	path := filepath.Join(request.RootDirectory, request.Media[0].Path)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(payload)
	request.SensitiveValues = []string{declaredSecret}
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := marshalRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(declaredSecret)) ||
		containsCredentialJSON(wire, testAPIKey) {
		t.Fatal("fixture does not isolate the declared-secret guard from the active-key guard")
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if _, err := plugin.Review(t.Context(), prepared); err == nil ||
		!strings.Contains(err.Error(), "declared sensitive material") ||
		strings.Contains(err.Error(), declaredSecret) {
		t.Fatalf("Review() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("encoded declared secret crossed transport %d times", calls.Load())
	}
}

func TestPluginScansLargeNestedInlineRequestWithoutRecursiveReplay(t *testing.T) {
	request, _, _ := preparedMultimodalRequest(t)
	values := make([]string, 4000)
	for index := range values {
		values[index] = fmt.Sprintf(
			`{"frame":%d,"text":"ordinary retained trace %d"}`, index, index,
		)
	}
	contextPayload, err := json.Marshal(map[string]any{"trace": values})
	if err != nil {
		t.Fatal(err)
	}
	request.Context = contextPayload
	request.SensitiveValues = []string{strings.Repeat("Z", 64)}
	largeWAV := make([]byte, 8<<20)
	copy(largeWAV, geminiTestWAV())
	binary.LittleEndian.PutUint32(largeWAV[4:8], uint32(len(largeWAV)-8))
	binary.LittleEndian.PutUint32(largeWAV[40:44], uint32(len(largeWAV)-44))
	if err := os.WriteFile(
		filepath.Join(request.RootDirectory, request.Media[0].Path), largeWAV, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(largeWAV)
	prepared, err := review.PrepareContext(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatalf("large inline Review() produced a bounded-scan false positive: %v", err)
	}
	if len(response.Request) <= len(largeWAV) || calls.Load() != 1 {
		t.Fatalf("large inline request bytes=%d calls=%d", len(response.Request), calls.Load())
	}
	if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
		t.Fatalf("large inline VerifyResponse() produced a bounded-scan false positive: %v", err)
	}
}

func TestPinnedInteractionCapabilityTableClaimsOnlyEvidencedInlineFormats(t *testing.T) {
	want := []string{
		"audio/wav", "image/png", "video/mp4",
	}
	capabilities := interactionInlineMediaCapabilities()
	if len(capabilities) != len(want) {
		t.Fatalf("runtime capabilities = %v, want %v", capabilities, want)
	}
	for index, capability := range capabilities {
		if capability.mediaType != want[index] ||
			!strings.HasPrefix(capability.mediaType, capability.kind+"/") ||
			!supportedMediaType(capability.kind, capability.mediaType) {
			t.Fatalf("runtime capability %d = %+v, want %q and accepted", index, capability, want[index])
		}
	}
	for _, test := range []struct{ kind, mediaType string }{
		{"audio", "audio/mp3"},
		{"audio", "audio/flac"},
		{"audio", "audio/m4a"},
		{"audio", "audio/ogg"},
		{"audio", "audio/opus"},
		{"image", "image/gif"},
		{"image", "image/jpeg"},
		{"image", "image/webp"},
		{"image", "image/bmp"},
		{"video", "video/3gpp"},
		{"video", "video/mov"},
		{"video", "video/webm"},
		{"audio", "audio/webm"},
	} {
		if supportedMediaType(test.kind, test.mediaType) {
			t.Errorf("supportedMediaType(%q, %q) unexpectedly accepted",
				test.kind, test.mediaType)
		}
	}
	configuration := productionConfigurationArtifact()
	if !bytes.Contains(configuration, []byte("video/mp4")) ||
		bytes.Contains(configuration, []byte("video/webm")) ||
		bytes.Contains(configuration, []byte("audio/webm")) {
		t.Fatal("stable-v1 configuration artifact does not exactly claim bounded video without WebM")
	}
	var artifact struct {
		Supported      []string `json:"supported_inline_media_types"`
		FilesSupported []string `json:"supported_files_media_types"`
		MaxCount       int      `json:"inline_media_max_count"`
		MaxBytes       int64    `json:"inline_media_max_bytes"`
		FilesMaxBytes  int64    `json:"files_media_max_bytes"`
		FilesEndpoint  string   `json:"files_upload_endpoint"`
		FilesEvidence  string   `json:"files_transport_evidence"`
		FilesSHA256    string   `json:"files_sha256_encoding"`
	}
	if err := json.Unmarshal(configuration, &artifact); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(artifact.Supported, want) || !slices.Equal(artifact.FilesSupported, want) ||
		artifact.MaxCount != 3 || artifact.MaxBytes != maximumInlineMediaBytes ||
		artifact.FilesMaxBytes != maximumFileMediaBytes || artifact.FilesEndpoint != filesUploadURL ||
		artifact.FilesEvidence != filesTransportEvidenceFormat ||
		artifact.FilesSHA256 != "base64_lowercase_hex_digest_bytes_v1" ||
		providerCapabilities().MaximumMediaCount != 3 ||
		providerCapabilities().MaximumMediaBytes != maximumFileMediaBytes {
		t.Fatalf("configuration capabilities = %+v", artifact)
	}
}

func TestInlineMediaAggregateBoundariesWithScenarioVisualShape(t *testing.T) {
	makeWAV := func(size int) []byte {
		if size < 44 || (size-44)%2 != 0 {
			t.Fatalf("invalid WAV fixture size %d", size)
		}
		payload := make([]byte, size)
		copy(payload[0:4], "RIFF")
		binary.LittleEndian.PutUint32(payload[4:8], uint32(size-8))
		copy(payload[8:12], "WAVE")
		copy(payload[12:16], "fmt ")
		binary.LittleEndian.PutUint32(payload[16:20], 16)
		binary.LittleEndian.PutUint16(payload[20:22], 1)
		binary.LittleEndian.PutUint16(payload[22:24], 1)
		binary.LittleEndian.PutUint32(payload[24:28], 24_000)
		binary.LittleEndian.PutUint32(payload[28:32], 48_000)
		binary.LittleEndian.PutUint16(payload[32:34], 2)
		binary.LittleEndian.PutUint16(payload[34:36], 16)
		copy(payload[36:40], "data")
		binary.LittleEndian.PutUint32(payload[40:44], uint32(size-44))
		return payload
	}
	for _, test := range []struct {
		name      string
		delta     int
		wantFiles bool
	}{
		{name: "just_under", delta: -2},
		{name: "at", delta: 0},
		{name: "over", delta: 2, wantFiles: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _, payloads := preparedMultimodalRequest(t)
			// Match scenario case 10 exactly: one stereo WAV followed by two
			// submitted PNG frames. Provider admission is an aggregate budget,
			// not a per-file budget that can be reset for each visual input.
			var secondPNGBuffer bytes.Buffer
			secondImage := image.NewRGBA(image.Rect(0, 0, 2, 2))
			secondImage.Set(1, 1, color.RGBA{R: 0x10, G: 0x90, B: 0xe0, A: 0xff})
			if err := png.Encode(&secondPNGBuffer, secondImage); err != nil {
				t.Fatal(err)
			}
			secondPNG := secondPNGBuffer.Bytes()
			secondPNGPath := "screen-second.png"
			if err := os.WriteFile(
				filepath.Join(request.RootDirectory, secondPNGPath), secondPNG, 0o600,
			); err != nil {
				t.Fatal(err)
			}
			request.Media[2] = review.Media{
				Path: secondPNGPath, Kind: "image", Role: "submitted_visual_input_02",
				MediaType: "image/png", SHA256: digest(secondPNG),
			}
			target := maximumInlineMediaBytes - len(payloads[1]) - len(secondPNG) + test.delta
			wav := makeWAV(target)
			if err := os.WriteFile(
				filepath.Join(request.RootDirectory, request.Media[0].Path), wav, 0o600,
			); err != nil {
				t.Fatal(err)
			}
			request.Media[0].SHA256 = digest(wav)
			prepared, err := review.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			if got := useFilesTransport(prepared); got != test.wantFiles {
				t.Fatalf("Files transport selection = %t, want %t", got, test.wantFiles)
			}
			if test.wantFiles {
				// The full create/review/cleanup/bundle path for this exact first
				// over-boundary shape is exercised in files_test.go.
				return
			}
			var calls atomic.Int32
			plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
				func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			defer plugin.Close()
			_, err = plugin.Review(t.Context(), prepared)
			if err != nil || calls.Load() != 1 {
				t.Fatalf("exact-limit Review() = %v; calls=%d", err, calls.Load())
			}
		})
	}

	request, _, _ := preparedMultimodalRequest(t)
	extra := geminiTestWAV()
	extra[len(extra)-1] = 1
	extraPath := "second-room.wav"
	if err := os.WriteFile(filepath.Join(request.RootDirectory, extraPath), extra, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media = append(request.Media, review.Media{
		Kind: "audio", Role: "second_room", Path: extraPath,
		MediaType: "audio/wav", SHA256: digest(extra),
	})
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if _, err := plugin.Review(t.Context(), prepared); err == nil || calls.Load() != 0 {
		t.Fatalf("four-part Review() = %v; calls=%d", err, calls.Load())
	}
}

func TestEncodedInlineEnvelopeJustUnderAtAndOver(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	prepared = clonePrepared(prepared)
	otherMedia := len(prepared.Media[1].Bytes) + len(prepared.Media[2].Bytes)
	prepared.Media[0].Bytes = make([]byte, maximumInlineMediaBytes-otherMedia)

	// This test isolates the final encoder after validated admission. A
	// one-byte ASCII prompt makes the JSON field overhead measurable; from
	// there each additional byte grows the encoded request by exactly one.
	prepared.Prompt = "x"
	oneByteBody, err := marshalValidatedRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	fixedBytes := len(oneByteBody) - 1
	atPromptBytes := maximumInlineRequestBytes - fixedBytes
	if atPromptBytes <= 1 || atPromptBytes > maximumPromptBytes {
		t.Fatalf("encoded envelope leaves invalid prompt budget %d", atPromptBytes)
	}
	for _, test := range []struct {
		name  string
		delta int
	}{
		{name: "just_under", delta: -1},
		{name: "at", delta: 0},
		{name: "over", delta: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := clonePrepared(prepared)
			candidate.Prompt = strings.Repeat("x", atPromptBytes+test.delta)
			body, err := marshalValidatedRequest(candidate)
			if test.delta > 0 {
				if err == nil || body != nil || !strings.Contains(err.Error(), "maximum is 20000000") {
					t.Fatalf("over-limit encoded request = %d bytes, %v", len(body), err)
				}
				return
			}
			if err != nil || len(body) != maximumInlineRequestBytes+test.delta {
				t.Fatalf("encoded request = %d bytes, %v; want %d",
					len(body), err, maximumInlineRequestBytes+test.delta)
			}
		})
	}
}

func TestVerifyResponseRejectsEveryRetainedExchangeMutation(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.VerifyResponse(context.Background(), prepared, response); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*review.ProviderResponse)
	}{
		{"request", func(value *review.ProviderResponse) {
			value.Request = bytes.Replace(value.Request, []byte(ModelID), []byte("gemini-3.7-flasz"), 1)
		}},
		{"raw", func(value *review.ProviderResponse) {
			value.Raw = bytes.Replace(value.Raw, []byte("reviewed evidence"), []byte("reviewed evidencf"), 1)
		}},
		{"output", func(value *review.ProviderResponse) {
			value.Output = bytes.Replace(value.Output, []byte("audiovisual"), []byte("audiovisuam"), 1)
		}},
		{"model", func(value *review.ProviderResponse) { value.ReportedModel = "gemini-drift" }},
		{"request ID", func(value *review.ProviderResponse) { value.RequestID = "different" }},
		{"request ID state", func(value *review.ProviderResponse) {
			value.RequestID = ""
			value.RequestIDState = review.ProviderRequestIDNull
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fresh, reviewErr := plugin.Review(t.Context(), prepared)
			if reviewErr != nil {
				t.Fatal(reviewErr)
			}
			candidate := fresh
			candidate.Request = slices.Clone(fresh.Request)
			candidate.Raw = slices.Clone(fresh.Raw)
			candidate.Output = slices.Clone(fresh.Output)
			test.mutate(&candidate)
			if err := plugin.VerifyResponse(context.Background(), prepared, candidate); err == nil {
				t.Fatal("mutated exchange passed verification")
			}
		})
	}
}

func TestVerifyResponsePrivateEvidenceIsExactOwnerBoundAndOneUse(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	responseBody := successfulInteraction(t, testAssessment)
	newReviewer := func(t *testing.T) *Plugin {
		t.Helper()
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, responseBody), nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = plugin.Close() })
		return plugin
	}
	owner := newReviewer(t)
	foreign := newReviewer(t)
	response, err := owner.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	withoutEvidence := response
	withoutEvidence.VerificationEvidence = nil
	if err := owner.VerifyResponse(context.Background(), prepared, withoutEvidence); err == nil ||
		!strings.Contains(err.Error(), "private verification evidence") {
		t.Fatalf("missing-evidence verification error = %v", err)
	}
	if err := foreign.VerifyResponse(context.Background(), prepared, response); err == nil ||
		!strings.Contains(err.Error(), "private verification evidence") {
		t.Fatalf("foreign-owner verification error = %v", err)
	}
	if err := owner.VerifyResponse(context.Background(), prepared, response); err != nil {
		t.Fatalf("owner verification failed: %v", err)
	}
	if err := owner.VerifyResponse(context.Background(), prepared, response); err == nil ||
		!strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("exact replay verification error = %v", err)
	}
}

func TestVerifyResponseConcurrentReplayHasExactlyOneWinner(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 32
	start := make(chan struct{})
	var group sync.WaitGroup
	var successes atomic.Int32
	var failures atomic.Int32
	for index := 0; index < contenders; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if plugin.VerifyResponse(context.Background(), prepared, response) == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if successes.Load() != 1 || failures.Load() != contenders-1 {
		t.Fatalf("concurrent verification successes=%d failures=%d",
			successes.Load(), failures.Load())
	}
}

func TestVerifyResponseCancellationDoesNotConsumeEvidence(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := plugin.VerifyResponse(canceled, prepared, response); !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyResponse() canceled error = %v", err)
	}
	if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
		t.Fatalf("VerifyResponse() after cancellation = %v", err)
	}
}

type countdownCancelContext struct {
	parent  context.Context
	trigger int64
	calls   atomic.Int64
	stopped atomic.Bool
}

func (ctx *countdownCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *countdownCancelContext) Done() <-chan struct{}       { return nil }
func (ctx *countdownCancelContext) Value(key any) any           { return ctx.parent.Value(key) }
func (ctx *countdownCancelContext) Err() error {
	if ctx.stopped.Load() {
		return context.Canceled
	}
	if ctx.calls.Add(1) >= ctx.trigger {
		ctx.stopped.Store(true)
		return context.Canceled
	}
	return nil
}

func TestVerifyResponseMidWorkCancellationDoesNotConsumeEvidence(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	largeThought := bytes.Repeat([]byte{'a'}, 1<<20)
	responseBody := bytes.Replace(
		successfulInteraction(t, testAssessment), []byte("reviewed evidence"), largeThought, 1,
	)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, responseBody), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	cancelDuringWork := &countdownCancelContext{parent: t.Context(), trigger: 40}
	if err := plugin.VerifyResponse(cancelDuringWork, prepared, response); !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyResponse() mid-work error = %v; calls=%d", err, cancelDuringWork.calls.Load())
	}
	if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
		t.Fatalf("VerifyResponse() retry after mid-work cancellation = %v", err)
	}
}

func TestVerifyResponseAndCloseAreLinearizable(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		_, prepared, _ := preparedMultimodalRequest(t)
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
			func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		response, err := plugin.Review(t.Context(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		verifyResult := make(chan error, 1)
		closeResult := make(chan error, 1)
		go func() {
			<-start
			verifyResult <- plugin.VerifyResponse(context.Background(), prepared, response)
		}()
		go func() {
			<-start
			closeResult <- plugin.Close()
		}()
		close(start)
		verifyErr, closeErr := <-verifyResult, <-closeResult
		if closeErr != nil {
			t.Fatalf("attempt %d Close() = %v", attempt, closeErr)
		}
		if verifyErr != nil && !strings.Contains(verifyErr.Error(), "closed") {
			t.Fatalf("attempt %d VerifyResponse() = %v", attempt, verifyErr)
		}
		if err := plugin.Close(); err != nil {
			t.Fatalf("attempt %d idempotent Close() = %v", attempt, err)
		}
	}
}

func TestVerifyResponsePreflightsRetainedExchangeBounds(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	response, err := plugin.Review(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*review.ProviderResponse)
	}{
		{"request bytes", func(value *review.ProviderResponse) {
			value.Request = make([]byte, maximumInlineRequestBytes+1)
		}},
		{"raw bytes", func(value *review.ProviderResponse) {
			value.Raw = make([]byte, maximumResponseBytes+1)
		}},
		{"output bytes", func(value *review.ProviderResponse) {
			value.Output = make([]byte, maximumResponseBytes+1)
		}},
		{"model", func(value *review.ProviderResponse) {
			value.ReportedModel = strings.Repeat("m", 257)
		}},
		{"request ID", func(value *review.ProviderResponse) {
			value.RequestID = strings.Repeat("r", 4097)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := response
			test.mutate(&candidate)
			if err := plugin.VerifyResponse(context.Background(), prepared, candidate); err == nil {
				t.Fatal("oversized retained exchange passed preflight")
			}
		})
	}
	if err := plugin.VerifyResponse(context.Background(), prepared, response); err != nil {
		t.Fatalf("failed preflights consumed valid private evidence: %v", err)
	}
}

func TestRequestFingerprintBindingRejectsCrossAttemptReplay(t *testing.T) {
	requestA, preparedA, _ := preparedMultimodalRequest(t)
	requestB := requestA
	requestB.AttemptID = "scenario/interrupt/retry-2"
	preparedB, err := review.Prepare(requestB)
	if err != nil {
		t.Fatal(err)
	}
	if preparedA.RequestFingerprint == preparedB.RequestFingerprint {
		t.Fatal("distinct attempt IDs produced the same prepared-request fingerprint")
	}
	bodyA, err := marshalRequest(preparedA)
	if err != nil {
		t.Fatal(err)
	}
	bodyB, err := marshalRequest(preparedB)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(bodyA, bodyB) ||
		!bytes.Contains(bodyA, []byte(requestFingerprintLabel+preparedA.RequestFingerprint)) ||
		!bytes.Contains(bodyB, []byte(requestFingerprintLabel+preparedB.RequestFingerprint)) {
		t.Fatal("Gemini wire request is not bound to the exact prepared-request fingerprint")
	}
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, successfulInteraction(t, testAssessment)), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	responseA, err := plugin.Review(t.Context(), preparedA)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.VerifyResponse(context.Background(), preparedB, responseA); err == nil ||
		!strings.Contains(err.Error(), "retained request differs") {
		t.Fatalf("cross-attempt response replay verification error = %v", err)
	}
}

func TestProductionConstructorUsesPinnedArtifactContract(t *testing.T) {
	plugin, err := New(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if plugin.Descriptor() != Descriptor() ||
		!bytes.Equal(plugin.Implementation(), implementationArtifact()) ||
		!bytes.Equal(plugin.Configuration(), productionConfigurationArtifact()) {
		t.Fatal("production constructor drifted from the registered artifact contract")
	}
	if plugin.httpClient == nil || plugin.httpClient.Timeout != defaultRequestTimeout ||
		plugin.httpClient.Jar != nil || plugin.httpClient.CheckRedirect == nil ||
		plugin.httpClient.Transport == nil {
		t.Fatal("production HTTP policy is not pinned")
	}
}

func TestProductionTransportIsPrivateAndIgnoresMutableHTTPDefault(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var globalCalls atomic.Int32
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		globalCalls.Add(1)
		return nil, errors.New("mutable global transport was invoked")
	})
	client := snapshotHTTPClient(nil)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil || transport.Proxy != nil || !transport.DisableCompression ||
		transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion == 0 {
		t.Fatalf("private production transport = %#v", client.Transport)
	}
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		globalCalls.Add(1)
		return nil, errors.New("replacement global transport was invoked")
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Transport.RoundTrip(request)
	if globalCalls.Load() != 0 {
		t.Fatalf("mutable http.DefaultTransport observed %d private requests", globalCalls.Load())
	}
}

func TestProductionArtifactsContainInspectableSourceAndExactPolicyPreimages(t *testing.T) {
	implementation := implementationArtifact()
	if len(implementation) < 10_000 ||
		!bytes.Contains(implementation, []byte("func (plugin *Plugin) Review")) ||
		!bytes.Contains(implementation, []byte("func marshalRequest")) {
		t.Fatal("Gemini implementation artifact is not the embedded source preimage")
	}
	configuration := productionConfigurationArtifact()
	var decoded map[string]any
	if err := json.Unmarshal(configuration, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["system_instruction"] != systemInstruction {
		t.Fatal("configuration does not retain the exact system instruction")
	}
	if decoded["request_binding"] != "prepared_request_fingerprint_text_block_v1" ||
		decoded["request_fingerprint_label"] != requestFingerprintLabel ||
		decoded["media_order"] != "prompt_then_request_fingerprint_then_manifest_media" ||
		decoded["thinking_level"] != "high" ||
		decoded["max_output_tokens"] != float64(maximumOutputTokens) {
		t.Fatalf("configuration request binding = %#v / %#v / %#v thinking=%#v max=%#v",
			decoded["request_binding"], decoded["request_fingerprint_label"], decoded["media_order"],
			decoded["thinking_level"], decoded["max_output_tokens"])
	}
	headers, ok := decoded["headers"].(map[string]any)
	if !ok || headers["credential_header"] != "x-goog-api-key" ||
		headers["user_agent"] != "OpenRealtime-benchmark-review/1" {
		t.Fatalf("configuration header policy = %#v", decoded["headers"])
	}
	transport, ok := decoded["transport"].(map[string]any)
	if !ok || transport["policy"] != "private_direct_transport_v1" ||
		transport["proxy"] != "disabled" || transport["tls_minimum"] != "1.2" ||
		transport["compression"] != false {
		t.Fatalf("configuration transport policy = %#v", decoded["transport"])
	}
	descriptor := Descriptor()
	if decoded["response_schema_policy"] != "omit_redundant_finding_timestamps_v1" {
		t.Fatalf("configuration response schema policy = %#v", decoded["response_schema_policy"])
	}
	if descriptor.Implementation.Version != "openrealtime.gemini-review.impl.v11" ||
		descriptor.Implementation.SHA256 != digest(implementation) ||
		descriptor.ConfigurationSHA256 != digest(configuration) {
		t.Fatalf("production descriptor = %+v", descriptor)
	}
	if bytes.Contains(configuration, []byte(testAPIKey)) {
		t.Fatal("configuration artifact contains credential material")
	}
}

func TestGeminiProviderCanBeClaimedOnlyOnce(t *testing.T) {
	plugin, err := New(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Claim(); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Claim(); err == nil {
		t.Fatal("Gemini provider accepted a second ownership claim")
	}
}

func TestClaimAndCloseAreRaceSafeAndLinearizable(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		plugin, err := New(testAPIKey)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var group sync.WaitGroup
		var claims atomic.Int32
		for contender := 0; contender < 8; contender++ {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				if plugin.Claim() == nil {
					claims.Add(1)
				}
			}()
		}
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_ = plugin.Close()
		}()
		close(start)
		group.Wait()
		if claims.Load() > 1 {
			t.Fatalf("attempt %d accepted %d ownership claims", attempt, claims.Load())
		}
		if err := plugin.Claim(); err == nil {
			t.Fatalf("attempt %d accepted a post-close ownership claim", attempt)
		}
		if err := plugin.Close(); err != nil {
			t.Fatalf("attempt %d idempotent Close() error = %v", attempt, err)
		}
	}
}

func TestCloseWaitsForActiveReviewAndPermanentlyClosesPlugin(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	responseBody := successfulInteraction(t, testAssessment)
	entered := make(chan struct{})
	release := make(chan struct{})
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			close(entered)
			<-release
			return jsonResponse(http.StatusOK, responseBody), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	reviewDone := make(chan error, 1)
	go func() {
		_, reviewErr := plugin.Review(t.Context(), prepared)
		reviewDone <- reviewErr
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- plugin.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before the active review released ownership: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-reviewDone; err != nil {
		t.Fatalf("active Review() failed during orderly close: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := plugin.Review(t.Context(), prepared); err == nil ||
		!strings.Contains(err.Error(), "closed") {
		t.Fatalf("post-close Review() error = %v", err)
	}
	if err := plugin.Claim(); err == nil {
		t.Fatal("closed plugin accepted a new ownership claim")
	}
	if err := plugin.Close(); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
}

func TestCanceledReviewDoesNotWaitBehindActiveReviewAndClose(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	responseBody := successfulInteraction(t, testAssessment)
	entered := make(chan struct{})
	release := make(chan struct{})
	plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			close(entered)
			<-release
			return jsonResponse(http.StatusOK, responseBody), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, reviewErr := plugin.Review(t.Context(), prepared)
		firstDone <- reviewErr
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- plugin.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		plugin.mu.Lock()
		closing := plugin.closing
		plugin.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close() did not enter its active-call drain")
		}
		time.Sleep(time.Millisecond)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, reviewErr := plugin.Review(canceled, prepared)
		secondDone <- reviewErr
	}()
	select {
	case reviewErr := <-secondDone:
		if !errors.Is(reviewErr, context.Canceled) {
			t.Fatalf("canceled Review() error = %v", reviewErr)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("canceled Review() waited behind active Review() and Close()")
	}
	close(release)
	if reviewErr := <-firstDone; reviewErr != nil {
		t.Fatalf("active Review() error = %v", reviewErr)
	}
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
}

func TestPreparedCredentialScanCoversMediaBytes(t *testing.T) {
	request, _, _ := preparedMultimodalRequest(t)
	path := filepath.Join(request.RootDirectory, request.Media[0].Path)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte(testAPIKey)...)
	if (len(payload)-44)%2 != 0 {
		payload = append(payload, 0)
	}
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(payload)
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if !preparedContainsCredential(prepared, testAPIKey) {
		t.Fatal("credential in prepared media bytes was not detected")
	}
}
