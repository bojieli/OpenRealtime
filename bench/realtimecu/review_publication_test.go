package realtimecu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewBundlePublicationIsCreateOnlyAndVerifiesIncompleteDiagnostic(t *testing.T) {
	directory, receipt := fixtureFinishedReviewedBundle(t)
	receiptPath := directory + ".receipt.json"
	if err := WriteReviewBundleReceipt(t.Context(), receiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewBundleReceipt(t.Context(), receiptPath, receipt); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create-only final receipt rewrite error = %v", err)
	}
	after, err := os.ReadFile(receiptPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("create-only final receipt changed: error=%v", err)
	}
	opened, err := ReadReviewBundleReceipt(t.Context(), receiptPath)
	if err != nil || opened != receipt {
		t.Fatalf("opened final receipt=%+v error=%v", opened, err)
	}
	verified, err := VerifyReviewBundlePublication(t.Context(), ReviewBundleVerificationOptions{
		Directory: directory, ReceiptPath: receiptPath,
		SourceReceiptPath:          fixtureReviewSourceReceiptPath(directory),
		EvaluationReceiptDirectory: fixtureReviewEvaluationReceiptDirectory(directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.Receipt != receipt || len(verified.Manifest.Attempts) != 1 ||
		len(verified.EvaluationReceipts) != 1 || verified.EvidenceComplete {
		t.Fatalf("diagnostic verification was upgraded: %+v", verified)
	}
}

func TestPublishReviewBundleReceiptAnchorsPreviouslySealedBundleWithoutProvider(t *testing.T) {
	directory, receipt := fixtureFinishedReviewedBundle(t)
	options := ReviewBundleVerificationOptions{
		Directory: directory, ReceiptPath: directory + ".receipt.json",
		SourceReceiptPath:          fixtureReviewSourceReceiptPath(directory),
		EvaluationReceiptDirectory: fixtureReviewEvaluationReceiptDirectory(directory),
	}
	verified, err := PublishReviewBundleReceipt(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Receipt != receipt || verified.EvidenceComplete ||
		len(verified.EvaluationReceipts) != 1 {
		t.Fatalf("anchored diagnostic publication=%+v", verified)
	}
	if _, err := PublishReviewBundleReceipt(t.Context(), options); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("migration publication was not create-only: %v", err)
	}
	reopened, err := VerifyReviewBundlePublication(t.Context(), options)
	if err != nil || reopened.Receipt != receipt || reopened.EvidenceComplete {
		t.Fatalf("reopened diagnostic publication=%+v error=%v", reopened, err)
	}
}

func TestReviewBundlePublicationRejectsExternalReceiptSetDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, *ReviewBundleVerificationOptions)
	}{
		{
			name: "missing evaluation receipt",
			mutate: func(t *testing.T, directory string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				path := onlyExternalEvaluationReceipt(t, options.EvaluationReceiptDirectory)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "extra evaluation artifact",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				if err := os.WriteFile(
					filepath.Join(options.EvaluationReceiptDirectory, "unexpected.receipt.json"),
					[]byte("{}\n"), 0o400,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tampered evaluation receipt",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				path := onlyExternalEvaluationReceipt(t, options.EvaluationReceiptDirectory)
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different source receipt",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				otherDirectory, otherReceipt := fixtureFinishedReviewedBundle(t)
				otherReceiptPath := otherDirectory + ".receipt.json"
				if err := WriteReviewBundleReceipt(t.Context(), otherReceiptPath, otherReceipt); err != nil {
					t.Fatal(err)
				}
				options.SourceReceiptPath = fixtureReviewSourceReceiptPath(otherDirectory)
			},
		},
		{
			name: "different evaluation receipts",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				otherDirectory, otherReceipt := fixtureFinishedReviewedBundle(t)
				otherReceiptPath := otherDirectory + ".receipt.json"
				if err := WriteReviewBundleReceipt(t.Context(), otherReceiptPath, otherReceipt); err != nil {
					t.Fatal(err)
				}
				options.EvaluationReceiptDirectory = fixtureReviewEvaluationReceiptDirectory(otherDirectory)
			},
		},
		{
			name: "symlinked evaluation directory",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				alias := filepath.Join(filepath.Dir(options.Directory), "evaluation-alias")
				if err := os.Symlink(options.EvaluationReceiptDirectory, alias); err != nil {
					t.Fatal(err)
				}
				options.EvaluationReceiptDirectory = alias
			},
		},
		{
			name: "hardlinked final receipt",
			mutate: func(t *testing.T, _ string, options *ReviewBundleVerificationOptions) {
				t.Helper()
				if err := os.Link(options.ReceiptPath, options.ReceiptPath+".alias"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory, receipt := fixtureFinishedReviewedBundle(t)
			options := ReviewBundleVerificationOptions{
				Directory: directory, ReceiptPath: directory + ".receipt.json",
				SourceReceiptPath:          fixtureReviewSourceReceiptPath(directory),
				EvaluationReceiptDirectory: fixtureReviewEvaluationReceiptDirectory(directory),
			}
			if err := WriteReviewBundleReceipt(t.Context(), options.ReceiptPath, receipt); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, directory, &options)
			if _, err := VerifyReviewBundlePublication(
				context.Background(), options,
			); err == nil {
				t.Fatal("publication verifier accepted drifted external evidence")
			}
		})
	}
}

func TestReviewBundleReceiptRejectsInsideTreeAndSymlinkPath(t *testing.T) {
	directory, receipt := fixtureFinishedReviewBundle(t)
	inside := filepath.Join(directory, "receipt.json")
	if err := WriteReviewBundleReceipt(t.Context(), inside, receipt); err == nil ||
		!strings.Contains(err.Error(), "outside") {
		t.Fatalf("inside-tree receipt error = %v", err)
	}
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewBundleReceipt(
		t.Context(), filepath.Join(aliasParent, "receipt.json"), receipt,
	); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("symlinked receipt parent error = %v", err)
	}
}

func onlyExternalEvaluationReceipt(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".receipt.json") {
			if result != "" {
				t.Fatal("fixture has more than one external evaluation receipt")
			}
			result = filepath.Join(directory, entry.Name())
		}
	}
	if result == "" {
		t.Fatal("fixture has no external evaluation receipt")
	}
	return result
}
