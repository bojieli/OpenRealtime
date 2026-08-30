package review

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"hash"
	"sync/atomic"
)

const evaluationRetentionSealVersion = "openrealtime.review-evaluation-retention.v1"

// evaluationRetentionSeal is deliberately not serializable. It proves that
// Evaluate produced the exact public Evaluation snapshot after a successful
// provider verification, and retains the original opaque declared-secret
// guard without revealing or fingerprinting its values.
type evaluationRetentionSeal struct {
	digest  string
	guard   *declaredSensitiveGuard
	claimed atomic.Bool
}

func newEvaluationRetentionSealContext(
	ctx context.Context, evaluation Evaluation, guard *declaredSensitiveGuard,
) (*evaluationRetentionSeal, error) {
	if ctx == nil {
		return nil, errors.New("evaluation retention seal requires a context")
	}
	if guard == nil || guard.matcher == nil ||
		guard.count != evaluation.Record.SensitiveValueCount {
		return nil, errors.New("evaluation retention seal has no valid sensitive-value capability")
	}
	digest, err := evaluationRetentionDigestContext(ctx, evaluation)
	if err != nil {
		return nil, err
	}
	return &evaluationRetentionSeal{digest: digest, guard: guard}, nil
}

func validateEvaluationRetentionSealContext(
	ctx context.Context, evaluation Evaluation,
) (*evaluationRetentionSeal, error) {
	seal := evaluation.retentionSeal
	if seal == nil || seal.digest == "" || seal.guard == nil || seal.guard.matcher == nil ||
		seal.guard.count != evaluation.Record.SensitiveValueCount {
		return nil, errors.New("evaluation has no valid provider-verified retention seal")
	}
	digest, err := evaluationRetentionDigestContext(ctx, evaluation)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(digest), []byte(seal.digest)) != 1 {
		return nil, errors.New("evaluation differs from its provider-verified retention seal")
	}
	return seal, nil
}

func claimEvaluationRetentionSeal(seal *evaluationRetentionSeal) error {
	if seal == nil || !seal.claimed.CompareAndSwap(false, true) {
		return errors.New("evaluation retention seal was already claimed")
	}
	return nil
}

func secureReviewDigestEqual(left, right string) bool {
	return len(left) == len(right) &&
		subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// evaluationRetentionDigestContext binds every byte exposed by Evaluation.
// The opaque guard is intentionally not hashed: hashing low-entropy secrets
// would create a guessing oracle. The seal instead owns the exact guard.
func evaluationRetentionDigestContext(
	ctx context.Context, evaluation Evaluation,
) (string, error) {
	if ctx == nil {
		return "", errors.New("evaluation retention digest requires a context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	record, err := MarshalRecord(evaluation.Record)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	if err := writeEvaluationSealField(ctx, hasher, []byte(evaluationRetentionSealVersion)); err != nil {
		return "", err
	}
	for _, payload := range [][]byte{
		record,
		evaluation.ProviderImplementation,
		evaluation.ProviderConfiguration,
		evaluation.ProviderRequest,
		evaluation.Prompt,
		evaluation.Schema,
		evaluation.Context,
		evaluation.RawResponse,
		evaluation.NormalizedOutput,
	} {
		if err := writeEvaluationSealField(ctx, hasher, payload); err != nil {
			return "", err
		}
	}
	writeSealInt(hasher, int64(len(evaluation.Media)))
	for index := range evaluation.Media {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		metadata, err := marshalCanonicalCompact(
			evaluation.Media[index].Media, maximumPreparedPublicBytes,
		)
		if err != nil {
			return "", err
		}
		if err := writeEvaluationSealField(ctx, hasher, metadata); err != nil {
			return "", err
		}
		if err := writeEvaluationSealField(ctx, hasher, evaluation.Media[index].Bytes); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func writeEvaluationSealField(ctx context.Context, hasher hash.Hash, payload []byte) error {
	writeSealInt(hasher, int64(len(payload)))
	for offset := 0; offset < len(payload); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+(64<<10), len(payload))
		_, _ = hasher.Write(payload[offset:end])
		offset = end
	}
	return ctx.Err()
}
