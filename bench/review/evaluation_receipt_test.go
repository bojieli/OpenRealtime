package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExternalEvaluationReceiptIsCreateOnlyCanonicalAndStrictlyReopened(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "evaluation")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := evaluationReceiptFixture(t, directory)
	path := filepath.Join(parent, "evaluation.receipt.json")
	if err := WriteEvaluationBundleReceipt(context.Background(), path, receipt); err != nil {
		t.Fatal(err)
	}
	opened, err := ReadEvaluationBundleReceipt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened, receipt) {
		t.Fatalf("opened receipt = %+v, want %+v", opened, receipt)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("receipt permissions = %04o", info.Mode().Perm())
	}
	if err := WriteEvaluationBundleReceipt(context.Background(), path, receipt); err == nil {
		t.Fatal("create-only evaluation receipt was overwritten")
	}
	if err := WriteEvaluationBundleReceipt(
		context.Background(), filepath.Join(directory, "inside.receipt.json"), receipt,
	); err == nil {
		t.Fatal("evaluation receipt was accepted inside its mutable bundle")
	}
}

func TestExternalEvaluationReceiptRejectsTamperingAndSymlinkedParent(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "evaluation")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := evaluationReceiptFixture(t, directory)
	path := filepath.Join(parent, "evaluation.receipt.json")
	if err := WriteEvaluationBundleReceipt(context.Background(), path, receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEvaluationBundleReceipt(context.Background(), path); err == nil {
		t.Fatal("tampered evaluation receipt unexpectedly reopened")
	}
	realParent := filepath.Join(parent, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "parent-alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteEvaluationBundleReceipt(
		context.Background(), filepath.Join(alias, "receipt.json"), receipt,
	); err == nil {
		t.Fatal("evaluation receipt accepted a symlinked parent")
	}
}

func evaluationReceiptFixture(t testing.TB, directory string) EvaluationBundleReceipt {
	t.Helper()
	receipt := EvaluationBundleReceipt{
		Directory:      directory,
		ManifestSHA256: digest([]byte("manifest")),
		RecordSHA256:   digest([]byte("record")),
		FileSetSHA256:  digest([]byte("files")),
	}
	var err error
	receipt.ReceiptSHA256, err = evaluationBundleReceiptDigest(
		receipt.ManifestSHA256, receipt.RecordSHA256, receipt.FileSetSHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
