package releasevalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	meetingbench "github.com/bojieli/OpenRealtime/bench/meeting"
	realtimecu "github.com/bojieli/OpenRealtime/bench/realtimecu"
	candidatesource "github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
	scenariosource "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
)

// campaignSourceVerifier reopens a repository-owned external source receipt
// and its complete source tree. It returns the digest of the deterministic
// result authenticated by that receipt. Keeping this registry closed inside
// the package prevents a closure from declaring its own verifier trustworthy.
type campaignSourceVerifier func(
	context.Context, string, []byte, CampaignArtifactReceipt,
) (string, error)

var campaignSourceVerifiers = map[string]campaignSourceVerifier{
	candidatesource.ReceiptFormat:          verifyCandidateSourceReceipt,
	scenariosource.SourceReceiptFormat:     verifyScenarioSourceReceipt,
	meetingbench.ReviewSourceReceiptFormat: verifyMeetingSourceReceipt,
	realtimecu.ReviewSourceReceiptFormat:   verifyRealtimeCUSourceReceipt,
}

func verifyCampaignSourceReceipt(
	ctx context.Context,
	path string,
	payload []byte,
	wrapper CampaignArtifactReceipt,
) (string, error) {
	verifier, found := campaignSourceVerifiers[wrapper.ArtifactFormat]
	if !found {
		return "", fmt.Errorf("no repository-owned verifier for source receipt format %q",
			wrapper.ArtifactFormat)
	}
	digest, err := verifier(ctx, path, payload, wrapper)
	if err != nil {
		return "", err
	}
	if !sha256Pattern.MatchString(digest) {
		return "", errors.New("source verifier returned no canonical deterministic-result digest")
	}
	return digest, nil
}

func verifyCandidateSourceReceipt(
	ctx context.Context,
	_ string,
	payload []byte,
	_ CampaignArtifactReceipt,
) (string, error) {
	var receipt candidatesource.Receipt
	if err := decodeCampaignSourceReceipt(payload, &receipt); err != nil {
		return "", err
	}
	manifest, opened, err := candidatesource.VerifyReceiptPayload(ctx, payload)
	if err != nil {
		return "", fmt.Errorf("verify candidate source bundle: %w", err)
	}
	if opened.ReceiptSHA256 != receipt.ReceiptSHA256 {
		return "", errors.New("candidate source verifier reopened another receipt")
	}
	return manifest.ResultSHA256, nil
}

func verifyScenarioSourceReceipt(
	ctx context.Context,
	_ string,
	payload []byte,
	_ CampaignArtifactReceipt,
) (string, error) {
	var receipt scenariosource.SourceReceipt
	if err := decodeCampaignSourceReceipt(payload, &receipt); err != nil {
		return "", err
	}
	bundle, err := scenariosource.VerifyScoredSourceBundle(ctx, scenariosource.SourceBundleOptions{
		Directory: receipt.Directory,
	}, receipt)
	if err != nil {
		return "", fmt.Errorf("verify scenario source bundle: %w", err)
	}
	return bundle.Receipt.ArchitectureSHA256, nil
}

func verifyMeetingSourceReceipt(
	ctx context.Context,
	_ string,
	payload []byte,
	_ CampaignArtifactReceipt,
) (string, error) {
	var receipt meetingbench.ReviewSourceReceipt
	if err := decodeCampaignSourceReceipt(payload, &receipt); err != nil {
		return "", err
	}
	bundle, err := meetingbench.VerifyMeetingReviewSource(ctx, receipt.Directory, receipt)
	if err != nil {
		return "", fmt.Errorf("verify meeting source bundle: %w", err)
	}
	return bundle.Receipt.ResultSHA256, nil
}

func verifyRealtimeCUSourceReceipt(
	_ context.Context,
	_ string,
	payload []byte,
	_ CampaignArtifactReceipt,
) (string, error) {
	var receipt realtimecu.ReviewSourceReceipt
	if err := decodeCampaignSourceReceipt(payload, &receipt); err != nil {
		return "", err
	}
	if _, err := realtimecu.VerifyReviewSourceReceipt(receipt.Directory, receipt); err != nil {
		return "", fmt.Errorf("verify realtime computer-use source bundle: %w", err)
	}
	return receipt.ResultSHA256, nil
}

func decodeCampaignSourceReceipt(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode source receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("source receipt contains trailing JSON")
		}
		return fmt.Errorf("decode source receipt trailing JSON: %w", err)
	}
	return nil
}
