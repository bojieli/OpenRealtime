package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// VerifyIntentSettlementCancellation proves that an exact cancellation names
// canonical final user authority. It intentionally permits an older intent:
// cancellation can race a newer intent, but the complete identity prevents it
// from revoking that newer epoch.
func VerifyIntentSettlementCancellation(
	snapshot trajectory.Snapshot, cancellation IntentSettlementCancellation,
) error {
	if err := preflightIntentSettlementCancellation(cancellation); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("settlement cancellation session ID", cancellation.SessionID, true); err != nil {
		return err
	}
	if boundedPolicyReason(cancellation.Reason) != cancellation.Reason {
		return fmt.Errorf("settlement cancellation reason exceeds %d bytes or is not valid UTF-8",
			maximumPolicyReasonBytes)
	}
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return errors.New("settlement cancellation snapshot has inconsistent version and items")
	}
	_, item, err := temporalEvidenceItem(snapshot.Items, cancellation.DurableIntent)
	if err != nil {
		return fmt.Errorf("settlement cancellation durable intent: %w", err)
	}
	if trajectory.AuthorityOf(item) != trajectory.AuthorityUser ||
		item.Producer.Phase != trajectory.PhaseUser || item.Event == nil ||
		item.Event.Type != item.Event.Source+".endpoint" || item.Event.OccurredNS == 0 {
		return errors.New("settlement cancellation does not name final timestamped user authority")
	}
	return nil
}

// VerifyIntentSettlementReset proves that a reset names one canonical durable
// user-intent epoch. Authorization to emit it remains a graph-edge decision;
// this verifier makes the terminal witness independently checkable.
func VerifyIntentSettlementReset(
	snapshot trajectory.Snapshot, reset IntentSettlementAddress,
) error {
	if err := preflightIntentSettlementAddress("settlement reset", reset); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("settlement reset session ID", reset.SessionID, true); err != nil {
		return err
	}
	if boundedPolicyReason(reset.Reason) != reset.Reason {
		return fmt.Errorf("settlement reset reason exceeds %d bytes or is not valid UTF-8",
			maximumPolicyReasonBytes)
	}
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return errors.New("settlement reset snapshot has inconsistent version and items")
	}
	_, item, err := temporalEvidenceItem(snapshot.Items, reset.DurableIntent)
	if err != nil {
		return fmt.Errorf("settlement reset durable intent: %w", err)
	}
	if trajectory.AuthorityOf(item) != trajectory.AuthorityUser ||
		item.Producer.Phase != trajectory.PhaseUser || item.Event == nil ||
		item.Event.Type != item.Event.Source+".endpoint" || item.Event.OccurredNS == 0 {
		return errors.New("settlement reset does not name final timestamped user authority")
	}
	return nil
}

// VerifyIntentSettlementProbe independently proves that a probe is the exact,
// canonical classification request selected by contract. A disposition
// producer should call this against the current store snapshot before resolving
// media or invoking a model; graph typing alone does not make a payload trusted.
func VerifyIntentSettlementProbe(
	snapshot trajectory.Snapshot, probe IntentSettlementProbe, expected IntentSettlementConfig,
) error {
	if err := preflightIntentSettlementProbe(probe); err != nil {
		return err
	}
	expected = intentSettlementConfigWithDefaults(expected)
	expected, err := normalizeIntentSettlementConfig(expected)
	if err != nil {
		return fmt.Errorf("expected intent settlement contract: %w", err)
	}
	if err := validatePolicyIdentifier("settlement probe ID", probe.ProbeID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("settlement probe issuer", probe.Issuer, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("settlement probe session ID", probe.SessionID, true); err != nil {
		return err
	}
	if probe.Sequence == 0 || probe.IssuedNS == 0 {
		return errors.New("settlement probe has no positive sequence or issue time")
	}
	if probe.Detector != expected.Detector {
		return errors.New("settlement probe detector differs from the independently pinned detector")
	}
	if err := VerifyAdmittedTemporalEvidence(
		snapshot, probe.Evidence, expected.ExpectedAdmission,
	); err != nil {
		return fmt.Errorf("verify settlement probe temporal evidence: %w", err)
	}
	if err := validateIntentSettlementEvidenceProjection(probe.Evidence); err != nil {
		return fmt.Errorf("verify settlement probe temporal evidence: %w", err)
	}
	if probe.Evidence.DurableIntent == nil ||
		probe.DurableIntent != *probe.Evidence.DurableIntent ||
		probe.TriggerObservation != probe.Evidence.TriggerObservation ||
		probe.Prefix != probe.Evidence.Prefix {
		return errors.New("settlement probe summary differs from its complete temporal evidence")
	}
	result, candidate, err := intentSettlementCandidate(
		snapshot, probe.Evidence, expected.CandidateSources,
	)
	if err != nil {
		return fmt.Errorf("verify settlement probe result: %w", err)
	}
	if !candidate || result != probe.Result {
		return errors.New("settlement probe does not name its exact successful result-linked consequence")
	}
	want, err := intentSettlementProbeID(probe)
	if err != nil {
		return err
	}
	if probe.ProbeID != want {
		return errors.New("settlement probe ID does not match its canonical payload")
	}
	return nil
}

// validateIntentSettlementEvidenceProjection prevents upstream diagnostic
// fields from becoming unauthenticated inputs to probe and terminal IDs. A
// committed outcome has no error code or message; those fields belong only to
// rejected commit outcomes and are not part of settlement evidence.
func validateIntentSettlementEvidenceProjection(evidence AdmittedTemporalEvidence) error {
	if evidence.TriggerCommit.Code != "" || evidence.TriggerCommit.Message != "" {
		return errors.New("committed settlement evidence carries rejection diagnostics")
	}
	return nil
}

// VerifyIntentSettlementDecision independently proves a gate-to-activation
// terminal control. It verifies the immutable historical prefix even when a
// newer user intent has since been appended; the consumer must additionally
// require the decision's invocation/call/intent to match its own active state.
func VerifyIntentSettlementDecision(
	snapshot trajectory.Snapshot, decision IntentSettlementDecision,
	expected IntentSettlementConfig,
) error {
	if err := preflightIntentSettlementDecision(decision); err != nil {
		return err
	}
	if decision.Evidence.Prefix.Version == 0 ||
		decision.Evidence.Prefix.Version > snapshot.Version ||
		decision.Evidence.Prefix.Version > uint64(len(snapshot.Items)) {
		return errors.New("settlement decision prefix is outside the canonical snapshot")
	}
	if err := trajectory.VerifyPrefix(snapshot, decision.Evidence.Prefix); err != nil {
		return fmt.Errorf("verify settlement decision prefix: %w", err)
	}
	historical := trajectory.Snapshot{
		Version: decision.Evidence.Prefix.Version,
		Items:   snapshot.Items[:decision.Evidence.Prefix.Version],
	}
	if err := VerifyIntentSettlementProbe(historical, decision.Probe, expected); err != nil {
		return err
	}
	if decision.SessionID != decision.Probe.SessionID ||
		decision.InvocationID != decision.Probe.Result.InvocationID ||
		!reflect.DeepEqual(decision.Evidence, decision.Probe.Evidence) {
		return errors.New("settlement decision identity differs from its verified probe")
	}
	if decision.StateRevisionAfter != decision.StateRevisionBefore+1 ||
		decision.FinishedNS == 0 || decision.FinishedNS < decision.Probe.IssuedNS {
		return errors.New("settlement decision has invalid state revisions or timing")
	}
	switch decision.Kind {
	case IntentSettlementDecisionSucceeded, IntentSettlementDecisionFailed:
		if decision.Disposition == nil || decision.Cancellation != nil || decision.Reset != nil ||
			decision.SupersedingIntent != nil ||
			!reflect.DeepEqual(decision.Disposition.Probe, decision.Probe) ||
			intentDispositionDecisionKind(decision.Disposition.Kind) != decision.Kind {
			return errors.New("terminal settlement decision lacks its exact matching disposition")
		}
		if err := validateIntentDisposition(*decision.Disposition, expected.Detector); err != nil {
			return fmt.Errorf("verify settlement decision disposition: %w", err)
		}
		if decision.FinishedNS < decision.Disposition.DecisionFinishedNS {
			return errors.New("settlement decision predates its accepted disposition")
		}
	case IntentSettlementDecisionSuperseded:
		if decision.Disposition != nil || decision.Cancellation != nil || decision.Reset != nil ||
			decision.SupersedingIntent == nil {
			return errors.New("superseded settlement decision lacks its exact newer intent")
		}
		_, item, err := temporalEvidenceItem(snapshot.Items, *decision.SupersedingIntent)
		if err != nil {
			return fmt.Errorf("verify settlement superseding intent: %w", err)
		}
		if decision.SupersedingIntent.StoreVersion <= decision.Probe.DurableIntent.StoreVersion ||
			trajectory.AuthorityOf(item) != trajectory.AuthorityUser ||
			item.Producer.Phase != trajectory.PhaseUser || item.Event == nil ||
			item.Event.Type != item.Event.Source+".endpoint" || item.Event.OccurredNS == 0 {
			return errors.New("settlement supersession witness is not newer final user authority")
		}
	case IntentSettlementDecisionReset:
		if decision.Disposition != nil || decision.Cancellation != nil ||
			decision.SupersedingIntent != nil || decision.Reset == nil ||
			decision.Reset.SessionID != decision.SessionID ||
			decision.Reset.DurableIntent != decision.Probe.DurableIntent {
			return errors.New("reset settlement decision lacks its exact reset address")
		}
		if err := VerifyIntentSettlementReset(snapshot, *decision.Reset); err != nil {
			return fmt.Errorf("verify settlement decision reset: %w", err)
		}
	case IntentSettlementDecisionCanceled:
		if decision.Disposition != nil || decision.Reset != nil ||
			decision.SupersedingIntent != nil || decision.Cancellation == nil ||
			decision.Cancellation.SessionID != decision.SessionID ||
			decision.Cancellation.DurableIntent != decision.Probe.DurableIntent {
			return errors.New("canceled settlement decision lacks its exact durable-intent cancellation")
		}
		if err := VerifyIntentSettlementCancellation(snapshot, *decision.Cancellation); err != nil {
			return fmt.Errorf("verify settlement decision cancellation: %w", err)
		}
	default:
		return fmt.Errorf("unknown settlement decision kind %q", decision.Kind)
	}
	want, err := intentSettlementTerminalID(decision)
	if err != nil {
		return err
	}
	if decision.TerminalID != want {
		return errors.New("settlement terminal ID does not match its canonical payload")
	}
	return nil
}

func intentSettlementConfigWithDefaults(config IntentSettlementConfig) IntentSettlementConfig {
	if config.MaxTrackedIntents == 0 {
		config.MaxTrackedIntents = defaultIntentSettlementTrackedIntents
	}
	if config.CancelMemory == 0 {
		config.CancelMemory = defaultIntentSettlementCancellations
	}
	return config
}

func intentSettlementProbeID(probe IntentSettlementProbe) (string, error) {
	probe.ProbeID = ""
	payload, err := json.Marshal(probe)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "intent-settlement-probe:sha256:" + hex.EncodeToString(digest[:]), nil
}

func intentSettlementTerminalID(decision IntentSettlementDecision) (string, error) {
	decision.TerminalID = ""
	payload, err := json.Marshal(decision)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "intent-settlement-terminal:sha256:" + hex.EncodeToString(digest[:]), nil
}
