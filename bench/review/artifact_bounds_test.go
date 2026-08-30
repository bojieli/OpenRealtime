package review

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestArtifactRecordTypedCardinalityIsRejectedBeforeMarshal(t *testing.T) {
	evaluation := artifactBoundsEvaluation(t, nil)

	tests := []struct {
		name   string
		mutate func(*Record)
		want   string
	}{
		{
			name: "media",
			mutate: func(record *Record) {
				record.Media = make([]Media, maximumMediaCount+1)
			},
			want: "media artifacts",
		},
		{
			name: "significant problems",
			mutate: func(record *Record) {
				record.Assessment.SignificantProblems = make([]Finding, 129)
			},
			want: "more than 128",
		},
		{
			name: "minor observations",
			mutate: func(record *Record) {
				record.Assessment.MinorObservations = make([]Finding, 129)
			},
			want: "more than 128",
		},
		{
			name: "limitations",
			mutate: func(record *Record) {
				record.Assessment.Limitations = make([]string, 129)
			},
			want: "more than 128",
		},
		{
			name: "summary",
			mutate: func(record *Record) {
				record.Assessment.Summary = strings.Repeat("s", (16<<10)+1)
			},
			want: "no larger than 16384 bytes",
		},
		{
			name: "sensitive value count",
			mutate: func(record *Record) {
				record.SensitiveValueCount = maximumSensitiveValues + 1
			},
			want: "sensitive-value count",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := evaluation.Record
			test.mutate(&record)
			if _, err := MarshalRecord(record); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("MarshalRecord() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestDecodeRecordAppliesStructuralCardinalityBeforeTypedDecode(t *testing.T) {
	var source strings.Builder
	source.WriteString(`{"media":[`)
	for index := 0; index <= maximumMediaCount; index++ {
		if index != 0 {
			source.WriteByte(',')
		}
		source.WriteString("null")
	}
	source.WriteString(`]}`)

	_, err := DecodeRecord(strings.NewReader(source.String()))
	if err == nil || !strings.Contains(err.Error(), "array elements 257 exceed maximum 256") {
		t.Fatalf("DecodeRecord() error = %v, want strict pre-decode array limit", err)
	}
}

func TestRecordStructuralLimitsAdmitEveryContractMaximum(t *testing.T) {
	evaluation := artifactBoundsEvaluation(t, nil)
	record := evaluation.Record
	baseMedia := record.Media[0]
	record.Media = make([]Media, maximumMediaCount)
	for index := range record.Media {
		item := baseMedia
		item.Path = fmt.Sprintf("media/case-%03d.wav", index)
		record.Media[index] = item
	}
	finding := Finding{Category: "observable_problem", Evidence: "evidence", Impact: "impact"}
	record.Assessment.SignificantProblems = make([]Finding, 128)
	record.Assessment.MinorObservations = make([]Finding, 128)
	record.Assessment.Limitations = make([]string, 128)
	for index := 0; index < 128; index++ {
		record.Assessment.SignificantProblems[index] = finding
		record.Assessment.MinorObservations[index] = finding
		record.Assessment.Limitations[index] = "bounded limitation"
	}

	payload, err := MarshalRecord(record)
	if err != nil {
		t.Fatalf("MarshalRecord() at contract maxima: %v", err)
	}
	decoded, err := DecodeRecord(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("DecodeRecord() at contract maxima: %v", err)
	}
	if len(decoded.Media) != maximumMediaCount ||
		len(decoded.Assessment.SignificantProblems) != 128 ||
		len(decoded.Assessment.MinorObservations) != 128 ||
		len(decoded.Assessment.Limitations) != 128 {
		t.Fatalf("decoded maximum-cardinality record has unexpected shape: %+v", decoded)
	}
}

func TestVerifyArtifactsPreflightsEverySizeBeforeHashing(t *testing.T) {
	evaluation := artifactBoundsEvaluation(t, nil)
	base := [][]byte{
		evaluation.ProviderImplementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	}
	labels := []string{
		"provider implementation", "provider configuration", "provider request", "prompt",
		"schema", "context", "raw response", "normalized output",
	}
	for index, label := range labels {
		t.Run("empty "+label, func(t *testing.T) {
			artifacts := append([][]byte(nil), base...)
			artifacts[index] = nil
			err := VerifyArtifacts(
				evaluation.Record, artifacts[0], artifacts[1], artifacts[2], artifacts[3],
				artifacts[4], artifacts[5], artifacts[6], artifacts[7],
			)
			if err == nil || !strings.Contains(err.Error(), "review "+label+" must be") {
				t.Fatalf("VerifyArtifacts() error = %v, want %s size admission", err, label)
			}
		})
	}

	implementation := append([]byte(nil), evaluation.ProviderImplementation...)
	implementation[0] ^= 1 // Deliberately wrong, but still within its admitted size.
	oversizedOutput := make([]byte, maximumNormalizedOutputBytes+1)

	err := VerifyArtifacts(
		evaluation.Record, implementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, oversizedOutput,
	)
	if err == nil || !strings.Contains(err.Error(), "normalized output must be") {
		t.Fatalf("VerifyArtifacts() error = %v, want late artifact size preflight", err)
	}
}

func TestArtifactCanonicalEncodingAndReconstructionDoNotEscapeHTML(t *testing.T) {
	evaluation := artifactBoundsEvaluation(t, func(request *Request) {
		request.AttemptID = "attempt<&>"
		request.Suite = "suite<&>"
		request.Case = "case<&>"
		request.Context = []byte(`{"markup":"<&>"}`)
	})

	payload, err := MarshalRecord(evaluation.Record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`\u003c`)) || bytes.Contains(payload, []byte(`\u003e`)) ||
		bytes.Contains(payload, []byte(`\u0026`)) || !bytes.Contains(payload, []byte(`"attempt_id": "attempt<&>"`)) {
		t.Fatalf("canonical record unexpectedly HTML-escaped: %s", payload)
	}
	decoded, err := DecodeRecord(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifacts(
		decoded, evaluation.ProviderImplementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	); err != nil {
		t.Fatalf("VerifyArtifacts() with canonical HTML-significant inputs: %v", err)
	}
}

func artifactBoundsEvaluation(t *testing.T, mutate func(*Request)) Evaluation {
	t.Helper()
	request, _ := testRequest(t)
	if mutate != nil {
		mutate(&request)
	}
	descriptor := testDescriptor("artifact-bounds-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{"id":"artifact-bounds-response"}`), Output: validAssessment,
			ReportedModel: descriptor.Model, RequestID: "artifact-bounds-request",
			RequestIDState: ProviderRequestIDValue,
			Request:        []byte("artifact bounds wire request"),
		},
	}
	evaluation, err := Evaluate(t.Context(), openTestLease(t, provider), request)
	if err != nil {
		t.Fatal(err)
	}
	return evaluation
}
