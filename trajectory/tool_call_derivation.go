package trajectory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/toolargs"
)

// ToolCallArgumentsDigest binds a derivation to the exact argument bytes in a
// proposal or effective call. It deliberately does not canonicalize JSON:
// byte identity is part of the immutable trajectory contract.
func ToolCallArgumentsDigest(arguments json.RawMessage) string {
	digest := sha256.Sum256(arguments)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// validateToolCallPromotion preserves byte-exact promotion as the default and
// admits changed arguments only through a recognized, fully bound derivation.
func validateToolCallPromotion(call Item, proposal toolProposalRecord) error {
	if !slices.Contains(call.CausalParentIDs, proposal.itemID) {
		return fmt.Errorf("tool call %q does not causally promote proposal item %q",
			call.ToolCall.CallID, proposal.itemID)
	}
	if call.InvocationID != proposal.invocationID || call.SourceRevision != proposal.sourceRevision {
		return fmt.Errorf("tool call %q changes proposal invocation or source revision", call.ToolCall.CallID)
	}
	if call.ToolCall.CallID != proposal.call.CallID || call.ToolCall.Name != proposal.call.Name {
		return fmt.Errorf("tool call %q changes the canonical proposal", call.ToolCall.CallID)
	}
	if bytes.Equal(call.ToolCall.Arguments, proposal.call.Arguments) {
		if call.ToolCallDerivation != nil {
			return fmt.Errorf("tool call %q carries a derivation without changing proposal arguments", call.ToolCall.CallID)
		}
		return nil
	}
	if call.ToolCallDerivation == nil {
		return fmt.Errorf("tool call %q changes the canonical proposal", call.ToolCall.CallID)
	}
	if err := validateToolCallDerivation(call, proposal.call.Arguments); err != nil {
		return fmt.Errorf("tool call %q has invalid derivation: %w", call.ToolCall.CallID, err)
	}
	return nil
}

func validateToolCallDerivation(call Item, sourceArguments json.RawMessage) error {
	derivation := call.ToolCallDerivation
	if derivation == nil {
		return errors.New("derivation is missing")
	}
	if call.Producer.Phase != PhaseRuntime {
		return errors.New("derived tool call must be authored by the runtime")
	}
	if derivation.Kind != ToolCallDerivationSchemaNormalizationV1 {
		return fmt.Errorf("unknown derivation kind %q", derivation.Kind)
	}
	if derivation.SourceArgumentsDigest != ToolCallArgumentsDigest(sourceArguments) {
		return errors.New("source argument digest does not match the canonical proposal")
	}
	if derivation.EffectiveArgumentsDigest != ToolCallArgumentsDigest(call.ToolCall.Arguments) {
		return errors.New("effective argument digest does not match the canonical call")
	}
	if derivation.SourceArgumentsDigest == derivation.EffectiveArgumentsDigest {
		return errors.New("derivation does not bind changed arguments")
	}
	if derivation.RegistryReference == "" || derivation.RegistryReference != strings.TrimSpace(derivation.RegistryReference) {
		return errors.New("registry reference is not canonical")
	}
	if !isCanonicalSHA256(derivation.RegistryDigest) {
		return errors.New("registry digest is not canonical SHA-256")
	}
	if !isCanonicalSHA256(derivation.DeclarationDigest) {
		return errors.New("declaration digest is not canonical SHA-256")
	}
	if len(derivation.Rewrites) == 0 || len(derivation.Rewrites) > 4096 {
		return errors.New("derivation must name a bounded non-empty rewrite set")
	}
	last := ""
	for index, rewrite := range derivation.Rewrites {
		if rewrite.Argument == "" || rewrite.Argument != strings.TrimSpace(rewrite.Argument) ||
			(index > 0 && rewrite.Argument <= last) {
			return errors.New("derivation rewrites are not unique, sorted, canonical arguments")
		}
		if !toolargs.Supported(rewrite.Normalizer) {
			return fmt.Errorf("derivation names unsupported normalizer %q", rewrite.Normalizer)
		}
		last = rewrite.Argument
	}
	rules := make([]toolargs.Rule, len(derivation.Rewrites))
	for index, rewrite := range derivation.Rewrites {
		rules[index] = toolargs.Rule{Argument: rewrite.Argument, Normalizer: rewrite.Normalizer}
	}
	effective, changed, err := toolargs.Apply(sourceArguments, rules)
	if err != nil {
		return fmt.Errorf("replay derivation: %w", err)
	}
	if !slices.Equal(changed, rules) {
		return errors.New("derivation names a rewrite that does not change its source argument")
	}
	if !bytes.Equal(effective, call.ToolCall.Arguments) {
		return errors.New("effective arguments differ from deterministic derivation replay")
	}
	return nil
}

func isCanonicalSHA256(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

// isPromotedToolProposal applies the same closed derivation contract when a
// provider adapter inspects a defensive snapshot. Snapshots can be supplied
// by callers rather than a Store, so this path must not trust their metadata.
func isPromotedToolProposal(proposal, call Item) bool {
	if proposal.Kind != KindToolProposal || proposal.ToolCall == nil ||
		call.Kind != KindToolCall || call.ToolCall == nil ||
		call.InvocationID != proposal.InvocationID ||
		call.SourceRevision != proposal.SourceRevision ||
		call.ToolCall.CallID != proposal.ToolCall.CallID ||
		call.ToolCall.Name != proposal.ToolCall.Name ||
		!slices.Contains(call.CausalParentIDs, proposal.ID) {
		return false
	}
	if validateToolCall(*proposal.ToolCall) != nil || validateToolCall(*call.ToolCall) != nil {
		return false
	}
	if bytes.Equal(call.ToolCall.Arguments, proposal.ToolCall.Arguments) {
		return call.ToolCallDerivation == nil
	}
	return validateToolCallDerivation(call, proposal.ToolCall.Arguments) == nil
}
