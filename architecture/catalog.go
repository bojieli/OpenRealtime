// Package architecture owns the versioned structural definitions OpenRealtime
// can realise.
//
// An architecture is not a model family and it is not a launch script. It is
// an immutable selection of subsystem owners, required capabilities, and an
// interaction boundary. A concrete deployment supplies model endpoints and
// credentials; a binding supplies the adapter machinery. Keeping those three
// concepts separate lets the same foreground participate in several evolving
// architectures without either renaming the model or adding another runtime.
package architecture

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/binding"
)

const CatalogVersion = 1

//go:embed catalog.json
var defaultCatalogJSON []byte

// Stage says how much operational confidence a definition carries. It is not
// a performance ranking.
type Stage string

const (
	StageExperimental Stage = "experimental"
	StageCandidate    Stage = "candidate"
	StageStable       Stage = "stable"
	StageRetired      Stage = "retired"
)

// InteractionMode selects where conversational acts are chosen.
type InteractionMode string

const (
	InteractionPredicates InteractionMode = "predicates"
	InteractionTextPolicy InteractionMode = "text-policy"
	InteractionComposed   InteractionMode = "composed-policy"
	InteractionNative     InteractionMode = "native"
	InteractionRemote     InteractionMode = "remote"
)

// EvidenceSource names the representation available to the interaction owner.
type EvidenceSource string

const (
	EvidenceAcousticPredicates EvidenceSource = "acoustic-predicates"
	EvidenceTranscript         EvidenceSource = "transcript"
	EvidenceNativeMultimodal   EvidenceSource = "native-multimodal"
	EvidenceRemoteMultimodal   EvidenceSource = "remote-multimodal"
)

// ActHandoff says how a selected act reaches the foreground.
type ActHandoff string

const (
	HandoffNone   ActHandoff = "none"
	HandoffDirect ActHandoff = "direct"
	HandoffTyped  ActHandoff = "typed"
)

// Ref is an exact architecture revision. There is intentionally no mutable
// "latest" reference in a runtime or benchmark artifact.
type Ref struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

func (ref Ref) String() string { return ref.ID + "@" + strconv.Itoa(ref.Revision) }

// ParseRef parses the exact external spelling id@revision.
func ParseRef(value string) (Ref, error) {
	value = strings.TrimSpace(value)
	index := strings.LastIndex(value, "@")
	if index <= 0 || index == len(value)-1 {
		return Ref{}, fmt.Errorf("architecture reference must be id@revision, got %q", value)
	}
	revision, err := strconv.Atoi(value[index+1:])
	if err != nil || revision <= 0 {
		return Ref{}, fmt.Errorf("architecture revision must be a positive integer, got %q", value[index+1:])
	}
	ref := Ref{ID: value[:index], Revision: revision}
	if !validName.MatchString(ref.ID) {
		return Ref{}, fmt.Errorf("invalid architecture id %q", ref.ID)
	}
	return ref, nil
}

// InteractionBoundary is the complete structural control seam. Policy model,
// recognizer, and timeout identities belong to a deployment/experiment cell.
type InteractionBoundary struct {
	Mode     InteractionMode `json:"mode"`
	Evidence EvidenceSource  `json:"evidence"`
	// EvidenceCapabilities is the exact selected input vector, rather than a
	// lower bound on what a model might be capable of seeing. A runtime with an
	// extra selected channel is a different architecture: direct vision, for
	// example, must never hide behind the same "transcript" label.
	//
	// The pointer is intentional. Revisions created before evidence vectors
	// existed retain nil and therefore retain their canonical fingerprints.
	// New revisions must set it and are eligible for evidence-controlled
	// experiments; legacy revisions remain runnable and inspectable but coarse.
	EvidenceCapabilities *binding.InteractionEvidenceCapabilities `json:"evidence_capabilities,omitempty"`
	Transport            string                                   `json:"transport"`
	ProtocolVersion      int                                      `json:"protocol_version,omitempty"`
	Handoff              ActHandoff                               `json:"handoff"`
	NativeSuppression    string                                   `json:"native_suppression,omitempty"`
	// Control is the exact selected-controller vector and arbitration rule.
	// It is optional only for immutable revisions authored before controller
	// composition was attested. New revisions set it so one architecture cannot
	// silently alternate between a predicate floor and a learned act floor.
	Control *binding.InteractionControl `json:"control,omitempty"`
}

// UsesTextPolicy reports whether the selected control composition contains an
// external text policy. Legacy revisions fall back to their coarse mode.
func (boundary InteractionBoundary) UsesTextPolicy() bool {
	if boundary.Control != nil {
		return boundary.Control.Selectors.TextPolicy
	}
	return boundary.Mode == InteractionTextPolicy
}

// Definition is an immutable architecture revision.
type Definition struct {
	ID          string                    `json:"id"`
	Revision    int                       `json:"revision"`
	Family      string                    `json:"family"`
	Stage       Stage                     `json:"stage"`
	DerivedFrom []Ref                     `json:"derived_from,omitempty"`
	Summary     string                    `json:"summary"`
	Change      string                    `json:"change"`
	Ownership   binding.Ownership         `json:"ownership"`
	Requires    binding.StackCapabilities `json:"requires"`
	Interaction InteractionBoundary       `json:"interaction"`
}

func (definition Definition) Ref() Ref {
	return Ref{ID: definition.ID, Revision: definition.Revision}
}

// Identity is the runtime-safe reference to this exact definition.
func (definition Definition) Identity() binding.ArchitectureIdentity {
	return binding.ArchitectureIdentity{
		ID: definition.ID, Revision: definition.Revision,
		Fingerprint: definition.Fingerprint(),
	}
}

// Fingerprint protects immutable revisions from in-place catalog edits.
func (definition Definition) Fingerprint() string { return fingerprint(definition) }

// Topology names the provider placement implied by ownership. It is assembly
// machinery, not a model species: several architectures share each topology.
type Topology string

const (
	TopologyComponents Topology = "components"
	TopologySidecar    Topology = "sidecar"
	TopologyUpstream   Topology = "upstream"
)

// Topology derives which existing binding machinery can realise a definition.
func (definition Definition) Topology() (Topology, error) {
	owners := definition.Ownership.Effective()
	switch {
	case owners.Perception == binding.OwnerEngine &&
		owners.FastCognition == binding.OwnerEngine && owners.Action == binding.OwnerEngine:
		return TopologyComponents, nil
	case owners.Perception == binding.OwnerModel &&
		owners.FastCognition == binding.OwnerModel && owners.Action == binding.OwnerModel:
		return TopologySidecar, nil
	case owners.Perception == binding.OwnerRemote &&
		owners.FastCognition == binding.OwnerRemote && owners.Action == binding.OwnerRemote:
		return TopologyUpstream, nil
	default:
		return "", fmt.Errorf("architecture %s mixes perception, fast cognition, and action owners in a topology no binding can realise", definition.Ref())
	}
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)

// Validate checks one complete definition independently of its catalog lineage.
func (definition Definition) Validate() error {
	if !validName.MatchString(definition.ID) || !validName.MatchString(definition.Family) {
		return fmt.Errorf("architecture id and family must use lowercase dotted or dashed names, got %q and %q", definition.ID, definition.Family)
	}
	if definition.Revision <= 0 {
		return errors.New("architecture revision must be positive")
	}
	switch definition.Stage {
	case StageExperimental, StageCandidate, StageStable, StageRetired:
	default:
		return fmt.Errorf("architecture %s has unknown stage %q", definition.Ref(), definition.Stage)
	}
	if strings.TrimSpace(definition.Summary) == "" || strings.TrimSpace(definition.Change) == "" {
		return fmt.Errorf("architecture %s requires a summary and revision change note", definition.Ref())
	}
	if err := definition.Ownership.Validate(); err != nil {
		return fmt.Errorf("architecture %s: %w", definition.Ref(), err)
	}
	if _, err := definition.Topology(); err != nil {
		return err
	}
	if !definition.Requires.TurnGeneration {
		return fmt.Errorf("architecture %s must require turn generation", definition.Ref())
	}
	owners := definition.Ownership.Effective()
	if owners.Floor == binding.OwnerModel && !definition.Requires.NativeFloor {
		return fmt.Errorf("architecture %s selects a model floor without requiring native-floor", definition.Ref())
	}
	if owners.Interaction == binding.OwnerModel && !definition.Requires.NativeInteraction {
		return fmt.Errorf("architecture %s selects model interaction without requiring native-interaction", definition.Ref())
	}
	if err := definition.validateInteraction(); err != nil {
		return err
	}
	for _, parent := range definition.DerivedFrom {
		if !validName.MatchString(parent.ID) || parent.Revision <= 0 {
			return fmt.Errorf("architecture %s has invalid lineage reference %+v", definition.Ref(), parent)
		}
	}
	return nil
}

func (definition Definition) validateInteraction() error {
	owners := definition.Ownership.Effective()
	boundary := definition.Interaction
	if err := definition.validateControl(); err != nil {
		return err
	}
	if boundary.EvidenceCapabilities != nil &&
		len(boundary.EvidenceCapabilities.Names()) == 0 {
		return fmt.Errorf("architecture %s selects an empty interaction evidence vector", definition.Ref())
	}
	switch boundary.Mode {
	case InteractionPredicates:
		if owners.Interaction != binding.OwnerEngine || boundary.Evidence != EvidenceAcousticPredicates {
			return fmt.Errorf("architecture %s predicate interaction requires engine ownership and acoustic-predicates evidence", definition.Ref())
		}
		if boundary.EvidenceCapabilities != nil &&
			(!boundary.EvidenceCapabilities.AcousticActivity || !boundary.EvidenceCapabilities.SilenceClock) {
			return fmt.Errorf("architecture %s predicate interaction evidence requires acoustic activity and a silence clock", definition.Ref())
		}
	case InteractionTextPolicy:
		if owners.Interaction != binding.OwnerEngine || boundary.Evidence != EvidenceTranscript || !definition.Requires.Transcription {
			return fmt.Errorf("architecture %s text policy requires engine ownership, transcript evidence, and transcription", definition.Ref())
		}
		if boundary.EvidenceCapabilities != nil && !boundary.EvidenceCapabilities.Transcript {
			return fmt.Errorf("architecture %s text policy evidence must select a transcript", definition.Ref())
		}
	case InteractionComposed:
		if owners.Interaction != binding.OwnerEngine || boundary.Evidence != EvidenceTranscript || !definition.Requires.Transcription {
			return fmt.Errorf("architecture %s composed policy requires engine ownership, transcript evidence, and transcription", definition.Ref())
		}
		if boundary.EvidenceCapabilities != nil && !boundary.EvidenceCapabilities.Transcript {
			return fmt.Errorf("architecture %s composed policy evidence must select a transcript", definition.Ref())
		}
	case InteractionNative:
		if owners.Interaction != binding.OwnerModel || boundary.Evidence != EvidenceNativeMultimodal || boundary.Handoff != HandoffNone {
			return fmt.Errorf("architecture %s native interaction requires model ownership, native-multimodal evidence, and no act handoff", definition.Ref())
		}
		if boundary.EvidenceCapabilities != nil && !boundary.EvidenceCapabilities.NativeModelState {
			return fmt.Errorf("architecture %s native interaction evidence must select native model state", definition.Ref())
		}
	case InteractionRemote:
		if owners.Interaction != binding.OwnerRemote || boundary.Evidence != EvidenceRemoteMultimodal || boundary.Handoff != HandoffNone {
			return fmt.Errorf("architecture %s remote interaction requires remote ownership, remote-multimodal evidence, and no act handoff", definition.Ref())
		}
		if boundary.EvidenceCapabilities != nil && !boundary.EvidenceCapabilities.NativeModelState {
			return fmt.Errorf("architecture %s remote interaction evidence must select native model state", definition.Ref())
		}
	default:
		return fmt.Errorf("architecture %s has unknown interaction mode %q", definition.Ref(), boundary.Mode)
	}
	switch strings.TrimSpace(boundary.Transport) {
	case "in-process":
		if boundary.ProtocolVersion != 0 || boundary.Handoff != HandoffDirect {
			return fmt.Errorf("architecture %s in-process interaction requires direct handoff and no protocol version", definition.Ref())
		}
	case "sidecar":
		if boundary.Mode != InteractionNative &&
			(boundary.ProtocolVersion < 2 || boundary.Handoff != HandoffTyped || !definition.Requires.InteractionActs) {
			return fmt.Errorf("architecture %s external sidecar interaction requires protocol v2 typed acts", definition.Ref())
		}
	case "upstream":
		if boundary.Mode != InteractionRemote || boundary.Handoff != HandoffNone {
			return fmt.Errorf("architecture %s upstream transport is valid only for remote interaction", definition.Ref())
		}
	default:
		return fmt.Errorf("architecture %s has unknown interaction transport %q", definition.Ref(), boundary.Transport)
	}
	if owners.Interaction == binding.OwnerEngine && definition.Requires.NativeInteraction {
		if strings.TrimSpace(boundary.NativeSuppression) == "" || boundary.Transport != "sidecar" ||
			boundary.ProtocolVersion < 2 || boundary.Handoff != HandoffTyped || !definition.Requires.InteractionActs {
			return fmt.Errorf("architecture %s external control over native interaction requires a suppression contract and protocol v2 typed acts", definition.Ref())
		}
	} else if strings.TrimSpace(boundary.NativeSuppression) != "" {
		return fmt.Errorf("architecture %s declares native suppression without external control over a native interaction capability", definition.Ref())
	}
	return nil
}

func (definition Definition) validateControl() error {
	boundary := definition.Interaction
	if boundary.Control == nil {
		return nil
	}
	control := *boundary.Control
	names := control.Selectors.Names()
	if len(names) == 0 {
		return fmt.Errorf("architecture %s selects no interaction controller", definition.Ref())
	}
	switch control.Arbitration {
	case "single":
		if len(names) != 1 {
			return fmt.Errorf("architecture %s single-controller arbitration selected %s", definition.Ref(), strings.Join(names, ", "))
		}
	case "predicate-floor":
		selectors := control.Selectors
		if !selectors.Predicates || !selectors.TextPolicy || selectors.Native || selectors.Remote || len(names) != 2 {
			return fmt.Errorf("architecture %s predicate-floor arbitration requires exactly predicates plus text-policy", definition.Ref())
		}
	default:
		return fmt.Errorf("architecture %s has unknown interaction arbitration %q", definition.Ref(), control.Arbitration)
	}
	want := binding.InteractionControllers{}
	wantArbitration := "single"
	switch boundary.Mode {
	case InteractionPredicates:
		want.Predicates = true
	case InteractionTextPolicy:
		want.TextPolicy = true
	case InteractionComposed:
		want.Predicates, want.TextPolicy = true, true
		wantArbitration = "predicate-floor"
	case InteractionNative:
		want.Native = true
	case InteractionRemote:
		want.Remote = true
	}
	if control.Selectors != want || control.Arbitration != wantArbitration {
		return fmt.Errorf(
			"architecture %s mode %q requires controllers %s with %q arbitration, got %s with %q",
			definition.Ref(), boundary.Mode, strings.Join(want.Names(), ", "), wantArbitration,
			strings.Join(control.Selectors.Names(), ", "), control.Arbitration,
		)
	}
	return nil
}

// ValidateRuntime proves that a concrete composition can realise this
// definition. Extra capabilities remain visible and are explicitly allowed.
func (definition Definition) ValidateRuntime(ownership binding.Ownership, capabilities binding.StackCapabilities) error {
	if ownership.Effective() != definition.Ownership.Effective() {
		return fmt.Errorf("architecture %s ownership does not match the resolved runtime", definition.Ref())
	}
	if missing := capabilities.Missing(definition.Requires); len(missing) > 0 {
		return fmt.Errorf("architecture %s is missing required capabilities: %s", definition.Ref(), strings.Join(missing, ", "))
	}
	return nil
}

// ValidateStatus checks the complete live structural boundary after provider
// negotiation. This is stronger than validating launch flags: sidecar
// capabilities and the accepted act protocol exist only after the handshake.
func (definition Definition) ValidateStatus(status binding.Status) error {
	if err := definition.ValidateRuntime(status.Ownership, status.Stack); err != nil {
		return err
	}
	actual := status.Interaction
	expected := definition.Interaction
	if actual.Evidence != string(expected.Evidence) ||
		actual.Transport != expected.Transport ||
		actual.ProtocolVersion != expected.ProtocolVersion ||
		actual.ActHandoff != string(expected.Handoff) ||
		actual.NativeSuppression != expected.NativeSuppression {
		return fmt.Errorf(
			"architecture %s live interaction boundary differs: evidence=%q/%q "+
				"transport=%q/%q protocol=%d/%d handoff=%q/%q suppression=%q/%q",
			definition.Ref(), actual.Evidence, expected.Evidence,
			actual.Transport, expected.Transport, actual.ProtocolVersion, expected.ProtocolVersion,
			actual.ActHandoff, expected.Handoff, actual.NativeSuppression, expected.NativeSuppression,
		)
	}
	if expected.EvidenceCapabilities != nil &&
		actual.EvidenceCapabilities != *expected.EvidenceCapabilities {
		return fmt.Errorf(
			"architecture %s live interaction evidence capabilities differ: selected=%s definition=%s",
			definition.Ref(), strings.Join(actual.EvidenceCapabilities.Names(), ", "),
			strings.Join(expected.EvidenceCapabilities.Names(), ", "),
		)
	}
	if expected.Control != nil && actual.Control != *expected.Control {
		return fmt.Errorf(
			"architecture %s live interaction control differs: controllers=%s/%s arbitration=%q/%q",
			definition.Ref(), strings.Join(actual.Control.Selectors.Names(), ", "),
			strings.Join(expected.Control.Selectors.Names(), ", "),
			actual.Control.Arbitration, expected.Control.Arbitration,
		)
	}
	if expected.EvidenceCapabilities != nil {
		capabilities := *expected.EvidenceCapabilities
		if capabilities.Transcript && expected.UsesTextPolicy() &&
			(strings.TrimSpace(actual.Recognizer) == "" ||
				strings.TrimSpace(actual.RecognizerRevision) == "") {
			return fmt.Errorf("architecture %s selects transcript policy evidence without a live recognizer identity", definition.Ref())
		}
		if capabilities.SpeakerIdentity &&
			(strings.TrimSpace(status.SpeakerIdentity) == "" ||
				strings.TrimSpace(status.SpeakerIdentityRevision) == "") {
			return fmt.Errorf("architecture %s selects speaker identity evidence without a live adapter identity", definition.Ref())
		}
		if capabilities.VisualDescription && strings.TrimSpace(status.VisualNarrator) == "" {
			return fmt.Errorf("architecture %s selects visual-description evidence without a live narrator identity", definition.Ref())
		}
	}
	return nil
}

// Catalog is the complete immutable architecture graph known to a deployment.
type Catalog struct {
	Version       int          `json:"version"`
	Architectures []Definition `json:"architectures"`
}

// Read loads a strict external catalog.
func Read(path string) (Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return Catalog{}, errors.New("an architecture catalog path is required")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, err
	}
	return Decode(payload)
}

// Decode strictly decodes and validates a catalog.
func Decode(payload []byte) (Catalog, error) {
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode architecture catalog: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Catalog{}, errors.New("decode architecture catalog: trailing JSON value")
		}
		return Catalog{}, fmt.Errorf("decode architecture catalog trailing content: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// Default returns a fresh copy of the repository-owned catalog.
func Default() Catalog {
	catalog, err := Decode(defaultCatalogJSON)
	if err != nil {
		panic(fmt.Sprintf("invalid embedded architecture catalog: %v", err))
	}
	return catalog
}

// Validate checks identities, immutable revision lineage, and graph cycles.
func (catalog Catalog) Validate() error {
	if catalog.Version != CatalogVersion {
		return fmt.Errorf("architecture catalog version must be %d, got %d", CatalogVersion, catalog.Version)
	}
	if len(catalog.Architectures) == 0 {
		return errors.New("architecture catalog is empty")
	}
	definitions := make(map[Ref]Definition, len(catalog.Architectures))
	for _, definition := range catalog.Architectures {
		if err := definition.Validate(); err != nil {
			return err
		}
		ref := definition.Ref()
		if _, duplicate := definitions[ref]; duplicate {
			return fmt.Errorf("duplicate architecture revision %s", ref)
		}
		definitions[ref] = definition
	}
	for ref, definition := range definitions {
		for _, parent := range definition.DerivedFrom {
			if _, exists := definitions[parent]; !exists {
				return fmt.Errorf("architecture %s derives from missing %s", ref, parent)
			}
		}
		if ref.Revision > 1 {
			continues := false
			for _, parent := range definition.DerivedFrom {
				if parent.ID == ref.ID && parent.Revision < ref.Revision {
					continues = true
				}
			}
			if !continues {
				return fmt.Errorf("architecture %s is a later revision without lineage to an earlier revision of the same id", ref)
			}
		}
	}
	visiting, visited := map[Ref]bool{}, map[Ref]bool{}
	var visit func(Ref) error
	visit = func(ref Ref) error {
		if visiting[ref] {
			return fmt.Errorf("architecture lineage contains a cycle at %s", ref)
		}
		if visited[ref] {
			return nil
		}
		visiting[ref] = true
		for _, parent := range definitions[ref].DerivedFrom {
			if err := visit(parent); err != nil {
				return err
			}
		}
		delete(visiting, ref)
		visited[ref] = true
		return nil
	}
	for ref := range definitions {
		if err := visit(ref); err != nil {
			return err
		}
	}
	return nil
}

// Resolve looks up one exact id@revision.
func (catalog Catalog) Resolve(value string) (Definition, error) {
	ref, err := ParseRef(value)
	if err != nil {
		return Definition{}, err
	}
	for _, definition := range catalog.Architectures {
		if definition.Ref() == ref {
			return definition, nil
		}
	}
	return Definition{}, fmt.Errorf("architecture catalog has no definition %s", ref)
}

// LookupIdentity resolves and verifies an attested architecture identity.
func (catalog Catalog) LookupIdentity(identity binding.ArchitectureIdentity) (Definition, error) {
	definition, err := catalog.Resolve(Ref{ID: identity.ID, Revision: identity.Revision}.String())
	if err != nil {
		return Definition{}, err
	}
	if identity.Fingerprint != definition.Fingerprint() {
		return Definition{}, fmt.Errorf("architecture %s fingerprint differs from the catalog", definition.Ref())
	}
	return definition, nil
}

// Sorted returns definitions in stable family/id/revision order.
func (catalog Catalog) Sorted() []Definition {
	result := slices.Clone(catalog.Architectures)
	sort.Slice(result, func(left, right int) bool {
		if result[left].Family != result[right].Family {
			return result[left].Family < result[right].Family
		}
		if result[left].ID != result[right].ID {
			return result[left].ID < result[right].ID
		}
		return result[left].Revision < result[right].Revision
	})
	return result
}

// Fingerprint identifies the whole catalog snapshot.
func (catalog Catalog) Fingerprint() string { return fingerprint(catalog) }

func fingerprint(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}
