package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testEvaluationBundleEvaluation(
	t *testing.T, sensitive []string,
) (Evaluation, Request) {
	t.Helper()
	request, _ := testRequest(t)
	request.SensitiveValues = slices.Clone(sensitive)
	descriptor := testDescriptor("bundle-model")
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw:    []byte(`{"id":"bundle-response","status":"complete"}`),
			Output: validAssessment, ReportedModel: descriptor.Model,
			RequestID: "bundle-request", RequestIDState: ProviderRequestIDValue,
			Request: []byte(`{"model":"bundle-model","input":"retained"}`),
		},
	}
	evaluation, err := Evaluate(t.Context(), openTestLease(t, provider), request)
	if err != nil {
		t.Fatal(err)
	}
	return evaluation, request
}

func TestEvaluationOwnsExactReviewedMedia(t *testing.T) {
	evaluation, request := testEvaluationBundleEvaluation(t, nil)
	if len(evaluation.Media) != 1 ||
		!bytes.Equal(evaluation.Media[0].Bytes, testWAVPayload()) ||
		evaluation.Media[0].Media != evaluation.Record.Media[0] {
		t.Fatalf("Evaluation.Media = %+v, want exact retained medium", evaluation.Media)
	}
	replacement := testWAVPayload()
	replacement[len(replacement)-1] = 99
	if err := os.WriteFile(
		filepath.Join(request.RootDirectory, request.Media[0].Path), replacement, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(evaluation.Media[0].Bytes, replacement) {
		t.Fatal("Evaluation.Media aliases or rereads mutable source media")
	}
}

func TestEvaluationCancellationReturnsNoRetainedMedia(t *testing.T) {
	request, _ := testRequest(t)
	descriptor := testDescriptor("bundle-cancel-model")
	ctx, cancel := context.WithCancel(t.Context())
	provider := &testProvider{
		descriptor: descriptor,
		response: ProviderResponse{
			Raw: []byte(`{"status":"complete"}`), Output: validAssessment,
			ReportedModel: descriptor.Model, RequestIDState: ProviderRequestIDMissing,
			Request: []byte(`{"input":"retained"}`),
		},
		onVerify: cancel,
	}
	evaluation, err := Evaluate(ctx, openTestLease(t, provider), request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Evaluate() error = %v, want cancellation", err)
	}
	if !reflect.DeepEqual(evaluation, Evaluation{}) || evaluation.Media != nil {
		t.Fatalf("canceled Evaluation retained bytes: %+v", evaluation)
	}
}

func TestEvaluationBundleRoundTripRetainsExactCanonicalReview(t *testing.T) {
	evaluation, request := testEvaluationBundleEvaluation(t, nil)
	// Prove retention uses the Evaluation snapshot rather than reopening the
	// source path after the provider has reviewed it.
	replacement := testWAVPayload()
	replacement[len(replacement)-1] = 77
	if err := os.WriteFile(
		filepath.Join(request.RootDirectory, request.Media[0].Path), replacement, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "evaluation")
	options := EvaluationBundleOptions{Directory: directory}
	receipt, err := WriteEvaluationBundle(t.Context(), options, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenEvaluationBundle(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened.Receipt, receipt) || opened.Record.AttemptID != evaluation.Record.AttemptID {
		t.Fatalf("opened receipt/record = %+v / %+v, want %+v", opened.Receipt, opened.Record, receipt)
	}
	for label, value := range map[string]string{
		"manifest": receipt.ManifestSHA256, "record": receipt.RecordSHA256,
		"file set": receipt.FileSetSHA256, "receipt": receipt.ReceiptSHA256,
	} {
		if err := validateDigest(value); err != nil {
			t.Fatalf("%s digest %q: %v", label, value, err)
		}
	}
	retainedMedia, err := os.ReadFile(filepath.Join(directory, "media-001.wav"))
	if err != nil || !bytes.Equal(retainedMedia, evaluation.Media[0].Bytes) ||
		bytes.Equal(retainedMedia, replacement) {
		t.Fatalf("retained media differs from provider-reviewed bytes: err=%v", err)
	}
	for path, want := range map[string][]byte{
		"provider-implementation.bin": evaluation.ProviderImplementation,
		"provider-configuration.json": evaluation.ProviderConfiguration,
		"provider-request.bin":        evaluation.ProviderRequest,
		"prompt.txt":                  evaluation.Prompt,
		"schema.json":                 evaluation.Schema,
		"context.json":                evaluation.Context,
		"raw-response.bin":            evaluation.RawResponse,
		"normalized-output.json":      evaluation.NormalizedOutput,
	} {
		got, err := os.ReadFile(filepath.Join(directory, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("retained %s differs: err=%v", path, err)
		}
	}
	review, err := os.ReadFile(filepath.Join(directory, evaluationBundleReviewName))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Observed outcome: **pass**", "Significant problems", "media-001.wav",
		"does not independently attest", "Deterministic benchmark scoring remains authoritative",
	} {
		if !bytes.Contains(review, []byte(required)) {
			t.Fatalf("REVIEW.md does not contain %q:\n%s", required, review)
		}
	}
	if !opened.Manifest.Complete || opened.Manifest.IndependentRemoteAttestation ||
		opened.Manifest.AuthenticityCaveat != evaluationBundleCaveat {
		t.Fatalf("manifest overstates provenance: %+v", opened.Manifest)
	}
}

func TestEvaluationBundleIsCreateOnly(t *testing.T) {
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	directory := filepath.Join(t.TempDir(), "evaluation")
	options := EvaluationBundleOptions{Directory: directory}
	first, err := WriteEvaluationBundle(t.Context(), options, evaluation)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(directory, evaluationBundleManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEvaluationBundle(t.Context(), options, evaluation); err == nil {
		t.Fatal("second WriteEvaluationBundle() replaced an existing final directory")
	}
	manifestAfter, err := os.ReadFile(filepath.Join(directory, evaluationBundleManifestName))
	if err != nil || !bytes.Equal(manifestBefore, manifestAfter) || digest(manifestAfter) != first.ManifestSHA256 {
		t.Fatalf("create-only manifest changed: err=%v", err)
	}

	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEvaluationBundle(t.Context(), EvaluationBundleOptions{Directory: existing}, evaluation); err == nil {
		t.Fatal("WriteEvaluationBundle() accepted an existing empty directory")
	}
	entries, err := os.ReadDir(existing)
	if err != nil || len(entries) != 0 {
		t.Fatalf("existing directory was changed: entries=%v err=%v", entries, err)
	}
}

func TestEvaluationBundleRejectsInvalidEvaluationMediaBeforeCreatingDirectory(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evaluation)
	}{
		{name: "missing", mutate: func(evaluation *Evaluation) {
			evaluation.Media = nil
		}},
		{name: "mutated bytes", mutate: func(evaluation *Evaluation) {
			evaluation.Media[0].Bytes = slices.Clone(evaluation.Media[0].Bytes)
			evaluation.Media[0].Bytes[len(evaluation.Media[0].Bytes)-1] ^= 1
		}},
		{name: "mutated metadata", mutate: func(evaluation *Evaluation) {
			evaluation.Media = append([]PreparedMedia(nil), evaluation.Media...)
			evaluation.Media[0].Role = "different_role"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation, _ := testEvaluationBundleEvaluation(t, nil)
			test.mutate(&evaluation)
			directory := filepath.Join(t.TempDir(), "evaluation")
			if _, err := WriteEvaluationBundle(t.Context(), EvaluationBundleOptions{
				Directory: directory,
			}, evaluation); err == nil {
				t.Fatal("WriteEvaluationBundle() accepted invalid reviewed media")
			}
			if _, err := os.Lstat(directory); !os.IsNotExist(err) {
				t.Fatalf("invalid Evaluation created a final directory: %v", err)
			}
		})
	}
}

func TestEvaluationBundleRejectsTamperingAndIncompleteTrees(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing", mutate: func(t *testing.T, directory string) {
			t.Helper()
			if err := os.Remove(filepath.Join(directory, "raw-response.bin")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra", mutate: func(t *testing.T, directory string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(directory, "extra.bin"), []byte("extra"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", mutate: func(t *testing.T, directory string) {
			t.Helper()
			path := filepath.Join(directory, "media-001.wav")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("record.json", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mutation", mutate: func(t *testing.T, directory string) {
			t.Helper()
			file, err := os.OpenFile(filepath.Join(directory, "provider-request.bin"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "noncanonical manifest", mutate: func(t *testing.T, directory string) {
			t.Helper()
			manifest := readTestEvaluationManifest(t, directory)
			payload, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeTestEvaluationFile(t, directory, evaluationBundleManifestName, payload)
		}},
		{name: "duplicate manifest key", mutate: func(t *testing.T, directory string) {
			t.Helper()
			path := filepath.Join(directory, evaluationBundleManifestName)
			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			payload = bytes.Replace(payload, []byte("{\n"), []byte("{\n  \"format\": \"duplicate\",\n"), 1)
			writeTestEvaluationFile(t, directory, evaluationBundleManifestName, payload)
		}},
		{name: "duplicate", mutate: func(t *testing.T, directory string) {
			t.Helper()
			manifest := readTestEvaluationManifest(t, directory)
			manifest.Files = append(manifest.Files, manifest.Files[0])
			manifest.FileSetSHA256 = digest(mustTestCanonical(t, manifest.Files))
			writeTestEvaluationManifest(t, directory, manifest)
		}},
		{name: "path traversal", mutate: func(t *testing.T, directory string) {
			t.Helper()
			manifest := readTestEvaluationManifest(t, directory)
			manifest.Files[0].Path = "../record.json"
			manifest.FileSetSHA256 = digest(mustTestCanonical(t, manifest.Files))
			writeTestEvaluationManifest(t, directory, manifest)
		}},
		{name: "incomplete", mutate: func(t *testing.T, directory string) {
			t.Helper()
			manifest := readTestEvaluationManifest(t, directory)
			manifest.Complete = false
			writeTestEvaluationManifest(t, directory, manifest)
		}},
		{name: "missing commit marker", mutate: func(t *testing.T, directory string) {
			t.Helper()
			if err := os.Remove(filepath.Join(directory, evaluationBundleManifestName)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation, _ := testEvaluationBundleEvaluation(t, nil)
			directory := filepath.Join(t.TempDir(), "evaluation")
			options := EvaluationBundleOptions{Directory: directory}
			if _, err := WriteEvaluationBundle(t.Context(), options, evaluation); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, directory)
			if _, err := OpenEvaluationBundle(t.Context(), options); err == nil {
				t.Fatal("OpenEvaluationBundle() accepted a tampered or incomplete tree")
			}
		})
	}
}

func TestEvaluationBundleRejectsSymlinkedAndNoncanonicalDirectories(t *testing.T) {
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	realParent := t.TempDir()
	directory := filepath.Join(realParent, "evaluation")
	options := EvaluationBundleOptions{Directory: directory}
	if _, err := WriteEvaluationBundle(t.Context(), options, evaluation); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "linked-evaluation")
	if err := os.Symlink(directory, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEvaluationBundle(t.Context(), EvaluationBundleOptions{
		Directory: symlink,
	}); err == nil {
		t.Fatal("OpenEvaluationBundle() accepted a symlink final directory")
	}

	linkedParentRoot := t.TempDir()
	linkedParent := filepath.Join(linkedParentRoot, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEvaluationBundle(t.Context(), EvaluationBundleOptions{
		Directory: filepath.Join(linkedParent, "evaluation"),
	}); err == nil {
		t.Fatal("OpenEvaluationBundle() accepted a symlink parent directory")
	}
	noncanonical := realParent + string(filepath.Separator) + "x" +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "evaluation"
	if _, err := OpenEvaluationBundle(t.Context(), EvaluationBundleOptions{
		Directory: noncanonical,
	}); err == nil {
		t.Fatal("OpenEvaluationBundle() accepted a traversal-bearing directory")
	}
}

func TestEvaluationBundleRejectsSensitiveLeakage(t *testing.T) {
	secret := "bundle-secret-value-12345"
	evaluation, _ := testEvaluationBundleEvaluation(t, []string{secret})
	directory := filepath.Join(t.TempDir(), "evaluation")
	options := EvaluationBundleOptions{Directory: directory, SensitiveValues: []string{secret}}
	if _, err := WriteEvaluationBundle(t.Context(), options, evaluation); err != nil {
		t.Fatal(err)
	}
	manifest := readTestEvaluationManifest(t, directory)
	reviewPath := filepath.Join(directory, evaluationBundleReviewName)
	review, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	review = append(review, []byte("\nInjected: "+secret+"\n")...)
	writeTestEvaluationFile(t, directory, evaluationBundleReviewName, review)
	for index := range manifest.Files {
		if manifest.Files[index].Purpose == "human_review" {
			manifest.Files[index].SHA256 = digest(review)
			manifest.Files[index].SizeBytes = int64(len(review))
		}
	}
	manifest.FileSetSHA256 = digest(mustTestCanonical(t, manifest.Files))
	writeTestEvaluationManifest(t, directory, manifest)
	if _, err := OpenEvaluationBundle(t.Context(), options); err == nil ||
		!strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), secret) {
		t.Fatalf("OpenEvaluationBundle() sensitive error = %v", err)
	}
	if _, err := OpenEvaluationBundle(t.Context(), EvaluationBundleOptions{
		Directory: directory,
	}); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("OpenEvaluationBundle() without required sensitive set = %v", err)
	}
}

func TestEvaluationBundleCancellationLeavesNoCompleteMarker(t *testing.T) {
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	directory := filepath.Join(t.TempDir(), "evaluation")
	ctx := newCancelWhenBundlePathExistsContext(t.Context(),
		filepath.Join(directory, "provider-configuration.json"))
	_, err := WriteEvaluationBundle(ctx, EvaluationBundleOptions{Directory: directory}, evaluation)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteEvaluationBundle() error = %v, want cancellation", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, evaluationBundleManifestName)); !os.IsNotExist(err) {
		t.Fatalf("canceled bundle exposed a manifest: %v", err)
	}
	if _, err := OpenEvaluationBundle(t.Context(), EvaluationBundleOptions{Directory: directory}); err == nil {
		t.Fatal("OpenEvaluationBundle() accepted a canceled partial tree")
	}
}

func TestEvaluationBundleConcurrentCreateAndReopen(t *testing.T) {
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	directory := filepath.Join(t.TempDir(), "evaluation")
	options := EvaluationBundleOptions{Directory: directory}
	const writers = 8
	results := make(chan error, writers)
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := WriteEvaluationBundle(t.Context(), options, evaluation)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent writers = %d, want 1", successes)
	}

	const readers = 16
	errorsFound := make(chan error, readers)
	for range readers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := OpenEvaluationBundle(t.Context(), options)
			errorsFound <- err
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent OpenEvaluationBundle() error = %v", err)
		}
	}
}

type cancelWhenBundlePathExistsContext struct {
	context.Context
	path string
	done chan struct{}
	once sync.Once
}

func newCancelWhenBundlePathExistsContext(
	parent context.Context, path string,
) *cancelWhenBundlePathExistsContext {
	return &cancelWhenBundlePathExistsContext{Context: parent, path: path, done: make(chan struct{})}
}

func (ctx *cancelWhenBundlePathExistsContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelWhenBundlePathExistsContext) Err() error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(ctx.path); err == nil {
		ctx.once.Do(func() { close(ctx.done) })
		return context.Canceled
	}
	return nil
}

func readTestEvaluationManifest(t *testing.T, directory string) EvaluationBundleManifest {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, evaluationBundleManifestName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeEvaluationBundleManifest(payload)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeTestEvaluationManifest(
	t *testing.T, directory string, manifest EvaluationBundleManifest,
) {
	t.Helper()
	payload, err := marshalCanonicalIndented(manifest, maximumEvaluationManifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEvaluationFile(t, directory, evaluationBundleManifestName, payload)
}

func writeTestEvaluationFile(t *testing.T, directory, name string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustTestCanonical(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := marshalCanonicalCompact(value, maximumEvaluationManifest)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
