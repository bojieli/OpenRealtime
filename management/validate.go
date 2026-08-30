package management

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// CanonicalSessionID reports whether value is safe as one opaque management
// resource segment. It deliberately rejects separators instead of trying to
// assign filesystem or URL semantics to a session identifier.
func CanonicalSessionID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 256 &&
		!strings.ContainsAny(value, "/\\\x00\r\n")
}

// CanonicalDigest reports whether value is a lowercase, canonical SHA-256
// content identity.
func CanonicalDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

// ValidateSessionSnapshot checks the identity-bearing evidence required at a
// public management boundary. Free-form fields are permitted here because a
// provider may validate before redaction; callers must still apply RedactLive
// before returning a snapshot to an inspector.
func ValidateSessionSnapshot(snapshot inspect.Live) error {
	if snapshot.FormatVersion != inspect.LiveFormatVersion || snapshot.GraphID == "" ||
		snapshot.GraphRevision == 0 || !CanonicalDigest(snapshot.Fingerprint) || snapshot.Configuration == nil {
		return fmt.Errorf("%w: session source returned incomplete live evidence", ErrConflict)
	}
	if err := snapshot.Configuration.Validate(); err != nil || snapshot.Configuration.Digest == "" {
		return fmt.Errorf("%w: session source returned invalid configuration evidence", ErrConflict)
	}
	if snapshot.Adapter != nil {
		if err := snapshot.Adapter.Validate(); err != nil {
			return fmt.Errorf("%w: session source returned invalid adapter resolution", ErrConflict)
		}
	}
	if len(snapshot.Nodes) == 0 {
		return fmt.Errorf("%w: session source returned no resolved nodes", ErrConflict)
	}
	for id, node := range snapshot.Nodes {
		if !canonicalEvidenceName(id) || node.Resolution == nil {
			return fmt.Errorf("%w: session source returned an unresolved node", ErrConflict)
		}
		if err := element.ValidateIdentity(node.Resolution.Element); err != nil ||
			node.Resolution.Runtime.Validate() != nil {
			return fmt.Errorf("%w: session source returned invalid node resolution", ErrConflict)
		}
		if _, err := inspect.CanonicalCapabilities(node.Resolution.Capabilities); err != nil {
			return fmt.Errorf("%w: session source returned invalid capabilities", ErrConflict)
		}
	}
	return nil
}

func canonicalEvidenceName(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

// ValidateDeltaPage verifies a bounded response against the request that
// selected it. Event payload structure is subsequently covered by the trace
// package when a complete recording is exported; this boundary additionally
// prevents cursor and resource substitution.
func ValidateDeltaPage(session string, after uint64, limit uint32, page DeltaPage) error {
	if !CanonicalSessionID(session) || limit == 0 || limit > 4096 ||
		page.FormatVersion != 1 || page.SessionID != session || page.After != after ||
		page.Next < page.After || len(page.Events) > int(limit) ||
		page.Graph.FormatVersion != ir.FormatVersion || page.Graph.ID == "" ||
		page.Graph.Revision == 0 || !CanonicalDigest(page.Graph.Fingerprint) {
		return fmt.Errorf("%w: session source returned an invalid delta page", ErrConflict)
	}
	if page.Baseline != nil {
		if page.Baseline.Sequence == 0 || page.Baseline.Sequence > page.Next {
			return fmt.Errorf("%w: delta baseline has an invalid sequence", ErrConflict)
		}
		if page.After != 0 && !page.Compacted && page.Baseline.Sequence > page.After {
			return fmt.Errorf("%w: an unsolicited delta baseline changes cursor semantics", ErrConflict)
		}
	}
	previous := page.After
	if page.Baseline != nil && page.Baseline.Sequence > previous {
		previous = page.Baseline.Sequence
	}
	for index, event := range page.Events {
		if event.Sequence <= previous || event.Sequence > page.Next {
			return fmt.Errorf("%w: delta event %d has an invalid sequence", ErrConflict, index)
		}
		previous = event.Sequence
	}
	if len(page.Events) > 0 && page.Next != page.Events[len(page.Events)-1].Sequence {
		return fmt.Errorf("%w: delta cursor does not name its last event", ErrConflict)
	}
	return nil
}

// ValidateReconciliationReceipt binds a mutation receipt to the exact request
// reviewed by the caller.
func ValidateReconciliationReceipt(
	request ReconciliationRequest, receipt ReconciliationReceipt,
) error {
	if receipt.FormatVersion != 1 || receipt.SessionID != request.SessionID ||
		receipt.PreviousFingerprint != request.ExpectedFingerprint ||
		receipt.CandidateFingerprint != request.Candidate.Fingerprint ||
		!CanonicalDigest(receipt.PreviousFingerprint) ||
		!CanonicalDigest(receipt.CandidateFingerprint) {
		return fmt.Errorf("%w: reconciler returned an invalid receipt", ErrConflict)
	}
	return nil
}
