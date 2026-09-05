package graphnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// VerifyScoredSourceBundle adds current deterministic scorer replay to source
// integrity verification. Historical bundles without replay inputs remain
// readable through VerifySourceBundle but cannot satisfy this acceptance gate.
// Recognizer text is treated as a retained external observation bound to its
// exact PCM window; this does not independently establish ASR accuracy.
func VerifyScoredSourceBundle(ctx context.Context, options SourceBundleOptions, expected SourceReceipt) (verified SourceBundle, resultErr error) {
	bundle, err := VerifySourceBundle(ctx, options, expected)
	if err != nil {
		return SourceBundle{}, err
	}
	root, info, err := openSourceRoot(options.Directory)
	if err != nil {
		return SourceBundle{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			verified = SourceBundle{}
			resultErr = errors.Join(resultErr, errors.New("close replayed scenario source directory"))
		}
	}()
	catalog := map[string]scenario.Scenario{}
	for _, item := range scenario.Catalog() {
		catalog[item.Name] = item
	}
	for index, attempt := range bundle.Manifest.Attempts {
		item, found := catalog[attempt.Record.Key.CaseName]
		if !found {
			return SourceBundle{}, errors.New("scenario score replay has no canonical authored case")
		}
		if len(attempt.Submitted) != len(item.Sees) {
			return SourceBundle{}, errors.New("scenario score replay visual inventory differs")
		}
		for index, sight := range item.Sees {
			data, err := os.ReadFile(sight.Path)
			if err != nil || sourceDigest(data) != attempt.Submitted[index].Receipt.SHA256 {
				return SourceBundle{}, errors.New("scenario score replay submitted visual differs from the authored input")
			}
		}
		payload, err := readSourceFile(ctx, root, attempt.Result)
		if err != nil {
			return SourceBundle{}, err
		}
		var retained scenario.Result
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&retained); err != nil {
			return SourceBundle{}, err
		}
		wav, err := readSourceFile(ctx, root, attempt.Audio)
		if err != nil {
			return SourceBundle{}, err
		}
		replayed, err := scenario.ReplayRecorded(ctx, item, retained, wav)
		if err != nil {
			return SourceBundle{}, fmt.Errorf("scenario score replay %s: %w", attempt.Record.Key.TaskID, err)
		}
		derived := scenario.TaskOutcome(attempt.Record.Key.TaskID, replayed, nil)
		if !reflect.DeepEqual(derived, bundle.ArchitectureResult.Measurement.Tasks[index]) {
			return SourceBundle{}, errors.New("scenario architecture task differs from replayed outcome or metrics")
		}
	}
	if err := verifySourceRootIdentity(options.Directory, root, info); err != nil {
		return SourceBundle{}, err
	}
	reopened, err := VerifySourceBundle(ctx, options, expected)
	if err != nil || !reflect.DeepEqual(reopened.Receipt, bundle.Receipt) {
		return SourceBundle{}, errors.New("scenario source changed during deterministic replay")
	}
	return reopened, nil
}
