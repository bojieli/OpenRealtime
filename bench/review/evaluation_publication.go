package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

const maximumAbandonedEvaluationStages = 10_000

// EvaluationBundleReceiptStore is scoped to exactly one publication target and
// acquires its exclusive, crash-released lease without receiving the final
// directory. The store does not see that directory until Publication.Write has
// checked it against the provider-owned sensitive-value guard.
type EvaluationBundleReceiptStore interface {
	Acquire(context.Context) (EvaluationBundleReceiptLease, error)
}

// EvaluationBundleReceiptLease is caller-owned durable receipt storage held
// exclusively from pre-provider recovery through final publication. Publish
// is create-only and a nil return guarantees durable commit. Load returns
// found=false only when no receipt has been durably committed.
type EvaluationBundleReceiptLease interface {
	Publish(context.Context, EvaluationBundleReceipt) error
	Load(context.Context) (EvaluationBundleReceipt, bool, error)
	Close() error
}

// FileEvaluationBundleReceiptStore is the standard filesystem-backed store.
// Path must be an absolute canonical file outside the final evaluation tree.
// Acquire holds an operating-system file lock whose descriptor is released by
// Close or by process exit; the lock marker itself is deliberately persistent.
type FileEvaluationBundleReceiptStore struct {
	Path string
}

type fileEvaluationBundleReceiptLease struct {
	path   string
	lock   *os.File
	mu     sync.Mutex
	closed bool
}

func (store FileEvaluationBundleReceiptStore) Acquire(
	ctx context.Context,
) (EvaluationBundleReceiptLease, error) {
	if ctx == nil {
		return nil, errors.New("acquire evaluation receipt store: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFileEvaluationReceiptPath(store.Path); err != nil {
		return nil, err
	}
	lock, err := acquireEvaluationReceiptFileLock(ctx, store.Path+".publication.lock")
	if err != nil {
		return nil, err
	}
	return &fileEvaluationBundleReceiptLease{path: store.Path, lock: lock}, nil
}

func (store FileEvaluationBundleReceiptStore) validateForBundle(directory string) error {
	_, err := os.Lstat(store.Path)
	mustExist := err == nil
	if err != nil && !os.IsNotExist(err) {
		return errors.New("inspect evaluation receipt store path")
	}
	return validateExternalEvaluationReceiptPath(store.Path, directory, mustExist)
}

func (lease *fileEvaluationBundleReceiptLease) Publish(
	ctx context.Context, receipt EvaluationBundleReceipt,
) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.lock == nil {
		return errors.New("evaluation receipt lease is closed")
	}
	return writeEvaluationBundleReceipt(ctx, lease.path, receipt, false)
}

func (lease *fileEvaluationBundleReceiptLease) Load(
	ctx context.Context,
) (EvaluationBundleReceipt, bool, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if ctx == nil {
		return EvaluationBundleReceipt{}, false, errors.New("load evaluation receipt lease: nil context")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundleReceipt{}, false, err
	}
	if lease.closed || lease.lock == nil {
		return EvaluationBundleReceipt{}, false, errors.New("evaluation receipt lease is closed")
	}
	info, err := os.Lstat(lease.path)
	if os.IsNotExist(err) {
		if err := validateExternalEvaluationReceiptPath(lease.path, "", false); err != nil {
			return EvaluationBundleReceipt{}, false, err
		}
		return EvaluationBundleReceipt{}, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return EvaluationBundleReceipt{}, false,
			errors.New("evaluation receipt store path is not a regular file")
	}
	if err := validateExternalEvaluationReceiptPath(lease.path, "", true); err != nil {
		return EvaluationBundleReceipt{}, false, err
	}
	receipt, err := ReadEvaluationBundleReceipt(ctx, lease.path)
	if err != nil {
		return EvaluationBundleReceipt{}, false, err
	}
	return receipt, true, nil
}

func (lease *fileEvaluationBundleReceiptLease) Close() error {
	if lease == nil {
		return errors.New("close evaluation receipt lease: nil lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	lease.closed = true
	if lease.lock == nil {
		return errors.New("close evaluation receipt lease: missing lock")
	}
	err := lease.lock.Close()
	lease.lock = nil
	if err != nil {
		return errors.New("close evaluation receipt lease lock")
	}
	return nil
}

// EvaluationBundlePublicationConfig binds one final bundle, one durable
// receipt store, and a disjoint caller-owned quarantine directory. Quarantine
// is intentionally outside the reportable bundle tree so exact-tree verifiers
// never mistake preserved pre-receipt crash debris for evidence.
type EvaluationBundlePublicationConfig struct {
	Bundle              EvaluationBundleOptions
	ReceiptStore        EvaluationBundleReceiptStore
	QuarantineDirectory string
}

// EvaluationBundlePublicationPreparation reports restart recovery before a
// caller invokes a review provider. A non-nil Recovered means no provider call
// is needed. QuarantinedDirectory is preserved but permanently non-reportable.
type EvaluationBundlePublicationPreparation struct {
	Recovered            *EvaluationBundle
	QuarantinedDirectory string
}

// EvaluationBundlePublication holds exclusive ownership from preparation,
// across the provider call, through receipt commit and atomic promotion.
type EvaluationBundlePublication struct {
	config EvaluationBundlePublicationConfig
	final  string
	stage  string
	lease  EvaluationBundleReceiptLease

	mu             sync.Mutex
	closed         bool
	writePermitted bool
	writeAttempted bool
}

// BeginEvaluationBundlePublication acquires an exclusive crash-released lease,
// recovers a receipt-anchored final/stage, or quarantines an unanchored stage.
// The returned publication must remain open while the provider is invoked and
// must always be closed.
func BeginEvaluationBundlePublication(
	ctx context.Context, config EvaluationBundlePublicationConfig,
) (*EvaluationBundlePublication, EvaluationBundlePublicationPreparation, error) {
	return beginEvaluationBundlePublicationOperations(
		ctx, config, evaluationBundlePublicationOperations{},
	)
}

type evaluationBundlePublicationOperations struct {
	beforePromotion        func() error
	afterStageVerification func() error
	beforePromotionRename  func() error
	afterPromotion         func() error
	beforeQuarantineRename func() error
}

func beginEvaluationBundlePublicationOperations(
	ctx context.Context, config EvaluationBundlePublicationConfig,
	operations evaluationBundlePublicationOperations,
) (*EvaluationBundlePublication, EvaluationBundlePublicationPreparation, error) {
	if ctx == nil {
		return nil, EvaluationBundlePublicationPreparation{},
			errors.New("begin evaluation publication: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, EvaluationBundlePublicationPreparation{}, err
	}
	if nilEvaluationReceiptStore(config.ReceiptStore) {
		return nil, EvaluationBundlePublicationPreparation{},
			errors.New("begin evaluation publication: nil receipt store")
	}
	finalDirectory, stageDirectory, err := evaluationPublicationDirectories(ctx, config.Bundle)
	if err != nil {
		return nil, EvaluationBundlePublicationPreparation{}, err
	}
	if err := validateEvaluationQuarantineDirectory(
		config.QuarantineDirectory, filepath.Dir(finalDirectory),
	); err != nil {
		return nil, EvaluationBundlePublicationPreparation{}, err
	}
	if validator, ok := config.ReceiptStore.(interface{ validateForBundle(string) error }); ok {
		if err := validator.validateForBundle(finalDirectory); err != nil {
			return nil, EvaluationBundlePublicationPreparation{}, err
		}
	}
	lease, err := config.ReceiptStore.Acquire(ctx)
	if err != nil {
		return nil, EvaluationBundlePublicationPreparation{},
			fmt.Errorf("acquire evaluation publication lease: %w", err)
	}
	if nilEvaluationReceiptLease(lease) {
		return nil, EvaluationBundlePublicationPreparation{},
			errors.Join(errors.New("receipt store returned a nil lease"), leaseCloseError(lease))
	}
	publication := &EvaluationBundlePublication{
		config: config, final: finalDirectory, stage: stageDirectory, lease: lease,
	}
	preparation, err := publication.prepare(ctx, operations)
	if err != nil {
		closeErr := publication.Close()
		return nil, EvaluationBundlePublicationPreparation{}, errors.Join(err, closeErr)
	}
	publication.writePermitted = preparation.Recovered == nil
	return publication, preparation, nil
}

func (publication *EvaluationBundlePublication) prepare(
	ctx context.Context, operations evaluationBundlePublicationOperations,
) (EvaluationBundlePublicationPreparation, error) {
	receipt, found, err := publication.lease.Load(ctx)
	if err != nil {
		return EvaluationBundlePublicationPreparation{},
			fmt.Errorf("load evaluation publication receipt: %w", err)
	}
	if found && receipt.Directory != publication.final {
		return EvaluationBundlePublicationPreparation{},
			errors.New("evaluation receipt names a different final directory")
	}
	finalExists, err := evaluationPublicationDirectoryExists(publication.final)
	if err != nil {
		return EvaluationBundlePublicationPreparation{}, err
	}
	stageExists, err := evaluationPublicationDirectoryExists(publication.stage)
	if err != nil {
		return EvaluationBundlePublicationPreparation{}, err
	}
	if found {
		return recoverEvaluationBundlePublication(
			ctx, publication.config.Bundle, receipt, finalExists, stageExists, operations,
		)
	}
	if finalExists {
		return EvaluationBundlePublicationPreparation{},
			errors.New("final evaluation bundle exists without a durable receipt")
	}
	if !stageExists {
		return EvaluationBundlePublicationPreparation{}, nil
	}
	quarantined, err := quarantineEvaluationBundleStage(
		ctx, publication.final, publication.stage,
		publication.config.QuarantineDirectory, operations,
	)
	if err != nil {
		return EvaluationBundlePublicationPreparation{}, err
	}
	return EvaluationBundlePublicationPreparation{QuarantinedDirectory: quarantined}, nil
}

// Write publishes exactly one provider-verified evaluation through this held
// lease. It commits and reopens the durable external receipt before the final
// name becomes visible. A Publish error never authorizes promotion.
func (publication *EvaluationBundlePublication) Write(
	ctx context.Context, source Evaluation,
) (EvaluationBundleReceipt, error) {
	return publication.writeOperations(ctx, source, evaluationBundlePublicationOperations{})
}

func (publication *EvaluationBundlePublication) writeOperations(
	ctx context.Context, source Evaluation, operations evaluationBundlePublicationOperations,
) (EvaluationBundleReceipt, error) {
	if publication == nil {
		return EvaluationBundleReceipt{}, errors.New("write evaluation publication: nil publication")
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if ctx == nil {
		return EvaluationBundleReceipt{}, errors.New("write evaluation publication: nil context")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	if publication.closed || publication.lease == nil {
		return EvaluationBundleReceipt{}, errors.New("evaluation publication is closed")
	}
	if !publication.writePermitted || publication.writeAttempted {
		return EvaluationBundleReceipt{}, errors.New("evaluation publication does not permit a provider write")
	}
	// This opaque guard check must precede every receipt-store operation that
	// follows provider evaluation. Acquire received no publication path.
	if err := validateEvaluationPublicationSource(ctx, source, publication.final); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	publication.writeAttempted = true
	if _, found, err := publication.lease.Load(ctx); err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("inspect held evaluation receipt lease: %w", err)
	} else if found {
		return EvaluationBundleReceipt{}, errors.New("evaluation receipt appeared during an exclusive publication")
	}
	if exists, err := evaluationPublicationDirectoryExists(publication.final); err != nil {
		return EvaluationBundleReceipt{}, err
	} else if exists {
		return EvaluationBundleReceipt{}, errors.New("final evaluation directory appeared during publication")
	}
	if exists, err := evaluationPublicationDirectoryExists(publication.stage); err != nil {
		return EvaluationBundleReceipt{}, err
	} else if exists {
		return EvaluationBundleReceipt{}, errors.New("evaluation staging directory requires preparation")
	}
	stageOptions := publication.config.Bundle
	stageOptions.Directory = publication.stage
	receipt, err := writeEvaluationBundleWithOperations(
		ctx, stageOptions, source,
		evaluationBundleWriteOperations{receiptDirectory: publication.final},
	)
	if err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("retain staged evaluation bundle: %w", err)
	}
	if receipt.Directory != publication.final {
		return EvaluationBundleReceipt{}, errors.New("staged evaluation receipt changed its final directory")
	}
	if _, err := VerifyEvaluationBundle(ctx, stageOptions, receipt); err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("verify staged evaluation bundle: %w", err)
	}
	if err := publication.lease.Publish(ctx, receipt); err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("durably publish evaluation receipt: %w", err)
	}
	retained, found, err := publication.lease.Load(ctx)
	if err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("reopen durable evaluation receipt: %w", err)
	}
	if !found || retained.Directory != publication.final ||
		!sameEvaluationBundleReceipt(retained, receipt) {
		return EvaluationBundleReceipt{},
			errors.New("durable evaluation receipt did not reopen exactly")
	}
	opened, err := promoteEvaluationBundleStage(
		ctx, publication.config.Bundle, publication.stage, retained,
		operations,
	)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	publication.writePermitted = false
	return opened.Receipt, nil
}

// Close releases the publication lease. It is idempotent.
func (publication *EvaluationBundlePublication) Close() error {
	if publication == nil {
		return errors.New("close evaluation publication: nil publication")
	}
	publication.mu.Lock()
	defer publication.mu.Unlock()
	if publication.closed {
		return nil
	}
	publication.closed = true
	lease := publication.lease
	publication.lease = nil
	return leaseCloseError(lease)
}

func recoverEvaluationBundlePublication(
	ctx context.Context, options EvaluationBundleOptions,
	receipt EvaluationBundleReceipt, finalExists, stageExists bool,
	operations evaluationBundlePublicationOperations,
) (EvaluationBundlePublicationPreparation, error) {
	if receipt.Directory != options.Directory {
		return EvaluationBundlePublicationPreparation{},
			errors.New("evaluation receipt recovery target differs from the final directory")
	}
	if finalExists && stageExists {
		return EvaluationBundlePublicationPreparation{},
			errors.New("final and staged evaluation directories both exist")
	}
	if finalExists {
		opened, err := VerifyEvaluationBundle(ctx, options, receipt)
		if err != nil {
			return EvaluationBundlePublicationPreparation{},
				fmt.Errorf("verify final evaluation bundle during recovery: %w", err)
		}
		return EvaluationBundlePublicationPreparation{Recovered: &opened}, nil
	}
	if !stageExists {
		return EvaluationBundlePublicationPreparation{},
			errors.New("evaluation receipt exists without its final or staged bundle")
	}
	stageDirectory, err := evaluationBundleStageDirectory(options.Directory)
	if err != nil {
		return EvaluationBundlePublicationPreparation{}, err
	}
	opened, err := promoteEvaluationBundleStage(ctx, options, stageDirectory, receipt, operations)
	if err != nil {
		return EvaluationBundlePublicationPreparation{},
			fmt.Errorf("recover staged evaluation bundle: %w", err)
	}
	return EvaluationBundlePublicationPreparation{Recovered: &opened}, nil
}

func promoteEvaluationBundleStage(
	ctx context.Context, finalOptions EvaluationBundleOptions, stageDirectory string,
	expected EvaluationBundleReceipt, operations evaluationBundlePublicationOperations,
) (result EvaluationBundle, resultErr error) {
	if err := ctx.Err(); err != nil {
		return EvaluationBundle{}, err
	}
	if expected.Directory != finalOptions.Directory {
		return EvaluationBundle{}, errors.New("staged evaluation receipt names a different final directory")
	}
	stageOptions := finalOptions
	stageOptions.Directory = stageDirectory
	if _, err := VerifyEvaluationBundle(ctx, stageOptions, expected); err != nil {
		return EvaluationBundle{}, fmt.Errorf("verify evaluation stage before promotion: %w", err)
	}
	if operations.afterStageVerification != nil {
		if err := operations.afterStageVerification(); err != nil {
			return EvaluationBundle{}, errors.New("evaluation stage verification hook failed")
		}
		if _, err := VerifyEvaluationBundle(ctx, stageOptions, expected); err != nil {
			return EvaluationBundle{}, fmt.Errorf("evaluation stage changed after verification: %w", err)
		}
	}
	parentPath := filepath.Dir(finalOptions.Directory)
	if filepath.Dir(stageDirectory) != parentPath {
		return EvaluationBundle{}, errors.New("evaluation stage is not a final-directory sibling")
	}
	if err := validateDirectoryAncestorChain(parentPath); err != nil {
		return EvaluationBundle{}, err
	}
	parent, err := openValidatedRoot(parentPath, nil)
	if err != nil {
		return EvaluationBundle{}, errors.New("open evaluation publication parent")
	}
	defer func() {
		if err := parent.Close(); err != nil {
			result = EvaluationBundle{}
			resultErr = errors.Join(resultErr, errors.New("close evaluation publication parent"))
		}
	}()
	parentInfo, err := parent.Stat(".")
	if err != nil {
		return EvaluationBundle{}, errors.New("inspect anchored evaluation publication parent")
	}
	stageBase, finalBase := filepath.Base(stageDirectory), filepath.Base(finalOptions.Directory)
	stageInfo, err := parent.Lstat(stageBase)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() {
		return EvaluationBundle{}, errors.New("evaluation stage is not a non-symlink directory")
	}
	stageRoot, err := parent.OpenRoot(stageBase)
	if err != nil {
		return EvaluationBundle{}, errors.New("open anchored evaluation stage")
	}
	defer stageRoot.Close()
	openedStage, err := stageRoot.Stat(".")
	if err != nil || !os.SameFile(stageInfo, openedStage) {
		return EvaluationBundle{}, errors.New("evaluation stage changed while anchoring")
	}
	if _, err := parent.Lstat(finalBase); err == nil {
		return EvaluationBundle{}, errors.New("final evaluation directory appeared before promotion")
	} else if !os.IsNotExist(err) {
		return EvaluationBundle{}, errors.New("inspect final evaluation directory before promotion")
	}
	if operations.beforePromotion != nil {
		if err := operations.beforePromotion(); err != nil {
			return EvaluationBundle{}, errors.New("evaluation promotion interrupted before rename")
		}
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundle{}, err
	}
	stageNow, stageErr := parent.Lstat(stageBase)
	_, finalErr := parent.Lstat(finalBase)
	visibleParent, visibleParentErr := os.Lstat(parentPath)
	if stageErr != nil || stageNow.Mode()&os.ModeSymlink != 0 || !stageNow.IsDir() ||
		!os.SameFile(openedStage, stageNow) || !os.IsNotExist(finalErr) ||
		visibleParentErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentInfo, visibleParent) {
		return EvaluationBundle{}, errors.New("evaluation publication names changed before promotion")
	}
	if operations.beforePromotionRename != nil {
		if err := operations.beforePromotionRename(); err != nil {
			return EvaluationBundle{}, errors.New("prepare exclusive evaluation promotion")
		}
	}
	if err := renameDirectoryNoReplace(
		parentPath, stageBase, parentInfo, parentPath, finalBase, parentInfo,
	); err != nil {
		return EvaluationBundle{}, fmt.Errorf("promote evaluation stage exclusively: %w", err)
	}
	if err := syncEvaluationBundleDirectory(parent); err != nil {
		return EvaluationBundle{}, err
	}
	finalInfo, finalInfoErr := parent.Lstat(finalBase)
	_, stageErr = parent.Lstat(stageBase)
	visibleParent, visibleParentErr = os.Lstat(parentPath)
	if finalInfoErr != nil || finalInfo.Mode()&os.ModeSymlink != 0 || !finalInfo.IsDir() ||
		!os.SameFile(openedStage, finalInfo) || !os.IsNotExist(stageErr) ||
		visibleParentErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentInfo, visibleParent) {
		return EvaluationBundle{}, errors.New("evaluation identity changed during promotion")
	}
	if operations.afterPromotion != nil {
		if err := operations.afterPromotion(); err != nil {
			return EvaluationBundle{}, errors.New("evaluation promotion interrupted after rename")
		}
	}
	opened, err := VerifyEvaluationBundle(ctx, finalOptions, expected)
	if err != nil {
		return EvaluationBundle{}, fmt.Errorf("verify promoted evaluation bundle: %w", err)
	}
	return opened, nil
}

func quarantineEvaluationBundleStage(
	ctx context.Context, finalDirectory, stageDirectory, quarantineDirectory string,
	operations evaluationBundlePublicationOperations,
) (result string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sourceParentPath := filepath.Dir(finalDirectory)
	if filepath.Dir(stageDirectory) != sourceParentPath {
		return "", errors.New("evaluation quarantine source is not a final-directory sibling")
	}
	if err := validateEvaluationQuarantineDirectory(quarantineDirectory, sourceParentPath); err != nil {
		return "", err
	}
	sourceParent, err := openValidatedRoot(sourceParentPath, nil)
	if err != nil {
		return "", errors.New("open evaluation quarantine source parent")
	}
	defer func() {
		if err := sourceParent.Close(); err != nil {
			result = ""
			resultErr = errors.Join(resultErr, errors.New("close evaluation quarantine source parent"))
		}
	}()
	destination, err := openValidatedRoot(quarantineDirectory, nil)
	if err != nil {
		return "", errors.New("open evaluation quarantine destination")
	}
	defer func() {
		if err := destination.Close(); err != nil {
			result = ""
			resultErr = errors.Join(resultErr, errors.New("close evaluation quarantine destination"))
		}
	}()
	sourceParentInfo, err := sourceParent.Stat(".")
	if err != nil {
		return "", errors.New("inspect evaluation quarantine source parent")
	}
	destinationInfo, err := destination.Stat(".")
	if err != nil {
		return "", errors.New("inspect evaluation quarantine destination")
	}
	stageBase, finalBase := filepath.Base(stageDirectory), filepath.Base(finalDirectory)
	if _, err := sourceParent.Lstat(finalBase); err == nil {
		return "", errors.New("cannot quarantine a stage beside an existing final evaluation")
	} else if !os.IsNotExist(err) {
		return "", errors.New("inspect final evaluation before quarantine")
	}
	stageInfo, err := sourceParent.Lstat(stageBase)
	if err != nil || stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() {
		return "", errors.New("evaluation stage is not a non-symlink directory")
	}
	stageRoot, err := sourceParent.OpenRoot(stageBase)
	if err != nil {
		return "", errors.New("open evaluation stage before quarantine")
	}
	defer stageRoot.Close()
	openedStage, err := stageRoot.Stat(".")
	if err != nil || !os.SameFile(stageInfo, openedStage) {
		return "", errors.New("evaluation stage changed before quarantine")
	}
	prefix := evaluationBundlePublicationIdentity(finalDirectory)
	quarantineBase := ""
	for index := 1; index <= maximumAbandonedEvaluationStages; index++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		candidate := fmt.Sprintf("openrealtime-evaluation-abandoned-%s-%06d", prefix, index)
		if _, err := destination.Lstat(candidate); os.IsNotExist(err) {
			quarantineBase = candidate
			break
		} else if err != nil {
			return "", errors.New("inspect evaluation quarantine destination")
		}
	}
	if quarantineBase == "" {
		return "", errors.New("evaluation quarantine namespace is exhausted")
	}
	stageNow, stageErr := sourceParent.Lstat(stageBase)
	_, finalErr := sourceParent.Lstat(finalBase)
	if stageErr != nil || stageNow.Mode()&os.ModeSymlink != 0 ||
		!stageNow.IsDir() || !os.SameFile(openedStage, stageNow) || !os.IsNotExist(finalErr) {
		return "", errors.New("evaluation stage changed during quarantine")
	}
	if operations.beforeQuarantineRename != nil {
		if err := operations.beforeQuarantineRename(); err != nil {
			return "", errors.New("prepare exclusive evaluation quarantine")
		}
	}
	if err := renameDirectoryNoReplace(
		sourceParentPath, stageBase, sourceParentInfo,
		quarantineDirectory, quarantineBase, destinationInfo,
	); err != nil {
		return "", fmt.Errorf("quarantine evaluation stage exclusively: %w", err)
	}
	if err := syncEvaluationBundleDirectory(sourceParent); err != nil {
		return "", err
	}
	if err := syncEvaluationBundleDirectory(destination); err != nil {
		return "", err
	}
	quarantined, quarantineErr := destination.Lstat(quarantineBase)
	_, stageErr = sourceParent.Lstat(stageBase)
	if quarantineErr != nil || quarantined.Mode()&os.ModeSymlink != 0 || !quarantined.IsDir() ||
		!os.SameFile(openedStage, quarantined) || !os.IsNotExist(stageErr) {
		return "", errors.New("evaluation stage identity changed during quarantine")
	}
	return filepath.Join(quarantineDirectory, quarantineBase), nil
}

func validateEvaluationPublicationSource(
	ctx context.Context, source Evaluation, finalDirectory string,
) error {
	seal := source.retentionSeal
	if seal == nil || seal.guard == nil || seal.guard.matcher == nil {
		return errors.New("evaluation has no provider-verified retention seal")
	}
	if _, err := validateEvaluationRetentionSealContext(ctx, source); err != nil {
		return err
	}
	if err := rejectEvaluationSensitive(ctx, seal.guard, []byte(finalDirectory)); err != nil {
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		return errors.New("evaluation final directory contains an evaluation sensitive value")
	}
	return nil
}

func evaluationPublicationDirectories(
	ctx context.Context, options EvaluationBundleOptions,
) (string, string, error) {
	if ctx == nil {
		return "", "", errors.New("evaluation publication: nil context")
	}
	if _, _, err := prepareEvaluationBundleOptions(ctx, options); err != nil {
		return "", "", err
	}
	if err := validateDirectoryAncestorChain(filepath.Dir(options.Directory)); err != nil {
		return "", "", err
	}
	stage, err := evaluationBundleStageDirectory(options.Directory)
	if err != nil {
		return "", "", err
	}
	return options.Directory, stage, nil
}

func evaluationBundleStageDirectory(finalDirectory string) (string, error) {
	if !validEvaluationReceiptDirectory(finalDirectory) {
		return "", errors.New("final evaluation directory is invalid")
	}
	name := ".openrealtime-evaluation-stage-" + evaluationBundlePublicationIdentity(finalDirectory)
	stage := filepath.Join(filepath.Dir(finalDirectory), name)
	if len(stage) > maximumRootDirectoryBytes || filepath.Clean(stage) != stage {
		return "", errors.New("evaluation staging directory is invalid")
	}
	return stage, nil
}

func evaluationBundlePublicationIdentity(finalDirectory string) string {
	value := strings.TrimPrefix(digest([]byte(filepath.Base(finalDirectory))), "sha256:")
	return value[:24]
}

func evaluationPublicationDirectoryExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("inspect evaluation publication directory")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("evaluation publication path is not a non-symlink directory")
	}
	return true, nil
}

func validateEvaluationQuarantineDirectory(path, publicationParent string) error {
	if len(path) == 0 || len(path) > maximumRootDirectoryBytes || !utf8.ValidString(path) ||
		containsControl(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == filepath.Dir(path) {
		return errors.New("evaluation quarantine directory must be canonical, absolute, and non-root")
	}
	if err := validateDirectoryAncestorChain(path); err != nil {
		return fmt.Errorf("evaluation quarantine directory: %w", err)
	}
	separator := string(filepath.Separator)
	if path == publicationParent || strings.HasPrefix(path, publicationParent+separator) ||
		strings.HasPrefix(publicationParent, path+separator) {
		return errors.New("evaluation quarantine and reportable publication trees must be disjoint")
	}
	return nil
}

func validateDirectoryAncestorChain(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("directory ancestor chain is not canonical")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("directory ancestor chain contains a symlink or non-directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return nil
}

func validateFileEvaluationReceiptPath(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return validateExternalEvaluationReceiptPath(path, "", false)
	}
	if err != nil {
		return errors.New("inspect evaluation receipt store path")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("evaluation receipt store path is not a regular file")
	}
	return validateExternalEvaluationReceiptPath(path, "", true)
}

func acquireEvaluationReceiptFileLock(ctx context.Context, path string) (*os.File, error) {
	if err := validateExternalLockPath(path); err != nil {
		return nil, err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	root, err := openValidatedRoot(parentPath, nil)
	if err != nil {
		return nil, errors.New("open evaluation receipt lock parent")
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		file, createErr := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, errors.New("create evaluation receipt lock marker")
		}
		if syncErr := file.Sync(); syncErr != nil {
			file.Close()
			return nil, errors.New("sync evaluation receipt lock marker")
		}
		if syncErr := syncEvaluationBundleDirectory(root); syncErr != nil {
			file.Close()
			return nil, syncErr
		}
		info, err = root.Lstat(name)
		file.Close()
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("evaluation receipt lock marker is invalid")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("open evaluation receipt lock marker")
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || fileidentity.RequireSingleLink(file) != nil {
		file.Close()
		return nil, errors.New("evaluation receipt lock marker changed while opening")
	}
	if err := lockFileExclusive(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("evaluation receipt publication is already leased: %w", err)
	}
	if err := ctx.Err(); err != nil {
		file.Close()
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || fileidentity.RequireSingleLink(file) != nil {
		file.Close()
		return nil, errors.New("evaluation receipt lock marker changed after locking")
	}
	return file, nil
}

func validateExternalLockPath(path string) error {
	if len(path) == 0 || len(path) > maximumRootDirectoryBytes || !utf8.ValidString(path) ||
		containsControl(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == filepath.Dir(path) {
		return errors.New("evaluation receipt lock path is invalid")
	}
	return validateDirectoryAncestorChain(filepath.Dir(path))
}

func leaseCloseError(lease EvaluationBundleReceiptLease) error {
	if nilEvaluationReceiptLease(lease) {
		return nil
	}
	return lease.Close()
}

func nilEvaluationReceiptStore(store EvaluationBundleReceiptStore) bool {
	return nilInterfaceValue(store)
}

func nilEvaluationReceiptLease(lease EvaluationBundleReceiptLease) bool {
	return nilInterfaceValue(lease)
}

func nilInterfaceValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
