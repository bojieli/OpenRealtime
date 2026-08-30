package review

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreparedRequestValidateRejectsEveryBoundInputMutation(t *testing.T) {
	request, _ := testRequest(t)
	prepared, err := Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*PreparedRequest)
	}{
		{"attempt ID", func(value *PreparedRequest) { value.AttemptID += "-other" }},
		{"suite", func(value *PreparedRequest) { value.Suite += "-other" }},
		{"case", func(value *PreparedRequest) { value.Case += "-other" }},
		{"trial", func(value *PreparedRequest) { value.Trial++ }},
		{"finding timestamp maximum", func(value *PreparedRequest) {
			value.FindingTimestampMaximumMS--
		}},
		{"prompt version", func(value *PreparedRequest) { value.PromptVersion += "-other" }},
		{"prompt", func(value *PreparedRequest) { value.Prompt += "\nforged" }},
		{"schema version", func(value *PreparedRequest) { value.SchemaVersion += "-other" }},
		{"schema", func(value *PreparedRequest) {
			value.Schema = bytes.Replace(value.Schema, []byte(`"boolean"`), []byte(`"string"`), 1)
		}},
		{"context", func(value *PreparedRequest) { value.Context = json.RawMessage(`{"a":9,"z":2}`) }},
		{"media role", func(value *PreparedRequest) { value.Media[0].Role += "_other" }},
		{"media kind", func(value *PreparedRequest) { value.Media[0].Kind = "video" }},
		{"media path", func(value *PreparedRequest) { value.Media[0].Path = "media/other.wav" }},
		{"media type", func(value *PreparedRequest) { value.Media[0].MediaType = "audio/mpeg" }},
		{"media validation", func(value *PreparedRequest) { value.Media[0].Validation += "-other" }},
		{"media size", func(value *PreparedRequest) { value.Media[0].SizeBytes++ }},
		{"media digest", func(value *PreparedRequest) { value.Media[0].SHA256 = digest([]byte("other")) }},
		{"media bytes", func(value *PreparedRequest) { value.Media[0].Bytes[0] ^= 1 }},
		{"fingerprint", func(value *PreparedRequest) { value.RequestFingerprint = digest([]byte("forged")) }},
		{"sanitization", func(value *PreparedRequest) { value.Sanitization += "-other" }},
		{"sensitive count", func(value *PreparedRequest) { value.SensitiveValueCount++ }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := clonePreparedRequest(prepared)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("mutated PreparedRequest passed validation")
			}
		})
	}
}

func TestPrepareRejectsDeclaredSensitiveValuesInContextAndMedia(t *testing.T) {
	const secret = "sensitive-value-123456"
	for _, location := range []string{"context literal", "context escaped", "media"} {
		t.Run(location, func(t *testing.T) {
			request, payload := testRequest(t)
			request.SensitiveValues = []string{secret, secret}
			switch location {
			case "context literal":
				request.Context = json.RawMessage(`{"value":"` + secret + `"}`)
			case "context escaped":
				escaped := strings.ReplaceAll(secret, "-", `\u002d`)
				request.Context = json.RawMessage(`{"value":"` + escaped + `"}`)
			case "media":
				payload = append(payload, []byte(secret)...)
				if (len(payload)-44)%2 != 0 {
					payload = append(payload, 0)
				}
				binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
				binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
				path := filepath.Join(request.RootDirectory, request.Media[0].Path)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatal(err)
				}
				request.Media[0].SHA256 = digest(payload)
			}
			if _, err := Prepare(request); err == nil || !strings.Contains(err.Error(), "sensitive value") {
				t.Fatalf("Prepare() error = %v", err)
			}
		})
	}
}

func TestPrepareRejectsDeclaredSecretSplitAcrossDecodedJSONTokens(t *testing.T) {
	const secret = "abcdefghijklmnop"
	contexts := map[string]json.RawMessage{
		"values":    json.RawMessage(`{"part1":"abcdefgh","part2":"ijklmnop"}`),
		"keys":      json.RawMessage(`{"abcdefgh":1,"ijklmnop":2}`),
		"key value": json.RawMessage(`{"abcdefgh":"ijklmnop"}`),
		"prefixed nested escape": json.RawMessage(
			`{"outer":"prefix {\"k\":\"abcdefgh\\u0069jklmnop\"} suffix"}`),
		"prefixed nested split": json.RawMessage(
			`{"outer":"prefix {\"a\":\"abcdefgh\",\"b\":\"ijklmnop\"} suffix"}`),
		"encoded then plain": json.RawMessage(
			`{"a":"\\u0061\\u0062\\u0063\\u0064\\u0065\\u0066\\u0067\\u0068","b":"ijklmnop"}`),
		"plain then encoded": json.RawMessage(
			`{"a":"abcdefgh","b":"\\u0069\\u006a\\u006b\\u006c\\u006d\\u006e\\u006f\\u0070"}`),
		"double encoded then plain": json.RawMessage(
			`{"a":"\\\\u0061\\\\u0062\\\\u0063\\\\u0064\\\\u0065\\\\u0066\\\\u0067\\\\u0068","b":"ijklmnop"}`),
		"heterogeneous escaped depths": json.RawMessage(
			`{"a":"\\u0061\\u0062\\u0063\\u0064\\u0065\\u0066\\u0067\\u0068","b":"\\\\u0069\\\\u006a\\\\u006b\\\\u006c\\\\u006d\\\\u006e\\\\u006f\\\\u0070"}`),
	}
	for name, contextJSON := range contexts {
		t.Run(name, func(t *testing.T) {
			request, _ := testRequest(t)
			request.Context = contextJSON
			request.SensitiveValues = []string{secret}
			if _, err := PrepareContext(t.Context(), request); err == nil ||
				strings.Contains(err.Error(), secret) {
				t.Fatalf("PrepareContext() error = %v; want redacted rejection", err)
			}
		})
	}
	t.Run("string numeric value", func(t *testing.T) {
		const numericSecret = "abcdefgh12345678"
		request, _ := testRequest(t)
		request.Context = json.RawMessage(`{"a":"abcdefgh","b":12345678}`)
		request.SensitiveValues = []string{numericSecret}
		if _, err := PrepareContext(t.Context(), request); err == nil ||
			strings.Contains(err.Error(), numericSecret) {
			t.Fatalf("PrepareContext() error = %v; want redacted rejection", err)
		}
	})
}

func TestPrepareScansLargeNestedPromptEnvelopeExactlyOnce(t *testing.T) {
	request, _ := testRequest(t)
	values := make([]string, 2000)
	for index := range values {
		values[index] = fmt.Sprintf(
			`{"frame":%d,"text":"ordinary retained trace %d"}`, index, index,
		)
	}
	payload, err := json.Marshal(map[string]any{"trace": values})
	if err != nil {
		t.Fatal(err)
	}
	request.Context = payload
	request.SensitiveValues = []string{strings.Repeat("Z", 64)}
	prepared, err := PrepareContext(t.Context(), request)
	if err != nil {
		t.Fatalf("benign nested prompt envelope produced a bounded-scan false positive: %v", err)
	}
	if !bytes.Contains([]byte(prepared.Prompt), payload) ||
		!prepared.ContainsDeclaredSensitiveValue([]byte(`{"value":"`+strings.Repeat("Z", 64)+`"}`)) {
		t.Fatal("prepared prompt or declared-sensitive guard lost its exact evidence contract")
	}

	literalRequest, _ := testRequest(t)
	literalRequest.SensitiveValues = []string{"offline quality reviewer"}
	if _, err := PrepareContext(t.Context(), literalRequest); err == nil ||
		!strings.Contains(err.Error(), "prompt envelope contains a declared sensitive value") {
		t.Fatalf("literal prompt secret error = %v", err)
	}
}

func TestSecretScannerDoesNotReplayOneNestedRepeatedHalf(t *testing.T) {
	const secret = "abcdefghabcdefgh"
	matcher, err := newSensitiveMatcher([][]byte{[]byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"outer":"{\"\":\"abcdefgh\"}"}`)
	if secretInJSON(payload, matcher) {
		t.Fatal("one nested repeated half was counted as two independent secret fragments")
	}
}

func TestPreparedAndMediaValidationHonorCancellation(t *testing.T) {
	request, payload := testRequest(t)
	prepared, err := Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := prepared.ValidateContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateContext() error = %v, want cancellation", err)
	}
	if err := validateMediaPayloadContext(canceled, "audio", "audio/wav", payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("validateMediaPayloadContext() error = %v, want cancellation", err)
	}
}

func TestEvaluateRejectsDuplicateMediaContentBeforeProviderSideEffects(t *testing.T) {
	request, payload := testRequest(t)
	duplicate := request.Media[0]
	duplicate.Path = "media/duplicate.wav"
	duplicate.Role = "duplicate_room_audio"
	if err := os.WriteFile(
		filepath.Join(request.RootDirectory, duplicate.Path), payload, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	request.Media = append(request.Media, duplicate)
	descriptor := testDescriptor("duplicate-media-model")
	provider := &testProvider{descriptor: descriptor}
	lease := openTestLease(t, provider)
	defer lease.Close()
	if _, err := Evaluate(t.Context(), lease, request); err == nil ||
		!strings.Contains(err.Error(), "repeats content digest") {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if provider.reviewCalls.Load() != 0 {
		t.Fatalf("duplicate content reached provider %d times", provider.reviewCalls.Load())
	}
}

func TestEvaluateBoundsUntrustedProviderErrors(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("bounded-error-model")
	huge := strings.Repeat("x", maximumProviderErrorBytes+1)
	for _, phase := range []string{"review", "verify"} {
		t.Run(phase, func(t *testing.T) {
			provider := &testProvider{
				descriptor: descriptor,
				response: ProviderResponse{
					Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
					RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
				},
			}
			if phase == "review" {
				provider.reviewErr = errors.New(huge)
			} else {
				provider.verifyErr = errors.New(huge)
			}
			lease := openTestLease(t, provider)
			defer lease.Close()
			_, err := Evaluate(t.Context(), lease, request)
			if err == nil || err.Error() != "failed" || len(err.Error()) > 32 {
				t.Fatalf("Evaluate() error = %v", err)
			}
		})
	}
	provider := &testProvider{
		descriptor: descriptor,
		reviewErr:  fmt.Errorf("%s: %w", huge, context.Canceled),
	}
	lease := openTestLease(t, provider)
	defer lease.Close()
	if _, err := Evaluate(t.Context(), lease, request); !errors.Is(err, context.Canceled) ||
		err.Error() != "failed" {
		t.Fatalf("cancellation-classified provider error = %v", err)
	}
}

func TestPrepareBoundsDeclaredSensitiveValueCardinality(t *testing.T) {
	request, _ := testRequest(t)
	request.SensitiveValues = make([]string, maximumSensitiveValues+1)
	for index := range request.SensitiveValues {
		request.SensitiveValues[index] = fmt.Sprintf("sensitive-value-%06d", index)
	}
	if _, err := Prepare(request); err == nil || !strings.Contains(err.Error(), "value limit") {
		t.Fatalf("Prepare() sensitive-value cardinality error = %v", err)
	}
}

func TestPrepareInstallsSecretGuardBeforeRequestPreflightDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		secret string
		mutate func(*Request, string)
	}{
		{
			name: "invalid kind", secret: "secret-kind-12345",
			mutate: func(request *Request, secret string) { request.Media[0].Kind = secret },
		},
		{
			name: "unsupported media type", secret: "audio/secret-preflight",
			mutate: func(request *Request, secret string) { request.Media[0].MediaType = secret },
		},
		{
			name: "invalid path", secret: "../secret-path-12345.wav",
			mutate: func(request *Request, secret string) { request.Media[0].Path = secret },
		},
		{
			name: "duplicate path", secret: "media/secret-duplicate-12345.wav",
			mutate: func(request *Request, secret string) {
				request.Media[0].Path = secret
				request.Media = append(request.Media, request.Media[0])
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := testRequest(t)
			test.mutate(&request, test.secret)
			request.SensitiveValues = []string{test.secret}
			_, err := Prepare(request)
			if err == nil || strings.Contains(err.Error(), test.secret) {
				t.Fatalf("Prepare() preflight error = %v; want redacted failure", err)
			}
		})
	}
}

func TestPrepareRejectsDeclaredSensitiveValuesInEveryRetainedMetadataField(t *testing.T) {
	for _, test := range []struct {
		name   string
		secret func(Request) string
		mutate func(*Request, string)
	}{
		{
			name: "attempt ID", secret: func(Request) string { return "secret-attempt" },
			mutate: func(request *Request, secret string) { request.AttemptID = "suite/" + secret + "/1" },
		},
		{
			name: "suite", secret: func(Request) string { return "secret-suite" },
			mutate: func(request *Request, secret string) { request.Suite = "suite-" + secret },
		},
		{
			name: "case", secret: func(Request) string { return "secret-case" },
			mutate: func(request *Request, secret string) { request.Case = "case-" + secret },
		},
		{
			name: "media role", secret: func(Request) string { return "secret-role" },
			mutate: func(request *Request, secret string) { request.Media[0].Role = "room_" + secret },
		},
		{
			name: "media path", secret: func(Request) string { return "secret-path" },
			mutate: func(request *Request, secret string) { request.Media[0].Path = "media/" + secret + ".wav" },
		},
		{
			name: "media type", secret: func(Request) string { return "audio/wav" },
			mutate: func(*Request, string) {},
		},
		{
			name: "media digest", secret: func(request Request) string { return request.Media[0].SHA256[7:19] },
			mutate: func(*Request, string) {},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := testRequest(t)
			secret := test.secret(request)
			test.mutate(&request, secret)
			request.SensitiveValues = []string{secret}
			if _, err := Prepare(request); err == nil || !strings.Contains(err.Error(), "sensitive value") {
				t.Fatalf("Prepare() error = %v", err)
			}
		})
	}
}

func TestEvaluateRejectsDeclaredSecretSynthesizedInProviderExchange(t *testing.T) {
	const declaredSecret = "QUJDREVGR0hJSktM"
	request, _ := testRequest(t)
	payload := slices.Clone(testWAVPayload()[:44])
	payload = append(payload, 0)
	payload = append(payload, []byte("ABCDEFGHIJKL")...)
	payload = append(payload, 0)
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
	if err := os.WriteFile(filepath.Join(request.RootDirectory, request.Media[0].Path), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(payload)
	request.SensitiveValues = []string{declaredSecret}
	descriptor := testDescriptor("declared-secret-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing,
			Request:        []byte(`{"encoded":"` + declaredSecret + `"}`),
		},
	}
	_, err := Evaluate(t.Context(), openTestLease(t, provider), request)
	if err == nil || !strings.Contains(err.Error(), "declared sensitive value") ||
		strings.Contains(err.Error(), declaredSecret) {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if provider.reviewCalls.Load() != 1 || provider.verifyCalls.Load() != 0 {
		t.Fatalf("provider calls review=%d verify=%d", provider.reviewCalls.Load(), provider.verifyCalls.Load())
	}
}

func TestEvaluatePostScanTreatsRawMediaAsLiteralBytes(t *testing.T) {
	const secret = "resume-sensitive-value-0123456789abcdefghijklmnop"
	escaped := strings.ReplaceAll(secret, "-", `\u002d`)
	if strings.Contains(escaped, secret) {
		t.Fatal("escaped media fixture contains the literal secret")
	}
	request, _ := testRequest(t)
	payload := append(slices.Clone(testWAVPayload()), []byte(escaped)...)
	if (len(payload)-44)%2 != 0 {
		payload = append(payload, 0)
	}
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
	if err := os.WriteFile(filepath.Join(request.RootDirectory, request.Media[0].Path), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(payload)
	request.SensitiveValues = []string{secret}
	descriptor := testDescriptor("literal-media-post-scan-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
		},
	}
	evaluation, err := Evaluate(t.Context(), openTestLease(t, provider), request)
	if err != nil {
		t.Fatalf("escaped raw media produced a contextual secret false positive: %v", err)
	}
	if len(evaluation.Media) != 1 || !bytes.Equal(evaluation.Media[0].Bytes, payload) ||
		provider.reviewCalls.Load() != 1 || provider.verifyCalls.Load() != 1 {
		t.Fatalf("literal media post-scan evaluation = %+v; calls review=%d verify=%d",
			evaluation.Record, provider.reviewCalls.Load(), provider.verifyCalls.Load())
	}
}

func TestPreparedSensitiveGuardIsOpaqueOwnedAndJSONAware(t *testing.T) {
	const secret = "sensitive-value-123456"
	request, _ := testRequest(t)
	request.SensitiveValues = []string{secret}
	prepared, err := Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(serialized, []byte(secret)) || prepared.sensitiveGuard == nil ||
		prepared.sensitiveGuard.count != 1 ||
		!prepared.ContainsDeclaredSensitiveValue([]byte(`{"escaped":"sensitive\u002dvalue\u002d123456"}`)) ||
		!prepared.ContainsDeclaredSensitiveValue([]byte(
			`provider failed near {"escaped":"sensitive\u002dvalue\u002d123456"} and stopped`)) ||
		!prepared.ContainsDeclaredSensitiveValue([]byte(
			`provider failed: sensitive\u002dvalue\u002d123456`)) ||
		!prepared.ContainsDeclaredSensitiveValue([]byte(
			`{"nested":"{\"escaped\":\"sensitive\\u002dvalue\\u002d123456\"}"}`)) {
		t.Fatal("prepared sensitive guard was serialized, absent, or unable to inspect JSON escapes")
	}
	if prepared.ContainsDeclaredSensitiveValue([]byte(
		`{"nested":"{\"value\":\"ordinary retained text\"}"}`)) {
		t.Fatal("bounded recursive scanning rejected ordinary nested JSON without a declared secret")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%q"} {
		formatted := fmt.Sprintf(format, prepared)
		if strings.Contains(formatted, secret) {
			t.Fatalf("format %q exposed a declared secret: %s", format, formatted)
		}
	}
	guardField := reflect.ValueOf(prepared).FieldByName("sensitiveGuard")
	if !guardField.IsValid() || strings.Contains(fmt.Sprint(guardField), secret) {
		t.Fatal("reflection-oriented formatting exposed or lost the opaque guard")
	}
	cloned := clonePreparedRequest(prepared)
	request.SensitiveValues[0] = "overwritten-after-prepare"
	if !cloned.ContainsDeclaredSensitiveValue([]byte(secret)) ||
		cloned.ContainsDeclaredSensitiveValue([]byte(request.SensitiveValues[0])) {
		t.Fatal("prepared sensitive guard did not own its immutable source")
	}
	var group sync.WaitGroup
	failures := make(chan struct{}, 32)
	for index := 0; index < cap(failures); index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if !cloned.ContainsDeclaredSensitiveValue([]byte(secret)) {
				failures <- struct{}{}
			}
		}()
	}
	group.Wait()
	close(failures)
	if len(failures) != 0 {
		t.Fatal("opaque sensitive guard was not concurrency-safe")
	}
}

func TestPreparedValidateRedactsDeclaredSecretFromMutationDiagnostics(t *testing.T) {
	const secret = "secret-mutated-kind-12345"
	request, _ := testRequest(t)
	request.SensitiveValues = []string{secret}
	prepared, err := Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Media = slices.Clone(prepared.Media)
	prepared.Media[0].Kind = secret
	err = prepared.Validate()
	if err == nil || strings.Contains(err.Error(), secret) || err.Error() != "failed" {
		t.Fatalf("PreparedRequest.Validate() mutation error = %v; want redacted failure", err)
	}
}

func TestPrepareRejectsSecretSynthesizedInDerivedFingerprint(t *testing.T) {
	request, _ := testRequest(t)
	request.SensitiveValues = []string{"placeholder-sensitive-value"}
	first, err := Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	secret := first.RequestFingerprint[7:31]
	request.SensitiveValues = []string{secret}
	if _, err := Prepare(request); err == nil || !strings.Contains(err.Error(), "sensitive value") ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("Prepare() fingerprint synthesis error = %v", err)
	}
}

func TestPrepareGuardsSecretsBeforeContextAndFilesystemErrors(t *testing.T) {
	const secret = "secret-root-or-json-key"
	for _, location := range []string{"duplicate context key", "root directory"} {
		t.Run(location, func(t *testing.T) {
			request, _ := testRequest(t)
			request.SensitiveValues = []string{secret}
			switch location {
			case "duplicate context key":
				request.Context = json.RawMessage(`{"\u0073ecret-root-or-json-key":1,"secre\u0074-root-or-json-key":2}`)
			case "root directory":
				request.RootDirectory = filepath.Join(t.TempDir(), secret, "missing")
			}
			_, err := Prepare(request)
			if err == nil || !strings.Contains(err.Error(), "sensitive value") ||
				strings.Contains(err.Error(), secret) {
				t.Fatalf("Prepare() %s error = %v", location, err)
			}
		})
	}
}

func TestEvaluateRedactsDeclaredSecretsFromProviderControlledErrors(t *testing.T) {
	const secret = "provider-controlled-sensitive-value"
	for _, mode := range []string{
		"review error", "canceled review error", "verify error", "canceled verify error", "reported model",
	} {
		t.Run(mode, func(t *testing.T) {
			request, _ := testRequest(t)
			request.SensitiveValues = []string{secret}
			descriptor := testDescriptor("redaction-model")
			provider := &testProvider{
				descriptor: descriptor,
				response: ProviderResponse{
					Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
					RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
				},
			}
			ctx := t.Context()
			if mode == "review error" || mode == "canceled review error" {
				provider.reviewErr = errors.New(secret)
			}
			if mode == "verify error" || mode == "canceled verify error" {
				provider.verifyErr = errors.New(secret)
			}
			if mode == "reported model" {
				provider.response.ReportedModel = secret
			}
			if mode == "canceled review error" {
				cancelable, cancel := context.WithCancel(t.Context())
				ctx = cancelable
				provider.onReview = func(PreparedRequest) { cancel() }
			}
			if mode == "canceled verify error" {
				cancelable, cancel := context.WithCancel(t.Context())
				ctx = cancelable
				provider.onVerify = cancel
			}
			_, err := Evaluate(ctx, openTestLease(t, provider), request)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("Evaluate() %s error = %v", mode, err)
			}
			if strings.HasPrefix(mode, "canceled ") && !errors.Is(err, context.Canceled) {
				t.Fatalf("Evaluate() cancellation error = %v", err)
			}
		})
	}

	request, _ := testRequest(t)
	request.SensitiveValues = []string{secret}
	descriptor := testDescriptor("ordinary-error-model")
	provider := &testProvider{descriptor: descriptor, reviewErr: errors.New("ordinary provider failure")}
	if _, err := Evaluate(t.Context(), openTestLease(t, provider), request); err == nil ||
		!strings.Contains(err.Error(), "ordinary provider failure") {
		t.Fatalf("Evaluate() erased non-sensitive provider error = %v", err)
	}

	request, _ = testRequest(t)
	request.SensitiveValues = []string{secret}
	descriptor = testDescriptor("embedded-error-model")
	provider = &testProvider{
		descriptor: descriptor,
		reviewErr: errors.New(
			`provider failed near {"value":"provider\u002dcontrolled\u002dsensitive\u002dvalue"}`),
	}
	if _, err := Evaluate(t.Context(), openTestLease(t, provider), request); err == nil ||
		err.Error() != "failed" || strings.Contains(err.Error(), `\u002d`) {
		t.Fatalf("Evaluate() exposed an embedded JSON-escaped provider secret: %v", err)
	}

	request, _ = testRequest(t)
	request.SensitiveValues = []string{secret}
	descriptor = testDescriptor("unquoted-error-model")
	provider = &testProvider{
		descriptor: descriptor,
		reviewErr: errors.New(
			`provider failed: provider\u002dcontrolled\u002dsensitive\u002dvalue`),
	}
	if _, err := Evaluate(t.Context(), openTestLease(t, provider), request); err == nil ||
		err.Error() != "failed" || strings.Contains(err.Error(), `\u002d`) {
		t.Fatalf("Evaluate() exposed an unquoted JSON-escaped provider secret: %v", err)
	}
}

func TestEvaluateRedactionCoversWrapperAndCanonicalCancellationSynthesis(t *testing.T) {
	t.Run("wrapper boundary", func(t *testing.T) {
		const secret = "invocation: ordinary provider"
		request, _ := testRequest(t)
		request.SensitiveValues = []string{secret}
		descriptor := testDescriptor("wrapper-redaction-model")
		provider := &testProvider{
			descriptor: descriptor, reviewErr: errors.New("ordinary provider failure"),
		}
		_, err := Evaluate(t.Context(), openTestLease(t, provider), request)
		if err == nil || err.Error() != "failed" || strings.Contains(err.Error(), secret) {
			t.Fatalf("Evaluate() wrapper synthesis error = %v", err)
		}
	})
	for _, test := range []struct {
		name   string
		secret string
		ctx    func() (context.Context, context.CancelFunc)
		want   error
	}{
		{"canceled", context.Canceled.Error(), func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"deadline", context.DeadlineExceeded.Error(), func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			return ctx, cancel
		}, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := testRequest(t)
			request.SensitiveValues = []string{test.secret}
			ctx, cancel := test.ctx()
			defer cancel()
			_, err := Evaluate(ctx, nil, request)
			if err == nil || err.Error() != "failed" || !errors.Is(err, test.want) ||
				strings.Contains(err.Error(), test.secret) {
				t.Fatalf("Evaluate() canonical %s synthesis error = %v", test.name, err)
			}
		})
	}
}

type cancelAfterChecksContext struct {
	context.Context
	remaining int
	done      chan struct{}
}

func (ctx *cancelAfterChecksContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelAfterChecksContext) Err() error {
	ctx.remaining--
	if ctx.remaining > 0 {
		return nil
	}
	select {
	case <-ctx.done:
	default:
		close(ctx.done)
	}
	return context.Canceled
}

func TestPrepareCancellationBeforeAndDuringSensitiveMatcherIsBoundedAndRedacted(t *testing.T) {
	t.Run("pre-canceled", func(t *testing.T) {
		request, _ := testRequest(t)
		request.SensitiveValues = []string{context.Canceled.Error()}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := PrepareContext(ctx, request)
		if err == nil || !errors.Is(err, context.Canceled) || err.Error() != "failed" {
			t.Fatalf("PrepareContext() pre-cancellation error = %v", err)
		}
	})
	t.Run("matcher build", func(t *testing.T) {
		request, _ := testRequest(t)
		request.SensitiveValues = []string{strings.Repeat("x", 4096)}
		ctx := &cancelAfterChecksContext{
			Context: t.Context(), remaining: 5, done: make(chan struct{}),
		}
		_, err := PrepareContext(ctx, request)
		if err == nil || !errors.Is(err, context.Canceled) || err.Error() != "failed" {
			t.Fatalf("PrepareContext() matcher cancellation error = %v", err)
		}
	})
}

func TestEvaluateRejectsSecretSynthesizedByJSONNormalization(t *testing.T) {
	const secret = `"summary": "The retained evidence`
	request, _ := testRequest(t)
	request.SensitiveValues = []string{secret}
	descriptor := testDescriptor("retention-synthesis-model")
	escaped := bytes.Replace(validAssessment, []byte(`"summary"`), []byte(`"\u0073ummary"`), 1)
	if bytes.Contains(escaped, []byte(secret)) {
		t.Fatal("provider fixture already contains the declared secret")
	}
	_, normalized, err := normalizeAssessment(escaped)
	if err != nil || !bytes.Contains(normalized, []byte(secret)) {
		t.Fatalf("fixture does not synthesize the declared secret during normalization: %v\n%s", err, normalized)
	}
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: escaped, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
		},
	}
	if _, err := Evaluate(t.Context(), openTestLease(t, provider), request); err == nil ||
		!strings.Contains(err.Error(), "declared sensitive value") || strings.Contains(err.Error(), secret) {
		t.Fatalf("Evaluate() normalized synthesis error = %v", err)
	}
}

func TestPrepareRejectsMediaContainerMismatch(t *testing.T) {
	request, _ := testRequest(t)
	request.Media[0].MediaType = "audio/m4a"
	if _, err := Prepare(request); err == nil || !strings.Contains(err.Error(), "payload does not match") {
		t.Fatalf("Prepare() error = %v", err)
	}
}

func TestPrepareRejectsUnsupportedMediaBeforeFilesystemAccess(t *testing.T) {
	request, _ := testRequest(t)
	request.Media[0].MediaType = "audio/mpeg"
	request.Media[0].Path = "media/does-not-exist.mp3"
	if _, err := Prepare(request); err == nil ||
		!strings.Contains(err.Error(), "no bounded payload validator") ||
		strings.Contains(err.Error(), "no such file") {
		t.Fatalf("Prepare() unsupported-media preflight error = %v", err)
	}
}

func TestMediaValidationRejectsHeaderOnlyAndCrossCodecSpoofs(t *testing.T) {
	ebml := []byte{0x1a, 0x45, 0xdf, 0xa3}
	for _, test := range []struct {
		name, kind, mediaType string
		payload               []byte
	}{
		{"header-only WAV", "audio", "audio/wav", []byte("RIFF\x04\x00\x00\x00WAVE")},
		{"header-only PNG", "image", "image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}},
		{"header-only MP4", "video", "video/mp4", isoBoxFixture("ftyp", []byte("isom\x00\x00\x00\x00"))},
		{"Vorbis relabeled Opus", "audio", "audio/opus", append([]byte("OggS"), []byte("vorbis")...)},
		{"audio WebM relabeled video", "video", "video/webm", append(slices.Clone(ebml), []byte("A_OPUS")...)},
		{"video WebM relabeled audio", "audio", "audio/webm", append(slices.Clone(ebml), []byte("V_VP9")...)},
		{"audio M4A relabeled video", "video", "video/mp4", isoTrackFixture("M4A ", "soun", "mp4a")},
		{"video MP4 relabeled audio", "audio", "audio/m4a", isoTrackFixture("isom", "vide", "avc1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMediaPayload(test.kind, test.mediaType, test.payload); err == nil {
				t.Fatal("spoofed or unusable media passed structural validation")
			}
		})
	}
	if err := validateMediaPayload("audio", "audio/wav", testWAVPayload()); err != nil {
		t.Fatalf("valid PCM WAV rejected: %v", err)
	}
	if err := validateMediaPayload("audio", "audio/m4a", isoTrackFixture("M4A ", "soun", "mp4a")); err != nil {
		t.Fatalf("structural M4A fixture rejected: %v", err)
	}
	if err := validateMediaPayload("video", "video/mp4", isoTrackFixture("isom", "vide", "avc1")); err != nil {
		t.Fatalf("structural MP4 fixture rejected: %v", err)
	}
}

func TestOggValidationParsesPagesChecksCRCAndRequiresCodecPackets(t *testing.T) {
	valid := oggOpusFixture()
	for _, mediaType := range []string{"audio/ogg", "audio/opus"} {
		if err := validateMediaPayload("audio", mediaType, valid); err != nil {
			t.Fatalf("valid Ogg Opus rejected as %s: %v", mediaType, err)
		}
	}
	markerInMetadata := oggPageFixture([][]byte{
		[]byte("untrusted-metadata:OpusHead"), []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"), {0xf8},
	})
	badSequence := slices.Clone(valid)
	binary.LittleEndian.PutUint32(badSequence[18:22], 1)
	binary.LittleEndian.PutUint32(badSequence[22:26], 0)
	binary.LittleEndian.PutUint32(badSequence[22:26], testOggChecksum(badSequence))
	badCRC := slices.Clone(valid)
	badCRC[len(badCRC)-1] ^= 1
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"marker only", []byte("OggS...OpusHead")},
		{"marker in metadata", markerInMetadata},
		{"truncated page", valid[:len(valid)-1]},
		{"bad CRC", badCRC},
		{"first sequence is not zero", badSequence},
		{"missing codec headers and audio", oggPageFixture([][]byte{[]byte("OpusHead\x01\x02")})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMediaPayload("audio", "audio/ogg", test.payload); err == nil {
				t.Fatal("malformed Ogg payload passed structural validation")
			}
		})
	}
}

func TestMediaValidationRejectsPostHeaderImageCorruptionAndInconsistentWAV(t *testing.T) {
	var encoded bytes.Buffer
	fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fixture.Set(0, 0, color.RGBA{R: 0x80, G: 0x40, B: 0x20, A: 0xff})
	if err := png.Encode(&encoded, fixture); err != nil {
		t.Fatal(err)
	}
	corruptPNG := slices.Clone(encoded.Bytes())
	for offset := 8; offset+12 <= len(corruptPNG); {
		length := int(binary.BigEndian.Uint32(corruptPNG[offset : offset+4]))
		end := offset + 12 + length
		if length < 0 || end > len(corruptPNG) {
			t.Fatal("generated PNG has invalid chunk bounds")
		}
		if string(corruptPNG[offset+4:offset+8]) == "IDAT" && length > 0 {
			corruptPNG[offset+8] ^= 0xff
			break
		}
		offset = end
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(corruptPNG)); err != nil || format != "png" {
		t.Fatalf("corrupt PNG no longer isolates full-decode validation: format=%q error=%v", format, err)
	}
	if err := validateMediaPayload("image", "image/png", corruptPNG); err == nil {
		t.Fatal("PNG with valid IHDR but corrupt IDAT passed full decode")
	}

	for _, field := range []string{"byte rate", "block align"} {
		t.Run(field, func(t *testing.T) {
			wav := testWAVPayload()
			if field == "byte rate" {
				binary.LittleEndian.PutUint32(wav[28:32], 47_999)
			} else {
				binary.LittleEndian.PutUint16(wav[32:34], 4)
			}
			if err := validateMediaPayload("audio", "audio/wav", wav); err == nil {
				t.Fatalf("WAV with inconsistent %s passed structural validation", field)
			}
		})
	}
}

func TestGIFValidationChecksEveryFrameAndTrailingBlock(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	first.SetColorIndex(0, 0, 1)
	second.SetColorIndex(1, 1, 1)
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{
		Image: []*image.Paletted{first, second}, Delay: []int{1, 1},
		Config: image.Config{ColorModel: palette, Width: 2, Height: 2},
	}); err != nil {
		t.Fatal(err)
	}
	valid := encoded.Bytes()
	if err := validateMediaPayload("image", "image/gif", valid); err != nil {
		t.Fatalf("valid animated GIF rejected: %v", err)
	}
	corrupt := append(slices.Clone(valid[:len(valid)-1]), 0x7f, 0x3b)
	if _, format, err := image.Decode(bytes.NewReader(corrupt)); err != nil || format != "gif" {
		t.Fatalf("corrupt fixture no longer proves first-frame-only decode: format=%q error=%v", format, err)
	}
	if _, err := gif.DecodeAll(bytes.NewReader(corrupt)); err == nil {
		t.Fatal("corrupt fixture unexpectedly passed gif.DecodeAll")
	}
	if err := validateMediaPayload("image", "image/gif", corrupt); err == nil {
		t.Fatal("GIF with valid frames and an invalid later block passed complete validation")
	}
}

func TestPNGAndJPEGValidationRejectsTrailingContainers(t *testing.T) {
	fixture := image.NewRGBA(image.Rect(0, 0, 3, 2))
	fixture.Set(0, 0, color.RGBA{R: 0x80, G: 0x40, B: 0x20, A: 0xff})

	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, fixture); err != nil {
		t.Fatal(err)
	}
	validPNG := pngBuffer.Bytes()
	if err := validateMediaPayload("image", "image/png", validPNG); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	fakeIEND := make([]byte, 12)
	copy(fakeIEND[4:8], "IEND")
	binary.BigEndian.PutUint32(fakeIEND[8:12], crc32.ChecksumIEEE(fakeIEND[4:8]))
	trailingPNG := append(slices.Clone(validPNG), fakeIEND...)
	if _, format, err := image.Decode(bytes.NewReader(trailingPNG)); err != nil || format != "png" {
		t.Fatalf("trailing PNG fixture no longer demonstrates decoder early-stop: %q %v", format, err)
	}
	if err := validateMediaPayload("image", "image/png", trailingPNG); err == nil {
		t.Fatal("PNG with a second terminal container passed complete validation")
	}

	var jpegBuffer bytes.Buffer
	if err := jpeg.Encode(&jpegBuffer, fixture, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	validJPEG := jpegBuffer.Bytes()
	if err := validateMediaPayload("image", "image/jpeg", validJPEG); err != nil {
		t.Fatalf("valid JPEG rejected: %v", err)
	}
	trailingJPEG := append(slices.Clone(validJPEG), []byte("ignored-trailing-payload")...)
	trailingJPEG = append(trailingJPEG, 0xff, 0xd9)
	if _, format, err := image.Decode(bytes.NewReader(trailingJPEG)); err != nil || format != "jpeg" {
		t.Fatalf("trailing JPEG fixture no longer demonstrates decoder early-stop: %q %v", format, err)
	}
	if err := validateMediaPayload("image", "image/jpeg", trailingJPEG); err == nil {
		t.Fatal("JPEG with bytes after its first EOI passed complete validation")
	}
}

func TestMediaValidationRejectsTypesWithoutBoundedParsers(t *testing.T) {
	for _, test := range []struct {
		kind, mediaType string
		payload         []byte
	}{
		{"audio", "audio/mp3", []byte("ID3signature-only")},
		{"audio", "audio/mpeg", []byte{0xff, 0xfb}},
		{"audio", "audio/aiff", []byte("FORM\x00\x00\x00\x04AIFF")},
		{"audio", "audio/aac", []byte("ADIF")},
		{"audio", "audio/flac", []byte("fLaC")},
		{"audio", "audio/l16", []byte{0, 0}},
		{"audio", "audio/alaw", []byte{0}},
		{"audio", "audio/mulaw", []byte{0}},
		{"image", "image/webp", []byte("RIFF\x04\x00\x00\x00WEBP")},
		{"image", "image/bmp", []byte("BM")},
		{"image", "image/tiff", []byte("II*\x00")},
		{"image", "image/heic", isoBoxFixture("ftyp", []byte("heic\x00\x00\x00\x00"))},
		{"image", "image/heif", isoBoxFixture("ftyp", []byte("mif1\x00\x00\x00\x00"))},
		{"video", "video/mpeg", []byte{0, 0, 1, 0xba}},
		{"video", "video/mpg", []byte{0, 0, 1, 0xba}},
		{"video", "video/avi", []byte("RIFF\x04\x00\x00\x00AVI ")},
		{"video", "video/x-flv", []byte("FLV")},
		{"video", "video/wmv", []byte("0&\xb2u\x8ef\xcf\x11")},
	} {
		t.Run(test.mediaType, func(t *testing.T) {
			if err := validateMediaPayload(test.kind, test.mediaType, test.payload); err == nil ||
				!strings.Contains(err.Error(), "no bounded payload validator") {
				t.Fatalf("signature-only %s validation error = %v", test.mediaType, err)
			}
		})
	}
}

func TestWAVValidationRejectsDuplicateRecoveryAndInvalidExtensibleFormat(t *testing.T) {
	standardFormat := slices.Clone(testWAVPayload()[20:36])
	invalidFormat := slices.Clone(standardFormat)
	binary.LittleEndian.PutUint16(invalidFormat[:2], 99)
	truncatedExtensible := slices.Clone(standardFormat)
	binary.LittleEndian.PutUint16(truncatedExtensible[:2], 0xfffe)
	badExtensible := validExtensibleWAVFormat()
	badExtensible[24] = 99
	for _, test := range []struct {
		name   string
		chunks []wavChunk
	}{
		{"invalid fmt then valid fmt", []wavChunk{{"fmt ", invalidFormat}, {"fmt ", standardFormat}, {"data", make([]byte, 4)}}},
		{"duplicate valid fmt", []wavChunk{{"fmt ", standardFormat}, {"fmt ", standardFormat}, {"data", make([]byte, 4)}}},
		{"empty data then populated data", []wavChunk{{"fmt ", standardFormat}, {"data", nil}, {"data", make([]byte, 4)}}},
		{"truncated extensible fmt", []wavChunk{{"fmt ", truncatedExtensible}, {"data", make([]byte, 4)}}},
		{"unknown extensible subformat", []wavChunk{{"fmt ", badExtensible}, {"data", make([]byte, 4)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMediaPayload("audio", "audio/wav", wavChunkFixture(test.chunks...)); err == nil {
				t.Fatal("inconsistent WAV passed bounded validation")
			}
		})
	}
	valid := wavChunkFixture(
		wavChunk{"fmt ", validExtensibleWAVFormat()}, wavChunk{"data", make([]byte, 4)},
	)
	if err := validateMediaPayload("audio", "audio/wav", valid); err != nil {
		t.Fatalf("valid PCM WAVE_FORMAT_EXTENSIBLE rejected: %v", err)
	}
	floatFormat := validExtensibleFloatWAVFormat()
	if err := validateMediaPayload("audio", "audio/wav", wavChunkFixture(
		wavChunk{"fmt ", floatFormat}, wavChunk{"data", make([]byte, 8)},
	)); err != nil {
		t.Fatalf("valid IEEE-float WAVE_FORMAT_EXTENSIBLE rejected: %v", err)
	}
	for _, mutation := range []string{"16-bit float", "invalid valid-bits"} {
		t.Run(mutation, func(t *testing.T) {
			format := slices.Clone(floatFormat)
			if mutation == "16-bit float" {
				binary.LittleEndian.PutUint16(format[14:16], 16)
				binary.LittleEndian.PutUint16(format[18:20], 16)
				binary.LittleEndian.PutUint16(format[12:14], 2)
				binary.LittleEndian.PutUint32(format[8:12], 48_000)
			} else {
				binary.LittleEndian.PutUint16(format[18:20], 33)
			}
			if err := validateMediaPayload("audio", "audio/wav", wavChunkFixture(
				wavChunk{"fmt ", format}, wavChunk{"data", make([]byte, 8)},
			)); err == nil {
				t.Fatal("invalid IEEE-float WAVE_FORMAT_EXTENSIBLE passed validation")
			}
		})
	}
	standardWithEmptyExtension := append(slices.Clone(standardFormat), 0, 0)
	if err := validateMediaPayload("audio", "audio/wav", wavChunkFixture(
		wavChunk{"fmt ", standardWithEmptyExtension}, wavChunk{"data", make([]byte, 4)},
	)); err != nil {
		t.Fatalf("valid zero-length WAVEFORMATEX extension rejected: %v", err)
	}
	badExtensionSize := slices.Clone(standardWithEmptyExtension)
	binary.LittleEndian.PutUint16(badExtensionSize[16:18], 1)
	if err := validateMediaPayload("audio", "audio/wav", wavChunkFixture(
		wavChunk{"fmt ", badExtensionSize}, wavChunk{"data", make([]byte, 4)},
	)); err == nil {
		t.Fatal("WAVEFORMATEX with cbSize inconsistent with the fmt chunk passed validation")
	}
}

func TestPNGReservedTypeBitGIFControlGrammarAndDecodedByteBounds(t *testing.T) {
	fixture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, fixture); err != nil {
		t.Fatal(err)
	}
	reservedPNG := slices.Clone(pngBuffer.Bytes())
	for offset := 8; offset+12 <= len(reservedPNG); {
		length := int(binary.BigEndian.Uint32(reservedPNG[offset : offset+4]))
		end := offset + 12 + length
		if string(reservedPNG[offset+4:offset+8]) == "IDAT" {
			reservedPNG[offset+6] = 'a'
			binary.BigEndian.PutUint32(
				reservedPNG[offset+8+length:end],
				crc32.ChecksumIEEE(reservedPNG[offset+4:offset+8+length]),
			)
			break
		}
		offset = end
	}
	if validPNGStructure(reservedPNG) {
		t.Fatal("PNG chunk with a lowercase reserved type byte passed validation")
	}

	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	var gifBuffer bytes.Buffer
	if err := gif.EncodeAll(&gifBuffer, &gif.GIF{
		Image: []*image.Paletted{frame}, Delay: []int{1},
		Config: image.Config{ColorModel: palette, Width: 2, Height: 2},
	}); err != nil {
		t.Fatal(err)
	}
	validGIF := gifBuffer.Bytes()
	control := bytes.Index(validGIF, []byte{0x21, 0xf9, 0x04})
	if control < 0 || control+8 > len(validGIF) {
		t.Fatal("generated GIF has no graphic-control extension")
	}
	duplicateControl := append(slices.Clone(validGIF[:control]), validGIF[control:control+8]...)
	duplicateControl = append(duplicateControl, validGIF[control:]...)
	if _, _, ok := boundedGIFStructure(duplicateControl); ok {
		t.Fatal("GIF with two pending graphic-control extensions passed validation")
	}
	reservedControl := slices.Clone(validGIF)
	reservedControl[control+3] |= 0x20
	if _, _, ok := boundedGIFStructure(reservedControl); ok {
		t.Fatal("GIF graphic-control extension with reserved bits passed validation")
	}
	if got := decodedImageBytes(16_384, 16_384); got <= maximumDecodedImageBytes {
		t.Fatalf("decoded-image byte admission accepted %d bytes", got)
	}
}

func TestISOBMFFSingletonBrandsAndMediaDataAccumulation(t *testing.T) {
	valid := isoTrackFixture("isom", "vide", "avc1")
	boxes, ok := parseISOBoxes(valid)
	if !ok || len(boxes) != 3 {
		t.Fatal("structural ISO fixture did not parse")
	}
	fileType := isoBoxFixture("ftyp", boxes[0].data)
	movie := isoBoxFixture("moov", boxes[1].data)
	mediaData := isoBoxFixture("mdat", []byte{1})
	emptyMediaData := isoBoxFixture("mdat", nil)
	withTrailingEmptyMediaData := bytes.Join([][]byte{fileType, movie, mediaData, emptyMediaData}, nil)
	if summary := inspectISOBMFF(withTrailingEmptyMediaData); !summary.valid || !summary.mediaData {
		t.Fatal("a later empty mdat erased prior non-empty media evidence")
	}
	for name, payload := range map[string][]byte{
		"duplicate ftyp": bytes.Join([][]byte{fileType, fileType, movie, mediaData}, nil),
		"duplicate moov": bytes.Join([][]byte{fileType, movie, movie, mediaData}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if inspectISOBMFF(payload).valid {
				t.Fatal("duplicate singleton ISO box passed validation")
			}
		})
	}
	tooManyBrands := make([]byte, 8+4*1025)
	copy(tooManyBrands[:4], "isom")
	if inspectISOBMFF(bytes.Join([][]byte{
		isoBoxFixture("ftyp", tooManyBrands), movie, mediaData,
	}, nil)).valid {
		t.Fatal("ftyp with more than 1024 compatible brands passed validation")
	}
}

type wavChunk struct {
	kind string
	data []byte
}

func wavChunkFixture(chunks ...wavChunk) []byte {
	result := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, chunk := range chunks {
		header := make([]byte, 8)
		copy(header[:4], chunk.kind)
		binary.LittleEndian.PutUint32(header[4:8], uint32(len(chunk.data)))
		result = append(result, header...)
		result = append(result, chunk.data...)
		if len(chunk.data)%2 != 0 {
			result = append(result, 0)
		}
	}
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	return result
}

func validExtensibleWAVFormat() []byte {
	format := make([]byte, 40)
	binary.LittleEndian.PutUint16(format[:2], 0xfffe)
	binary.LittleEndian.PutUint16(format[2:4], 1)
	binary.LittleEndian.PutUint32(format[4:8], 24_000)
	binary.LittleEndian.PutUint32(format[8:12], 48_000)
	binary.LittleEndian.PutUint16(format[12:14], 2)
	binary.LittleEndian.PutUint16(format[14:16], 16)
	binary.LittleEndian.PutUint16(format[16:18], 22)
	binary.LittleEndian.PutUint16(format[18:20], 16)
	binary.LittleEndian.PutUint32(format[20:24], 1)
	copy(format[24:40], []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	return format
}

func validExtensibleFloatWAVFormat() []byte {
	format := validExtensibleWAVFormat()
	binary.LittleEndian.PutUint32(format[8:12], 96_000)
	binary.LittleEndian.PutUint16(format[12:14], 4)
	binary.LittleEndian.PutUint16(format[14:16], 32)
	binary.LittleEndian.PutUint16(format[18:20], 32)
	format[24] = 3
	return format
}

func oggOpusFixture() []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint32(head[12:16], 48_000)
	tags := make([]byte, 16)
	copy(tags, "OpusTags")
	return oggPageFixture([][]byte{head, tags, {0xf8, 0xff, 0xfe}})
}

func oggPageFixture(packets [][]byte) []byte {
	segments := make([]byte, 0, len(packets))
	var body []byte
	for _, packet := range packets {
		for len(packet) >= 255 {
			segments = append(segments, 255)
			body = append(body, packet[:255]...)
			packet = packet[255:]
		}
		segments = append(segments, byte(len(packet)))
		body = append(body, packet...)
	}
	result := make([]byte, 27+len(segments)+len(body))
	copy(result[:4], "OggS")
	result[5] = 0x06 // beginning and end of the single logical stream
	binary.LittleEndian.PutUint32(result[14:18], 0x10203040)
	result[26] = byte(len(segments))
	copy(result[27:], segments)
	copy(result[27+len(segments):], body)
	binary.LittleEndian.PutUint32(result[22:26], testOggChecksum(result))
	return result
}

func testOggChecksum(page []byte) uint32 {
	var checksum uint32
	for index, value := range page {
		if index >= 22 && index < 26 {
			value = 0
		}
		checksum ^= uint32(value) << 24
		for bit := 0; bit < 8; bit++ {
			if checksum&0x80000000 != 0 {
				checksum = checksum<<1 ^ 0x04c11db7
			} else {
				checksum <<= 1
			}
		}
	}
	return checksum
}

func isoTrackFixture(brand, handler, codec string) []byte {
	handlerData := make([]byte, 12)
	copy(handlerData[8:12], handler)
	stsdData := make([]byte, 8)
	binary.BigEndian.PutUint32(stsdData[4:8], 1)
	stsdData = append(stsdData, isoBoxFixture(codec, nil)...)
	track := isoBoxFixture("trak", isoBoxFixture("mdia", append(
		isoBoxFixture("hdlr", handlerData),
		isoBoxFixture("minf", isoBoxFixture("stbl", isoBoxFixture("stsd", stsdData)))...,
	)))
	fileType := append([]byte(brand), []byte{0, 0, 0, 0}...)
	return bytes.Join([][]byte{
		isoBoxFixture("ftyp", fileType), isoBoxFixture("moov", track),
		isoBoxFixture("mdat", []byte{1}),
	}, nil)
}

func isoBoxFixture(kind string, data []byte) []byte {
	result := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
	copy(result[4:8], kind)
	copy(result[8:], data)
	return result
}

func TestEvaluateCannotTurnPostProviderCancellationIntoSuccess(t *testing.T) {
	for _, cancelAt := range []string{"review", "verification"} {
		t.Run(cancelAt, func(t *testing.T) {
			request, _ := testRequest(t)
			descriptor := testDescriptor("cancel-model")
			ctx, cancel := context.WithCancel(t.Context())
			provider := &testProvider{
				descriptor: descriptor,
				response: ProviderResponse{
					Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
					RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
				},
			}
			if cancelAt == "review" {
				provider.onReview = func(PreparedRequest) { cancel() }
			} else {
				provider.onVerify = cancel
				provider.verifyErr = errors.New("verification failed after cancellation")
			}
			_, err := Evaluate(ctx, openTestLease(t, provider), request)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Evaluate() error = %v, want cancellation", err)
			}
		})
	}
}

type cancelingReader struct {
	cancel context.CancelFunc
	read   bool
}

func (reader *cancelingReader) Read(target []byte) (int, error) {
	if reader.read {
		return 0, io.EOF
	}
	reader.read = true
	reader.cancel()
	return copy(target, "payload"), nil
}

func TestBoundedReadObservesCancellationDuringRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	_, err := readBoundedContext(ctx, &cancelingReader{cancel: cancel}, 64)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readBoundedContext() error = %v", err)
	}
}

func TestAssessmentAccepts128AndRejects129ItemsPerCollection(t *testing.T) {
	makeFinding := func(index int) Finding {
		return Finding{
			Category: fmt.Sprintf("problem_%d", index),
			Evidence: "retained evidence", Impact: "review impact",
		}
	}
	for _, collection := range []string{"significant_problems", "minor_observations", "limitations"} {
		for _, count := range []int{128, 129} {
			t.Run(fmt.Sprintf("%s/%d", collection, count), func(t *testing.T) {
				assessment := Assessment{
					MediaUsable: true, ObservedOutcome: "pass", AgreesWithDeterministic: true,
					Confidence: 1, Summary: "bounded output",
					SignificantProblems: []Finding{}, MinorObservations: []Finding{}, Limitations: []string{},
				}
				switch collection {
				case "significant_problems":
					for index := 0; index < count; index++ {
						assessment.SignificantProblems = append(assessment.SignificantProblems, makeFinding(index))
					}
				case "minor_observations":
					for index := 0; index < count; index++ {
						assessment.MinorObservations = append(assessment.MinorObservations, makeFinding(index))
					}
				case "limitations":
					for index := 0; index < count; index++ {
						assessment.Limitations = append(assessment.Limitations, fmt.Sprintf("limitation %d", index))
					}
				}
				payload, err := json.Marshal(assessment)
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = normalizeAssessment(payload)
				if count == 128 && err != nil {
					t.Fatalf("128 entries rejected: %v", err)
				}
				if count == 129 && (err == nil || !strings.Contains(err.Error(), "more than 128")) {
					t.Fatalf("129 entries error = %v", err)
				}
			})
		}
	}
}

func TestFindingCategoryRequiresLowercaseSnakeCase(t *testing.T) {
	for _, category := range []string{
		"Audio_dropout", "audio-dropout", "audio__dropout", "_audio_dropout", "audio_dropout_",
		"123", "2_audio",
	} {
		t.Run(category, func(t *testing.T) {
			err := validateFinding("finding", Finding{
				Category: category, Evidence: "evidence", Impact: "impact",
			})
			if err == nil || !strings.Contains(err.Error(), "lowercase snake_case") {
				t.Fatalf("validateFinding() error = %v", err)
			}
		})
	}
	if err := validateFinding("finding", Finding{
		Category: "audio_dropout_2", Evidence: "evidence", Impact: "impact",
	}); err != nil {
		t.Fatalf("valid lowercase snake_case category rejected: %v", err)
	}
}

func TestRecordMediaIdentityRejectsExactParentTraversal(t *testing.T) {
	media := Media{
		Kind: "audio", Role: "room_and_agent", Path: "..",
		SHA256: digest([]byte("payload")), MediaType: "audio/wav",
	}
	if err := validateMediaIdentity(media); err == nil || !strings.Contains(err.Error(), "without traversal") {
		t.Fatalf("validateMediaIdentity() error = %v", err)
	}
}

func TestMediaIdentityRejectsInvalidUTF8AndControlPaths(t *testing.T) {
	for _, path := range []string{"media/line\nbreak.wav", string([]byte{'m', 'e', 'd', 'i', 'a', '/', 0xff})} {
		media := Media{
			Kind: "audio", Role: "room_and_agent", Path: path,
			SHA256: digest([]byte("payload")), MediaType: "audio/wav",
		}
		if err := validateMediaIdentity(media); err == nil {
			t.Fatalf("invalid media path %q passed identity validation", path)
		}
	}
}

func TestReviewContractVersionsReflectIncompatibleFormatChanges(t *testing.T) {
	if FormatVersion != 5 || CasePromptVersion != "openrealtime.case-media-review.prompt.v7" ||
		CaseSchemaVersion != "openrealtime.case-media-review.schema.v3" ||
		SanitizationVersion != "openrealtime.review-sanitization.v4" ||
		MediaValidationVersion != "openrealtime.media-container-validation.v4" {
		t.Fatalf("contract versions = format:%d prompt:%q schema:%q sanitization:%q media:%q",
			FormatVersion, CasePromptVersion, CaseSchemaVersion,
			SanitizationVersion, MediaValidationVersion)
	}
}

func TestEvaluateRejectsLiveIdentityDriftBeforeReviewSideEffects(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("preflight-drift-model")
	for _, drift := range []string{"descriptor", "implementation", "configuration"} {
		t.Run(drift, func(t *testing.T) {
			provider := &testProvider{
				descriptor: descriptor,
				response: ProviderResponse{
					Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
					RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
				},
			}
			registry, err := NewRegistry([]Registration{
				testRegistration("provider", descriptor,
					func(context.Context) (Provider, error) { return provider, nil }),
			})
			if err != nil {
				t.Fatal(err)
			}
			lease, err := registry.Open(t.Context(), "provider")
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			switch drift {
			case "descriptor":
				changed := descriptor
				changed.APIRevision = "v2"
				provider.nextDescriptor = &changed
			case "implementation":
				provider.implementation = []byte("drifted implementation")
			case "configuration":
				provider.configuration = []byte(`{"name":"drifted"}`)
			}
			if _, err := Evaluate(t.Context(), lease, request); err == nil ||
				!strings.Contains(err.Error(), "before evaluation") {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if calls := provider.reviewCalls.Load(); calls != 0 {
				t.Fatalf("drifted provider performed %d Review side effects", calls)
			}
		})
	}
}

func TestProviderOwnershipClaimRejectsSingletonAcrossLeases(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("singleton-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
		},
	}
	registry, err := NewRegistry([]Registration{
		testRegistration("provider", descriptor,
			func(context.Context) (Provider, error) { return provider, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open(t.Context(), "provider"); err == nil ||
		!strings.Contains(err.Error(), "exclusive ownership claim failed") {
		t.Fatalf("second Open() error = %v", err)
	}
	if provider.closeCalls.Load() != 0 {
		t.Fatal("failed second claim closed the first lease's active provider")
	}
	if _, err := Evaluate(t.Context(), first, request); err != nil {
		t.Fatalf("first lease stopped working after rejected singleton reuse: %v", err)
	}
	if err := first.Close(); err != nil || provider.closeCalls.Load() != 1 {
		t.Fatalf("first Close() = %v; calls=%d", err, provider.closeCalls.Load())
	}
	if _, err := registry.Open(t.Context(), "provider"); err == nil {
		t.Fatal("closed singleton provider was claimed a second time")
	}
	if provider.closeCalls.Load() != 1 {
		t.Fatalf("rejected singleton reuse closed provider %d times", provider.closeCalls.Load())
	}
}

func TestFactoryErrorCannotCloseAlreadyClaimedSingleton(t *testing.T) {
	for _, cancelSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelSecond), func(t *testing.T) {
			secondContext := t.Context()
			secondCancel := context.CancelFunc(func() {})
			if cancelSecond {
				secondContext, secondCancel = context.WithCancel(t.Context())
			}
			defer secondCancel()

			request, _ := testRequest(t)
			descriptor := testDescriptor("factory-error-singleton")
			provider := &testProvider{
				descriptor: descriptor,
				response: ProviderResponse{
					Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
					RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
				},
			}
			var opens atomic.Int32
			registry, err := NewRegistry([]Registration{testRegistration(
				"provider", descriptor, func(context.Context) (Provider, error) {
					if opens.Add(1) == 1 {
						return provider, nil
					}
					if cancelSecond {
						secondCancel()
					}
					return provider, errors.New("factory failed")
				},
			)})
			if err != nil {
				t.Fatal(err)
			}
			first, err := registry.Open(t.Context(), "provider")
			if err != nil {
				t.Fatal(err)
			}
			_, err = registry.Open(secondContext, "provider")
			if err == nil || !strings.Contains(err.Error(), "factory failed") ||
				(cancelSecond && !errors.Is(err, context.Canceled)) {
				t.Fatalf("second Open() error = %v", err)
			}
			if provider.closeCalls.Load() != 0 {
				t.Fatal("factory error closed the first lease's provider")
			}
			if _, err := Evaluate(t.Context(), first, request); err != nil {
				t.Fatalf("first lease stopped working: %v", err)
			}
			if err := first.Close(); err != nil || provider.closeCalls.Load() != 1 {
				t.Fatalf("first Close() = %v; calls=%d", err, provider.closeCalls.Load())
			}
		})
	}
}

func TestRegistryObservesCancellationDuringProviderValidation(t *testing.T) {
	descriptor := testDescriptor("open-cancel-model")
	ctx, cancel := context.WithCancel(t.Context())
	provider := &testProvider{descriptor: descriptor, onConfiguration: cancel}
	registry, err := NewRegistry([]Registration{
		testRegistration("provider", descriptor,
			func(context.Context) (Provider, error) { return provider, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open(ctx, "provider"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open() error = %v, want cancellation", err)
	}
	if provider.closeCalls.Load() != 1 {
		t.Fatalf("canceled open closed provider %d times", provider.closeCalls.Load())
	}
}

func TestEvaluateChecksCancellationImmediatelyBeforeReview(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("pre-review-cancel-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
		},
	}
	registry, err := NewRegistry([]Registration{
		testRegistration("provider", descriptor,
			func(context.Context) (Provider, error) { return provider, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithCancel(t.Context())
	provider.onConfiguration = cancel
	if _, err := Evaluate(ctx, lease, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Evaluate() error = %v, want cancellation", err)
	}
	if provider.reviewCalls.Load() != 0 {
		t.Fatal("provider Review ran after pre-call cancellation")
	}
}

func TestRegistryRejectsArtifactDriftAndClosesFailedProvidersOnce(t *testing.T) {
	descriptor := testDescriptor("artifact-drift-model")
	validFactory := func(context.Context) (Provider, error) {
		return &testProvider{descriptor: descriptor}, nil
	}
	implementationDrift := testRegistration("provider", descriptor, validFactory)
	implementationDrift.Implementation = []byte("drifted")
	if _, err := NewRegistry([]Registration{implementationDrift}); err == nil ||
		!strings.Contains(err.Error(), "implementation bytes drifted") {
		t.Fatalf("implementation registration error = %v", err)
	}
	configurationDrift := testRegistration("provider", descriptor, validFactory)
	configurationDrift.Configuration = []byte(`{ "name": "artifact-drift-model" }`)
	if _, err := NewRegistry([]Registration{configurationDrift}); err == nil ||
		!strings.Contains(err.Error(), "configuration bytes drifted") {
		t.Fatalf("configuration registration error = %v", err)
	}

	for _, test := range []struct {
		name     string
		provider *testProvider
		factory  func(*testProvider) ProviderFactory
		match    string
	}{
		{
			name: "implementation", provider: &testProvider{
				descriptor: descriptor, implementation: []byte("different implementation"),
			},
			factory: func(provider *testProvider) ProviderFactory {
				return func(context.Context) (Provider, error) { return provider, nil }
			}, match: "implementation drifted",
		},
		{
			name: "configuration", provider: &testProvider{
				descriptor: descriptor, configuration: []byte(`{"name":"different"}`),
			},
			factory: func(provider *testProvider) ProviderFactory {
				return func(context.Context) (Provider, error) { return provider, nil }
			}, match: "configuration drifted",
		},
		{
			name: "factory error", provider: &testProvider{descriptor: descriptor},
			factory: func(provider *testProvider) ProviderFactory {
				return func(context.Context) (Provider, error) { return provider, errors.New("open failed") }
			}, match: "open failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := NewRegistry([]Registration{
				testRegistration("provider", descriptor, test.factory(test.provider)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Open(t.Context(), "provider"); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Open() error = %v", err)
			}
			wantClose := int32(1)
			if test.name == "factory error" {
				wantClose = 0
			}
			if calls := test.provider.closeCalls.Load(); calls != wantClose {
				t.Fatalf("provider close calls = %d, want %d", calls, wantClose)
			}
		})
	}
}

func TestProviderLeaseConcurrentCloseWaitsAndClosesExactlyOnce(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("concurrent-close-model")
	entered := make(chan struct{})
	release := make(chan struct{})
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{}`), Output: validAssessment, ReportedModel: descriptor.Model,
			RequestIDState: ProviderRequestIDMissing, Request: []byte(`{"wire":true}`),
		},
		onReview: func(PreparedRequest) {
			close(entered)
			<-release
		},
	}
	registry, err := NewRegistry([]Registration{
		testRegistration("provider", descriptor,
			func(context.Context) (Provider, error) { return provider, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	evaluationDone := make(chan error, 1)
	go func() {
		_, evaluateErr := Evaluate(t.Context(), lease, request)
		evaluationDone <- evaluateErr
	}()
	<-entered

	const closers = 16
	closeErrors := make(chan error, closers)
	var group sync.WaitGroup
	for index := 0; index < closers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			closeErrors <- lease.Close()
		}()
	}
	select {
	case err := <-closeErrors:
		t.Fatalf("Close returned before the active evaluation finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-evaluationDone; err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	group.Wait()
	close(closeErrors)
	for err := range closeErrors {
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	if calls := provider.closeCalls.Load(); calls != 1 {
		t.Fatalf("provider close calls = %d, want 1", calls)
	}
	if lease.Descriptor() != (ProviderDescriptor{}) {
		t.Fatal("closed lease exposed a descriptor")
	}

	zero := &ProviderLease{}
	if err := zero.Close(); err != nil || zero.Descriptor() != (ProviderDescriptor{}) {
		t.Fatalf("zero-value lease close/descriptor = %v, %+v", err, zero.Descriptor())
	}
	if _, err := Evaluate(t.Context(), zero, request); err == nil {
		t.Fatal("zero-value lease was accepted by Evaluate")
	}
}

func TestFilesystemIdentityChecksRejectDeterministicSymlinkSwaps(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		parent := t.TempDir()
		rootPath := filepath.Join(parent, "root")
		movedPath := filepath.Join(parent, "root-moved")
		if err := os.Mkdir(rootPath, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := openValidatedRoot(rootPath, func() {
			if renameErr := os.Rename(rootPath, movedPath); renameErr != nil {
				t.Fatal(renameErr)
			}
			if linkErr := os.Symlink(filepath.Base(movedPath), rootPath); linkErr != nil {
				t.Fatal(linkErr)
			}
		})
		if err == nil {
			t.Fatal("root rename-to-symlink swap was accepted")
		}
	})

	for _, target := range []string{"intermediate", "file"} {
		t.Run(target, func(t *testing.T) {
			rootPath := t.TempDir()
			mediaPath := filepath.Join(rootPath, "media")
			if err := os.Mkdir(mediaPath, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mediaPath, "case.wav"), []byte("payload"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := openValidatedRoot(rootPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			swapped := false
			_, _, err = openRegularNoSymlink(root, "media/case.wav", func(prefix string) {
				if swapped || prefix != map[string]string{
					"intermediate": "media", "file": filepath.Join("media", "case.wav"),
				}[target] {
					return
				}
				swapped = true
				if target == "intermediate" {
					if renameErr := os.Rename(mediaPath, mediaPath+"-moved"); renameErr != nil {
						t.Fatal(renameErr)
					}
					if linkErr := os.Symlink("media-moved", mediaPath); linkErr != nil {
						t.Fatal(linkErr)
					}
					return
				}
				filePath := filepath.Join(mediaPath, "case.wav")
				if renameErr := os.Rename(filePath, filePath+"-moved"); renameErr != nil {
					t.Fatal(renameErr)
				}
				if linkErr := os.Symlink("case.wav-moved", filePath); linkErr != nil {
					t.Fatal(linkErr)
				}
			})
			if !swapped || err == nil {
				t.Fatalf("%s rename-to-symlink swap accepted; swapped=%t error=%v", target, swapped, err)
			}
		})
	}
}

func TestProviderArtifactAccessorsReturnOwnedBytes(t *testing.T) {
	descriptor := testDescriptor("owned-artifact-model")
	provider := &testProvider{descriptor: descriptor}
	implementation := provider.Implementation()
	configuration := provider.Configuration()
	implementation[0] ^= 1
	configuration[0] ^= 1
	if bytes.Equal(implementation, provider.Implementation()) ||
		bytes.Equal(configuration, provider.Configuration()) {
		t.Fatal("provider artifact accessors alias mutable caller bytes")
	}

	registration := testRegistration("provider", descriptor,
		func(context.Context) (Provider, error) { return provider, nil })
	originalImplementation := slices.Clone(registration.Implementation)
	originalConfiguration := slices.Clone(registration.Configuration)
	registry, err := NewRegistry([]Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	registration.Implementation[0] ^= 1
	registration.Configuration[0] ^= 1
	lease, err := registry.Open(t.Context(), "provider")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, gotImplementation, gotConfiguration, err := lease.provenance()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotImplementation, originalImplementation) ||
		!bytes.Equal(gotConfiguration, originalConfiguration) {
		t.Fatal("registry provenance aliased registration artifact bytes")
	}
}
