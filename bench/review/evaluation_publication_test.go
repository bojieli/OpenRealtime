package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type evaluationPublicationTestStore struct {
	file FileEvaluationBundleReceiptStore

	mu                sync.Mutex
	acquires          int
	loads             int
	publishes         int
	failPublishBefore bool
	failPublishAfter  bool
}

type evaluationPublicationTestLease struct {
	store *evaluationPublicationTestStore
	inner EvaluationBundleReceiptLease
}

func (store *evaluationPublicationTestStore) Acquire(
	ctx context.Context,
) (EvaluationBundleReceiptLease, error) {
	store.mu.Lock()
	store.acquires++
	store.mu.Unlock()
	lease, err := store.file.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &evaluationPublicationTestLease{store: store, inner: lease}, nil
}

func (lease *evaluationPublicationTestLease) Publish(
	ctx context.Context, receipt EvaluationBundleReceipt,
) error {
	lease.store.mu.Lock()
	lease.store.publishes++
	failBefore, failAfter := lease.store.failPublishBefore, lease.store.failPublishAfter
	lease.store.mu.Unlock()
	if failBefore {
		return errors.New("injected pre-commit receipt failure")
	}
	if err := lease.inner.Publish(ctx, receipt); err != nil {
		return err
	}
	if failAfter {
		return errors.New("injected uncertain receipt close failure")
	}
	return nil
}

func (lease *evaluationPublicationTestLease) Load(
	ctx context.Context,
) (EvaluationBundleReceipt, bool, error) {
	lease.store.mu.Lock()
	lease.store.loads++
	lease.store.mu.Unlock()
	return lease.inner.Load(ctx)
}

func (lease *evaluationPublicationTestLease) Close() error { return lease.inner.Close() }

func testEvaluationPublicationConfig(
	t *testing.T,
) (EvaluationBundlePublicationConfig, *evaluationPublicationTestStore) {
	t.Helper()
	root := t.TempDir()
	publicationParent := filepath.Join(root, "reportable")
	receipts := filepath.Join(root, "receipts")
	quarantine := filepath.Join(root, "quarantine")
	for _, directory := range []string{publicationParent, receipts, quarantine} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store := &evaluationPublicationTestStore{file: FileEvaluationBundleReceiptStore{
		Path: filepath.Join(receipts, "evaluation.json"),
	}}
	return EvaluationBundlePublicationConfig{
		Bundle: EvaluationBundleOptions{
			Directory: filepath.Join(publicationParent, "evaluation"),
		},
		ReceiptStore: store, QuarantineDirectory: quarantine,
	}, store
}

func beginTestEvaluationPublication(
	t *testing.T, config EvaluationBundlePublicationConfig,
) (*EvaluationBundlePublication, EvaluationBundlePublicationPreparation) {
	t.Helper()
	publication, preparation, err := BeginEvaluationBundlePublication(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	return publication, preparation
}

func closeTestEvaluationPublication(t *testing.T, publication *EvaluationBundlePublication) {
	t.Helper()
	if err := publication.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluationPublicationStagesAnchorsAndPromotesInOrder(t *testing.T) {
	config, store := testEvaluationPublicationConfig(t)
	publication, preparation := beginTestEvaluationPublication(t, config)
	if preparation != (EvaluationBundlePublicationPreparation{}) {
		t.Fatalf("initial preparation = %+v", preparation)
	}
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	receipt, err := publication.Write(t.Context(), evaluation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEvaluationBundle(t.Context(), config.Bundle, receipt); err != nil {
		t.Fatal(err)
	}
	stage, err := evaluationBundleStageDirectory(config.Bundle.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains after promotion: %v", err)
	}
	store.mu.Lock()
	loads, publishes := store.loads, store.publishes
	store.mu.Unlock()
	if loads != 3 || publishes != 1 {
		t.Fatalf("store loads/publishes = %d/%d, want 3/1", loads, publishes)
	}
	closeTestEvaluationPublication(t, publication)

	recoveredPublication, recovered := beginTestEvaluationPublication(t, config)
	defer closeTestEvaluationPublication(t, recoveredPublication)
	if recovered.Recovered == nil || recovered.QuarantinedDirectory != "" ||
		!sameEvaluationBundleReceipt(recovered.Recovered.Receipt, receipt) {
		t.Fatalf("recovery = %+v, want exact final receipt", recovered)
	}
	if _, err := recoveredPublication.Write(t.Context(), Evaluation{}); err == nil {
		t.Fatal("recovered publication permitted another provider write")
	}
}

func TestEvaluationPublicationRecoversCrashesAroundPromotion(t *testing.T) {
	tests := []struct {
		name       string
		operations evaluationBundlePublicationOperations
	}{
		{
			name: "after durable receipt before rename",
			operations: evaluationBundlePublicationOperations{beforePromotion: func() error {
				return errors.New("injected pre-rename crash")
			}},
		},
		{
			name: "after rename before return",
			operations: evaluationBundlePublicationOperations{afterPromotion: func() error {
				return errors.New("injected post-rename crash")
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, _ := testEvaluationPublicationConfig(t)
			publication, _ := beginTestEvaluationPublication(t, config)
			evaluation, _ := testEvaluationBundleEvaluation(t, nil)
			if _, err := publication.writeOperations(
				t.Context(), evaluation, test.operations,
			); err == nil {
				t.Fatal("injected crash unexpectedly succeeded")
			}
			closeTestEvaluationPublication(t, publication)
			recovery, preparation := beginTestEvaluationPublication(t, config)
			defer closeTestEvaluationPublication(t, recovery)
			if preparation.Recovered == nil {
				t.Fatalf("receipt-anchored crash was not recovered: %+v", preparation)
			}
		})
	}
}

func TestEvaluationPublicationDoesNotInferDurabilityFromFailedPublish(t *testing.T) {
	config, store := testEvaluationPublicationConfig(t)
	store.failPublishAfter = true
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	if _, err := publication.Write(t.Context(), evaluation); err == nil {
		t.Fatal("publisher error authorized same-process promotion")
	}
	if _, err := os.Lstat(config.Bundle.Directory); !os.IsNotExist(err) {
		t.Fatalf("final became visible after publisher error: %v", err)
	}
	closeTestEvaluationPublication(t, publication)
	store.failPublishAfter = false
	recovery, preparation := beginTestEvaluationPublication(t, config)
	defer closeTestEvaluationPublication(t, recovery)
	if preparation.Recovered == nil {
		t.Fatalf("durable receipt was not recovered on a fresh lease: %+v", preparation)
	}
}

func TestEvaluationPublicationQuarantinesFailedPreReceiptStageOutsideReportableTree(t *testing.T) {
	config, store := testEvaluationPublicationConfig(t)
	store.failPublishBefore = true
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	if _, err := publication.Write(t.Context(), evaluation); err == nil {
		t.Fatal("pre-commit publisher failure unexpectedly succeeded")
	}
	closeTestEvaluationPublication(t, publication)
	store.failPublishBefore = false

	retry, preparation := beginTestEvaluationPublication(t, config)
	if preparation.Recovered != nil || preparation.QuarantinedDirectory == "" {
		t.Fatalf("preparation = %+v, want external quarantine", preparation)
	}
	if filepath.Dir(preparation.QuarantinedDirectory) != config.QuarantineDirectory {
		t.Fatalf("quarantine %q is not in caller-owned external root", preparation.QuarantinedDirectory)
	}
	if _, err := os.Lstat(preparation.QuarantinedDirectory); err != nil {
		t.Fatal(err)
	}
	newEvaluation, _ := testEvaluationBundleEvaluation(t, nil)
	if _, err := retry.Write(t.Context(), newEvaluation); err != nil {
		t.Fatalf("retry after quarantine: %v", err)
	}
	closeTestEvaluationPublication(t, retry)
}

func TestEvaluationPublicationLeaseExcludesConcurrentOwner(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	first, _ := beginTestEvaluationPublication(t, config)
	if second, _, err := BeginEvaluationBundlePublication(t.Context(), config); err == nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatal("second publication acquired an already-held lease")
	}
	closeTestEvaluationPublication(t, first)
	second, _ := beginTestEvaluationPublication(t, config)
	closeTestEvaluationPublication(t, second)
}

func TestEvaluationPublicationRejectsSensitiveFinalBeforePostProviderStoreCall(t *testing.T) {
	const secret = "publication-sensitive-directory"
	config, store := testEvaluationPublicationConfig(t)
	config.Bundle.Directory = filepath.Join(filepath.Dir(config.Bundle.Directory), secret)
	publication, _ := beginTestEvaluationPublication(t, config)
	store.mu.Lock()
	loadsBefore := store.loads
	store.mu.Unlock()
	evaluation, _ := testEvaluationBundleEvaluation(t, []string{secret})
	if _, err := publication.Write(t.Context(), evaluation); err == nil ||
		!strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("Write error = %v, want provider-owned sensitive guard rejection", err)
	}
	store.mu.Lock()
	loadsAfter := store.loads
	store.mu.Unlock()
	if loadsAfter != loadsBefore {
		t.Fatalf("rejected secret path reached post-provider store Load: %d -> %d", loadsBefore, loadsAfter)
	}
	closeTestEvaluationPublication(t, publication)
}

func TestEvaluationPromotionNeverReplacesConcurrentFinal(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	_, err := publication.writeOperations(
		t.Context(), evaluation,
		evaluationBundlePublicationOperations{beforePromotionRename: func() error {
			return os.Mkdir(config.Bundle.Directory, 0o700)
		}},
	)
	if err == nil {
		t.Fatal("exclusive promotion replaced a concurrent final")
	}
	entries, readErr := os.ReadDir(config.Bundle.Directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("concurrent final was not preserved exactly: entries=%v err=%v", entries, readErr)
	}
	stage, _ := evaluationBundleStageDirectory(config.Bundle.Directory)
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("source stage was lost after exclusive rename rejection: %v", err)
	}
	closeTestEvaluationPublication(t, publication)
}

func TestEvaluationPromotionRejectsVisibleParentSwap(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	parent := filepath.Dir(config.Bundle.Directory)
	moved := parent + "-moved"
	outside := filepath.Join(filepath.Dir(parent), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := publication.writeOperations(
		t.Context(), evaluation,
		evaluationBundlePublicationOperations{beforePromotionRename: func() error {
			if err := os.Rename(parent, moved); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		}},
	)
	if err == nil {
		t.Fatal("publication accepted a swapped visible parent")
	}
	if _, err := os.Lstat(filepath.Join(moved, filepath.Base(config.Bundle.Directory))); !os.IsNotExist(err) {
		t.Fatalf("publication renamed within the displaced parent: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("publication affected attacker-selected directory: entries=%v err=%v", entries, err)
	}
	closeTestEvaluationPublication(t, publication)
}

func TestEvaluationQuarantineNeverReplacesConcurrentDestination(t *testing.T) {
	config, store := testEvaluationPublicationConfig(t)
	store.failPublishBefore = true
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	if _, err := publication.Write(t.Context(), evaluation); err == nil {
		t.Fatal("pre-commit failure unexpectedly succeeded")
	}
	closeTestEvaluationPublication(t, publication)
	store.failPublishBefore = false
	candidate := filepath.Join(config.QuarantineDirectory, "openrealtime-evaluation-abandoned-"+
		evaluationBundlePublicationIdentity(config.Bundle.Directory)+"-000001")
	_, _, err := beginEvaluationBundlePublicationOperations(
		t.Context(), config,
		evaluationBundlePublicationOperations{beforeQuarantineRename: func() error {
			return os.Mkdir(candidate, 0o700)
		}},
	)
	if err == nil {
		t.Fatal("exclusive quarantine replaced a concurrent destination")
	}
	entries, readErr := os.ReadDir(candidate)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("concurrent quarantine destination changed: entries=%v err=%v", entries, readErr)
	}
	stage, _ := evaluationBundleStageDirectory(config.Bundle.Directory)
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("stage was lost after quarantine collision: %v", err)
	}
}

func TestEvaluationPromotionReverifiesMutationBeforeRename(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	publication, _ := beginTestEvaluationPublication(t, config)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	stage, _ := evaluationBundleStageDirectory(config.Bundle.Directory)
	_, err := publication.writeOperations(
		t.Context(), evaluation,
		evaluationBundlePublicationOperations{afterStageVerification: func() error {
			path := filepath.Join(stage, "raw-response.bin")
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			payload[len(payload)-1] ^= 1
			return os.WriteFile(path, payload, 0o600)
		}},
	)
	if err == nil {
		t.Fatal("mutated stage became reportable")
	}
	if _, err := os.Lstat(config.Bundle.Directory); !os.IsNotExist(err) {
		t.Fatalf("mutated stage obtained final name: %v", err)
	}
	closeTestEvaluationPublication(t, publication)
}

func TestEvaluationPublicationRejectsUnanchoredFinalAndTypedNilStore(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	evaluation, _ := testEvaluationBundleEvaluation(t, nil)
	if _, err := WriteEvaluationBundle(t.Context(), config.Bundle, evaluation); err != nil {
		t.Fatal(err)
	}
	if publication, _, err := BeginEvaluationBundlePublication(t.Context(), config); err == nil {
		if publication != nil {
			_ = publication.Close()
		}
		t.Fatal("final without durable receipt was accepted")
	}

	other, _ := testEvaluationPublicationConfig(t)
	var store *evaluationPublicationTestStore
	other.ReceiptStore = store
	if publication, _, err := BeginEvaluationBundlePublication(t.Context(), other); err == nil {
		if publication != nil {
			_ = publication.Close()
		}
		t.Fatal("typed-nil receipt store was accepted")
	}
}

func TestFileEvaluationReceiptStoreRejectsSymlinkAncestorAndHardLinkedLock(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store := FileEvaluationBundleReceiptStore{Path: filepath.Join(linkedParent, "receipt.json")}
	if lease, err := store.Acquire(t.Context()); err == nil {
		_ = lease.Close()
		t.Fatal("receipt store accepted a symlinked ancestor")
	}

	config, _ := testEvaluationPublicationConfig(t)
	fileStore := config.ReceiptStore.(*evaluationPublicationTestStore).file
	lease, err := fileStore.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := fileStore.Path + ".publication.lock"
	if err := os.Link(lockPath, filepath.Join(t.TempDir(), "outside-lock")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if lease, err := fileStore.Acquire(t.Context()); err == nil {
		_ = lease.Close()
		t.Fatal("receipt store accepted a hard-linked lock marker")
	}
}

func TestEvaluationPublicationPreparationReturnsExactRecoveredValue(t *testing.T) {
	config, _ := testEvaluationPublicationConfig(t)
	publication, preparation := beginTestEvaluationPublication(t, config)
	if !reflect.DeepEqual(preparation, EvaluationBundlePublicationPreparation{}) {
		t.Fatalf("empty preparation = %+v", preparation)
	}
	closeTestEvaluationPublication(t, publication)
}
