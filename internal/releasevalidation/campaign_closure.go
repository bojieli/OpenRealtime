package releasevalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

var campaignNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

const (
	CampaignClosureFormat          = "openrealtime.behavioral-campaign-closure"
	CampaignClosureVersion         = 1
	CampaignRunSpecFormat          = "openrealtime.behavioral-run-spec"
	CampaignRunSpecVersion         = 1
	CampaignInventoryFormat        = "openrealtime.behavioral-task-inventory"
	CampaignInventoryVersion       = 1
	CampaignScorerFormat           = "openrealtime.behavioral-scorer"
	CampaignScorerVersion          = 1
	CampaignArtifactReceiptFormat  = "openrealtime.behavioral-artifact-receipt"
	CampaignArtifactReceiptVersion = 1

	maximumCampaignClosureBytes   = 1 << 20
	maximumCampaignManifestBytes  = 64 << 20
	maximumCampaignSourceReceipts = 64
	maximumCampaignLineageDepth   = 10_000
)

// CampaignArtifact identifies one typed, immutable artifact. Path is a
// locator resolved relative to the closure file and is deliberately excluded
// from ArtifactSHA256's meaning: identity is the digest, never the filename.
type CampaignArtifact struct {
	Format         string `json:"format"`
	FormatVersion  int    `json:"format_version"`
	Path           string `json:"path"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

// CampaignRunSpec retains the behavior-affecting invocation as canonical
// argv, public environment bindings, and secret-free endpoint identities.
// Every collection is unique and sorted; positional argv order is preserved.
type CampaignRunSpec struct {
	Format           string                `json:"format"`
	FormatVersion    int                   `json:"format_version"`
	SuiteID          string                `json:"suite_id"`
	CampaignID       string                `json:"campaign_id"`
	WorkingDirectory string                `json:"working_directory"`
	Arguments        []string              `json:"arguments"`
	Environment      []CampaignNamedValue  `json:"environment,omitempty"`
	Endpoints        []CampaignNamedDigest `json:"endpoints,omitempty"`
}

type CampaignNamedValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type CampaignNamedDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// CampaignTaskInventory is the pre-run exact population. Task IDs must be
// unique and sorted. This prevents a result from choosing its own population.
type CampaignTaskInventory struct {
	Format             string   `json:"format"`
	FormatVersion      int      `json:"format_version"`
	SuiteID            string   `json:"suite_id"`
	ExpectedPopulation int      `json:"expected_population"`
	TaskIDs            []string `json:"task_ids"`
}

// CampaignScorerManifest identifies the deterministic code and all immutable
// inputs which own TaskOutcome.Passed. The named inputs are unique and sorted.
type CampaignScorerManifest struct {
	Format               string                `json:"format"`
	FormatVersion        int                   `json:"format_version"`
	SuiteID              string                `json:"suite_id"`
	Identity             string                `json:"identity"`
	Revision             string                `json:"revision"`
	ImplementationSHA256 string                `json:"implementation_sha256"`
	Inputs               []CampaignNamedDigest `json:"inputs,omitempty"`
}

// CampaignArtifactReceipt is the smallest common, externally verifiable
// source-evidence receipt. Suite-specific receipts can be wrapped by retaining
// their raw digest and semantic identity here. Acceptance verifies the wrapper
// and the exact raw receipt bytes; suite owners remain responsible for replay.
type CampaignArtifactReceipt struct {
	Format                string `json:"format"`
	FormatVersion         int    `json:"format_version"`
	Kind                  string `json:"kind"`
	ArtifactFormat        string `json:"artifact_format"`
	ArtifactPath          string `json:"artifact_path"`
	ArtifactSHA256        string `json:"artifact_sha256"`
	PortableReceiptSHA256 string `json:"portable_receipt_sha256"`
	ReceiptSHA256         string `json:"receipt_sha256"`
}

// CampaignClosure is a post-run, create-only commit record. ClosureSHA256 is
// calculated over the canonical JSON representation with that field empty.
// Predecessors recursively close every declared failed/full and focused run;
// the current closure must correspond to the last frozen final_full row.
type CampaignClosure struct {
	Format                     string             `json:"format"`
	FormatVersion              int                `json:"format_version"`
	SuiteID                    string             `json:"suite_id"`
	CampaignID                 string             `json:"campaign_id"`
	CandidateID                string             `json:"candidate_id"`
	CandidateSHA256            string             `json:"candidate_sha256"`
	Revision                   string             `json:"revision"`
	ExecutableSHA256           string             `json:"executable_sha256"`
	Machine                    bench.Machine      `json:"machine"`
	ExecutionRequirementSHA256 string             `json:"execution_requirement_sha256"`
	RunSpec                    CampaignArtifact   `json:"run_spec"`
	Inventory                  CampaignArtifact   `json:"inventory"`
	SourceReceipts             []CampaignArtifact `json:"source_receipts"`
	Result                     CampaignArtifact   `json:"result"`
	Scorer                     CampaignArtifact   `json:"scorer"`
	ExpectedPopulation         int                `json:"expected_population"`
	TaskPopulationSHA256       string             `json:"task_population_sha256"`
	Predecessors               []CampaignArtifact `json:"predecessors,omitempty"`
	ClosureSHA256              string             `json:"closure_sha256"`
}

// CampaignClosurePublication supplies caller-owned artifact paths. Publishing
// validates and reopens all inputs before atomically creating the closure.
type CampaignClosurePublication struct {
	Closure CampaignClosure
	Path    string
}

// VerifiedCampaignClosure contains the verified final result and closure.
type VerifiedCampaignClosure struct {
	Closure        CampaignClosure
	Result         bench.Result
	SourceReceipts []CampaignArtifactReceipt
	Predecessors   []CampaignClosure
}

// LoadCampaignClosure loads a strict canonical closure. Artifact paths remain
// relative to the closure's directory until VerifyCampaignClosure resolves
// them beneath that directory.
func LoadCampaignClosure(path string) (CampaignClosure, error) {
	payload, err := readBoundedRegularFile(path, maximumCampaignClosureBytes, "campaign closure")
	if err != nil {
		return CampaignClosure{}, err
	}
	var closure CampaignClosure
	if err := decodeCampaignCanonical(payload, &closure, maximumCampaignClosureBytes); err != nil {
		return CampaignClosure{}, fmt.Errorf("decode campaign closure: %w", err)
	}
	if err := closure.validateStructure(); err != nil {
		return CampaignClosure{}, err
	}
	return closure, nil
}

// LoadCampaignClosureDraft reads the strict canonical input to the create-only
// publisher. A draft must carry an explicitly empty closure_sha256; publishing
// calculates it only after every referenced artifact verifies.
func LoadCampaignClosureDraft(path string) (CampaignClosure, error) {
	payload, err := readBoundedRegularFile(path, maximumCampaignClosureBytes, "campaign closure draft")
	if err != nil {
		return CampaignClosure{}, err
	}
	var closure CampaignClosure
	if err := decodeCampaignCanonical(payload, &closure, maximumCampaignClosureBytes); err != nil {
		return CampaignClosure{}, fmt.Errorf("decode campaign closure draft: %w", err)
	}
	if closure.ClosureSHA256 != "" {
		return CampaignClosure{}, errors.New("campaign closure draft already carries a closure digest")
	}
	if err := closure.validateStructureWithoutDigest(); err != nil {
		return CampaignClosure{}, err
	}
	return closure, nil
}

// MarshalCampaignClosureDraft emits the only accepted publisher input form.
func MarshalCampaignClosureDraft(closure CampaignClosure) ([]byte, error) {
	if closure.ClosureSHA256 != "" {
		return nil, errors.New("campaign closure draft already carries a closure digest")
	}
	if err := closure.validateStructureWithoutDigest(); err != nil {
		return nil, err
	}
	return marshalCampaignCanonical(closure)
}

// PublishCampaignClosure verifies the complete campaign before writing a
// canonical closure with O_EXCL. No existing receipt is ever replaced.
func PublishCampaignClosure(publication CampaignClosurePublication) (CampaignClosure, error) {
	if strings.TrimSpace(publication.Path) == "" || publication.Path == "-" {
		return CampaignClosure{}, errors.New("campaign closure must name a create-only file")
	}
	closure := publication.Closure
	closure.ClosureSHA256 = ""
	if err := closure.validateStructureWithoutDigest(); err != nil {
		return CampaignClosure{}, err
	}
	closure.ClosureSHA256 = campaignClosureDigest(closure)
	if _, err := verifyCampaignClosure(filepath.Clean(publication.Path), closure, nil, 0); err != nil {
		return CampaignClosure{}, err
	}
	payload, err := marshalCampaignCanonical(closure)
	if err != nil {
		return CampaignClosure{}, err
	}
	if err := writeCampaignCreateOnly(publication.Path, payload); err != nil {
		return CampaignClosure{}, err
	}
	opened, err := LoadCampaignClosure(publication.Path)
	if err != nil {
		return CampaignClosure{}, fmt.Errorf("reopen campaign closure: %w", err)
	}
	if _, err := VerifyCampaignClosure(publication.Path); err != nil {
		return CampaignClosure{}, fmt.Errorf("reverify published campaign closure: %w", err)
	}
	return opened, nil
}

// VerifyCampaignClosure reopens all bound artifacts, recursively verifies the
// predecessor chain, and returns the exact final benchmark result.
func VerifyCampaignClosure(path string) (VerifiedCampaignClosure, error) {
	closure, err := LoadCampaignClosure(path)
	if err != nil {
		return VerifiedCampaignClosure{}, err
	}
	return verifyCampaignClosure(filepath.Clean(path), closure, nil, 0)
}

func verifyCampaignClosure(
	path string, closure CampaignClosure, active map[string]bool, depth int,
) (VerifiedCampaignClosure, error) {
	if depth > maximumCampaignLineageDepth {
		return VerifiedCampaignClosure{}, errors.New("campaign closure predecessor depth exceeds limit")
	}
	if err := closure.validateStructure(); err != nil {
		return VerifiedCampaignClosure{}, err
	}
	if active == nil {
		active = make(map[string]bool)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return VerifiedCampaignClosure{}, errors.New("resolve campaign closure path")
	}
	if active[absolute] {
		return VerifiedCampaignClosure{}, errors.New("campaign closure predecessor cycle")
	}
	active[absolute] = true
	defer delete(active, absolute)

	directory := filepath.Dir(absolute)
	runSpecPayload, err := readCampaignArtifact(directory, closure.RunSpec)
	if err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("verify run specification: %w", err)
	}
	var runSpec CampaignRunSpec
	if err := decodeCampaignCanonical(runSpecPayload, &runSpec, maximumCampaignManifestBytes); err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("decode run specification: %w", err)
	}
	if err := runSpec.Validate(); err != nil {
		return VerifiedCampaignClosure{}, err
	}
	if runSpec.SuiteID != closure.SuiteID || runSpec.CampaignID != closure.CampaignID {
		return VerifiedCampaignClosure{}, errors.New("run specification identity differs from campaign closure")
	}

	inventoryPayload, err := readCampaignArtifact(directory, closure.Inventory)
	if err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("verify task inventory: %w", err)
	}
	var inventory CampaignTaskInventory
	if err := decodeCampaignCanonical(inventoryPayload, &inventory, maximumCampaignManifestBytes); err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("decode task inventory: %w", err)
	}
	if err := inventory.Validate(); err != nil {
		return VerifiedCampaignClosure{}, err
	}
	if inventory.SuiteID != closure.SuiteID || inventory.ExpectedPopulation != closure.ExpectedPopulation {
		return VerifiedCampaignClosure{}, errors.New("task inventory identity or population differs from campaign closure")
	}

	scorerPayload, err := readCampaignArtifact(directory, closure.Scorer)
	if err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("verify scorer manifest: %w", err)
	}
	var scorer CampaignScorerManifest
	if err := decodeCampaignCanonical(scorerPayload, &scorer, maximumCampaignManifestBytes); err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("decode scorer manifest: %w", err)
	}
	if err := scorer.Validate(); err != nil {
		return VerifiedCampaignClosure{}, err
	}
	if scorer.SuiteID != closure.SuiteID {
		return VerifiedCampaignClosure{}, errors.New("scorer manifest suite differs from campaign closure")
	}

	sourceReceipts := make([]CampaignArtifactReceipt, 0, len(closure.SourceReceipts))
	for index, source := range closure.SourceReceipts {
		payload, err := readCampaignArtifact(directory, source)
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("verify source receipt %d: %w", index, err)
		}
		var receipt CampaignArtifactReceipt
		if err := decodeCampaignCanonical(payload, &receipt, maximumCampaignManifestBytes); err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("decode source receipt %d: %w", index, err)
		}
		if err := receipt.Validate(); err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d: %w", index, err)
		}
		rawPath, err := resolveCampaignPath(directory, receipt.ArtifactPath)
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d artifact locator: %w", index, err)
		}
		raw, err := readBoundedRegularFile(rawPath, maximumCampaignManifestBytes, "source evidence receipt")
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d artifact: %w", index, err)
		}
		if digestBytes(raw) != receipt.ArtifactSHA256 {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d artifact digest mismatch", index)
		}
		portable, err := campaignPortableReceiptDigest(raw)
		if err != nil || portable != receipt.PortableReceiptSHA256 {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d portable digest mismatch", index)
		}
		boundResult, err := verifyCampaignSourceReceipt(
			context.Background(), rawPath, raw, receipt,
		)
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("source receipt %d independent verification: %w", index, err)
		}
		if boundResult != closure.Result.ArtifactSHA256 {
			return VerifiedCampaignClosure{}, fmt.Errorf(
				"source receipt %d authenticates a different deterministic result", index)
		}
		sourceReceipts = append(sourceReceipts, receipt)
	}

	resultPath, err := campaignArtifactPath(directory, closure.Result)
	if err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("resolve campaign result: %w", err)
	}
	resultPayload, err := readBoundedRegularFile(
		resultPath, maximumBehavioralResultBytes, "campaign result",
	)
	if err != nil {
		return VerifiedCampaignClosure{}, err
	}
	result, digest, err := decodeBehavioralResult(resultPayload, closure.Result.Format)
	if err != nil {
		return VerifiedCampaignClosure{}, err
	}
	if digest != closure.Result.ArtifactSHA256 {
		return VerifiedCampaignClosure{}, errors.New("campaign result digest differs from closure")
	}
	if len(result.Tasks) != closure.ExpectedPopulation || result.Expected != closure.ExpectedPopulation {
		return VerifiedCampaignClosure{}, errors.New("campaign result population differs from closure")
	}
	derived := result
	derived.Finish()
	if !equalSummaries(result.Summary, derived.Summary) {
		return VerifiedCampaignClosure{}, errors.New("campaign result stored summary differs from its task rows")
	}
	if err := result.Reportable(); err != nil {
		return VerifiedCampaignClosure{}, fmt.Errorf("campaign result is not reportable: %w", err)
	}
	if err := validateCampaignResultIdentity(closure, result); err != nil {
		return VerifiedCampaignClosure{}, err
	}
	resultIDs := make([]string, len(result.Tasks))
	for index, task := range result.Tasks {
		resultIDs[index] = task.ID
	}
	sort.Strings(resultIDs)
	if !slices.Equal(resultIDs, inventory.TaskIDs) {
		return VerifiedCampaignClosure{}, errors.New("campaign result tasks differ from pre-run inventory")
	}
	if digestTaskPopulation(result.Tasks) != closure.TaskPopulationSHA256 {
		return VerifiedCampaignClosure{}, errors.New("campaign task population digest differs from closure")
	}

	predecessorClosures := make([]CampaignClosure, 0, len(closure.Predecessors))
	seenCampaigns := map[string]bool{closure.CampaignID: true}
	for index, predecessor := range closure.Predecessors {
		predecessorPath, err := campaignArtifactPath(directory, predecessor)
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("resolve predecessor %d: %w", index, err)
		}
		payload, err := readBoundedRegularFile(predecessorPath, maximumCampaignClosureBytes, "predecessor closure")
		if err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("read predecessor %d: %w", index, err)
		}
		if digestBytes(payload) != predecessor.ArtifactSHA256 {
			return VerifiedCampaignClosure{}, fmt.Errorf("predecessor %d artifact digest mismatch", index)
		}
		var prior CampaignClosure
		if err := decodeCampaignCanonical(payload, &prior, maximumCampaignClosureBytes); err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("decode predecessor %d: %w", index, err)
		}
		if prior.SuiteID != closure.SuiteID {
			return VerifiedCampaignClosure{}, fmt.Errorf("predecessor %d belongs to a different suite", index)
		}
		if seenCampaigns[prior.CampaignID] {
			return VerifiedCampaignClosure{}, errors.New("campaign closure repeats a campaign identity")
		}
		seenCampaigns[prior.CampaignID] = true
		if _, err := verifyCampaignClosure(predecessorPath, prior, active, depth+1); err != nil {
			return VerifiedCampaignClosure{}, fmt.Errorf("verify predecessor %d: %w", index, err)
		}
		predecessorClosures = append(predecessorClosures, prior)
	}
	return VerifiedCampaignClosure{
		Closure: closure, Result: result, SourceReceipts: sourceReceipts,
		Predecessors: predecessorClosures,
	}, nil
}

func validateCampaignResultIdentity(closure CampaignClosure, result bench.Result) error {
	if result.Provenance.Revision != closure.Revision ||
		"sha256:"+result.Provenance.ExecutableSHA256 != closure.ExecutableSHA256 ||
		!reflect.DeepEqual(result.Provenance.Machine, closure.Machine) || result.Provenance.Modified {
		return errors.New("campaign result build or machine differs from closure")
	}
	if !result.Cell.Execution.Required() || result.Cell.Execution.Kind != bench.ExecutionGraphNative {
		return errors.New("campaign result is not graph-native")
	}
	requirement, err := bench.MarshalExecutionRequirement(result.Cell.Execution)
	if err != nil {
		return fmt.Errorf("campaign result execution requirement: %w", err)
	}
	if digestBytes(requirement) != closure.ExecutionRequirementSHA256 {
		return errors.New("campaign result execution requirement differs from closure")
	}
	return nil
}

func (closure CampaignClosure) validateStructure() error {
	if err := closure.validateStructureWithoutDigest(); err != nil {
		return err
	}
	if !sha256Pattern.MatchString(closure.ClosureSHA256) || campaignClosureDigest(closure) != closure.ClosureSHA256 {
		return errors.New("campaign closure digest is absent or incorrect")
	}
	return nil
}

func (closure CampaignClosure) validateStructureWithoutDigest() error {
	if closure.Format != CampaignClosureFormat || closure.FormatVersion != CampaignClosureVersion {
		return errors.New("campaign closure format is unsupported")
	}
	if !behavioralIDPattern.MatchString(closure.SuiteID) || !behavioralIDPattern.MatchString(closure.CampaignID) ||
		!behavioralIDPattern.MatchString(closure.CandidateID) {
		return errors.New("campaign closure has an invalid suite, campaign, or candidate ID")
	}
	if !sha256Pattern.MatchString(closure.CandidateSHA256) || !revisionPattern.MatchString(closure.Revision) ||
		!sha256Pattern.MatchString(closure.ExecutableSHA256) ||
		!sha256Pattern.MatchString(closure.ExecutionRequirementSHA256) ||
		!sha256Pattern.MatchString(closure.TaskPopulationSHA256) {
		return errors.New("campaign closure has a noncanonical identity digest")
	}
	if err := validateFrozenMachine(closure.Machine); err != nil {
		return fmt.Errorf("campaign closure machine: %w", err)
	}
	if closure.ExpectedPopulation <= 0 || closure.ExpectedPopulation > 1_000_000 {
		return errors.New("campaign closure has an invalid expected population")
	}
	artifacts := []struct {
		name    string
		item    CampaignArtifact
		format  string
		version int
	}{
		{"run specification", closure.RunSpec, CampaignRunSpecFormat, CampaignRunSpecVersion},
		{"task inventory", closure.Inventory, CampaignInventoryFormat, CampaignInventoryVersion},
		{"scorer manifest", closure.Scorer, CampaignScorerFormat, CampaignScorerVersion},
	}
	if closure.Result.Format != ResultKindBench && closure.Result.Format != ResultKindArchitecture {
		return errors.New("campaign closure has an invalid result kind")
	}
	if closure.Result.FormatVersion != 1 {
		return errors.New("campaign closure result format version is unsupported")
	}
	if err := closure.Result.validate(); err != nil {
		return fmt.Errorf("campaign result: %w", err)
	}
	for _, artifact := range artifacts {
		if artifact.item.Format != artifact.format || artifact.item.FormatVersion != artifact.version {
			return fmt.Errorf("campaign %s has an incorrect typed artifact identity", artifact.name)
		}
		if err := artifact.item.validate(); err != nil {
			return fmt.Errorf("campaign %s: %w", artifact.name, err)
		}
	}
	if len(closure.SourceReceipts) == 0 || len(closure.SourceReceipts) > maximumCampaignSourceReceipts {
		return errors.New("campaign closure has no bounded source receipt set")
	}
	previousSource := ""
	for index, artifact := range closure.SourceReceipts {
		if artifact.Format != CampaignArtifactReceiptFormat || artifact.FormatVersion != CampaignArtifactReceiptVersion {
			return fmt.Errorf("source receipt %d has an incorrect typed artifact identity", index)
		}
		if err := artifact.validate(); err != nil {
			return fmt.Errorf("source receipt %d: %w", index, err)
		}
		if previousSource != "" && previousSource >= artifact.Path {
			return errors.New("campaign source receipts must be uniquely sorted by path")
		}
		previousSource = artifact.Path
	}
	if len(closure.Predecessors) > maximumCampaignLineageDepth {
		return errors.New("campaign closure has too many predecessors")
	}
	seenPredecessors := make(map[string]bool, len(closure.Predecessors))
	for index, artifact := range closure.Predecessors {
		if artifact.Format != CampaignClosureFormat || artifact.FormatVersion != CampaignClosureVersion {
			return fmt.Errorf("predecessor %d has an incorrect typed artifact identity", index)
		}
		if err := artifact.validate(); err != nil {
			return fmt.Errorf("predecessor %d: %w", index, err)
		}
		if seenPredecessors[artifact.Path] {
			return errors.New("campaign predecessors must have unique paths")
		}
		seenPredecessors[artifact.Path] = true
	}
	return nil
}

func (artifact CampaignArtifact) validate() error {
	if strings.TrimSpace(artifact.Format) == "" || len(artifact.Format) > 256 || artifact.FormatVersion <= 0 ||
		artifact.FormatVersion > 1_000_000 || !sha256Pattern.MatchString(artifact.ArtifactSHA256) {
		return errors.New("artifact has an invalid format, version, or digest")
	}
	_, err := validateCampaignRelativePath(artifact.Path)
	return err
}

func (spec CampaignRunSpec) Validate() error {
	if spec.Format != CampaignRunSpecFormat || spec.FormatVersion != CampaignRunSpecVersion ||
		!behavioralIDPattern.MatchString(spec.SuiteID) || !behavioralIDPattern.MatchString(spec.CampaignID) ||
		len(spec.Arguments) == 0 || len(spec.Arguments) > 4096 {
		return errors.New("campaign run specification has an invalid header or argument count")
	}
	if err := validateCampaignText(spec.WorkingDirectory, 4096, false); err != nil {
		return fmt.Errorf("campaign working directory: %w", err)
	}
	for index, argument := range spec.Arguments {
		if err := validateCampaignText(argument, 64<<10, index != 0); err != nil {
			return fmt.Errorf("campaign run argument: %w", err)
		}
	}
	if err := validateCampaignNamedValues(spec.Environment); err != nil {
		return err
	}
	return validateCampaignNamedDigests(spec.Endpoints)
}

func (inventory CampaignTaskInventory) Validate() error {
	if inventory.Format != CampaignInventoryFormat || inventory.FormatVersion != CampaignInventoryVersion ||
		!behavioralIDPattern.MatchString(inventory.SuiteID) || inventory.ExpectedPopulation <= 0 ||
		inventory.ExpectedPopulation > 1_000_000 || len(inventory.TaskIDs) != inventory.ExpectedPopulation {
		return errors.New("campaign task inventory has an invalid header or population")
	}
	previous := ""
	for _, id := range inventory.TaskIDs {
		if err := validateCampaignText(id, 4<<10, false); err != nil {
			return fmt.Errorf("campaign task inventory ID: %w", err)
		}
		if previous != "" && previous >= id {
			return errors.New("campaign task inventory IDs must be unique and sorted")
		}
		previous = id
	}
	return nil
}

func (scorer CampaignScorerManifest) Validate() error {
	if scorer.Format != CampaignScorerFormat || scorer.FormatVersion != CampaignScorerVersion ||
		!behavioralIDPattern.MatchString(scorer.SuiteID) {
		return errors.New("campaign scorer manifest has an invalid header")
	}
	if err := validateCampaignText(scorer.Identity, 4096, false); err != nil {
		return fmt.Errorf("campaign scorer identity: %w", err)
	}
	if err := validateCampaignText(scorer.Revision, 4096, false); err != nil {
		return fmt.Errorf("campaign scorer revision: %w", err)
	}
	if !sha256Pattern.MatchString(scorer.ImplementationSHA256) {
		return errors.New("campaign scorer implementation digest is invalid")
	}
	return validateCampaignNamedDigests(scorer.Inputs)
}

func (receipt CampaignArtifactReceipt) Validate() error {
	if receipt.Format != CampaignArtifactReceiptFormat || receipt.FormatVersion != CampaignArtifactReceiptVersion ||
		!behavioralIDPattern.MatchString(receipt.Kind) || !sha256Pattern.MatchString(receipt.ArtifactSHA256) ||
		!sha256Pattern.MatchString(receipt.PortableReceiptSHA256) ||
		!sha256Pattern.MatchString(receipt.ReceiptSHA256) {
		return errors.New("campaign artifact receipt has an invalid header or digest")
	}
	if err := validateCampaignText(receipt.ArtifactFormat, 256, false); err != nil {
		return fmt.Errorf("campaign artifact receipt format: %w", err)
	}
	if _, err := validateCampaignRelativePath(receipt.ArtifactPath); err != nil {
		return fmt.Errorf("campaign artifact receipt locator: %w", err)
	}
	copy := receipt
	copy.ReceiptSHA256 = ""
	payload, err := marshalCampaignCanonical(copy)
	if err != nil || digestBytes(payload) != receipt.ReceiptSHA256 {
		return errors.New("campaign artifact receipt digest is absent or incorrect")
	}
	return nil
}

// SealCampaignArtifactReceipt calculates the receipt's self digest.
func SealCampaignArtifactReceipt(receipt CampaignArtifactReceipt) (CampaignArtifactReceipt, error) {
	receipt.ReceiptSHA256 = ""
	if receipt.Format != CampaignArtifactReceiptFormat || receipt.FormatVersion != CampaignArtifactReceiptVersion ||
		!behavioralIDPattern.MatchString(receipt.Kind) || !sha256Pattern.MatchString(receipt.ArtifactSHA256) ||
		!sha256Pattern.MatchString(receipt.PortableReceiptSHA256) {
		return CampaignArtifactReceipt{}, errors.New("campaign artifact receipt has an invalid header or artifact digest")
	}
	if err := validateCampaignText(receipt.ArtifactFormat, 256, false); err != nil {
		return CampaignArtifactReceipt{}, err
	}
	if _, err := validateCampaignRelativePath(receipt.ArtifactPath); err != nil {
		return CampaignArtifactReceipt{}, err
	}
	payload, err := marshalCampaignCanonical(receipt)
	if err != nil {
		return CampaignArtifactReceipt{}, err
	}
	receipt.ReceiptSHA256 = digestBytes(payload)
	return receipt, receipt.Validate()
}

func campaignPortableReceiptDigest(payload []byte) (string, error) {
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumCampaignManifestBytes, MaxDepth: 128,
		MaxTokens: 8_000_000, MaxObjectMembers: 1_000_000,
		MaxArrayElements: 2_000_000, MaxTotalKeyBytes: 64 << 20,
		MaxWorkBytes: 512 << 20,
	}); err != nil {
		return "", err
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&object); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errors.New("source receipt has trailing JSON")
	}
	encoded, found := object["receipt_sha256"]
	if !found {
		return "", errors.New("source receipt has no portable receipt digest")
	}
	var digest string
	if err := json.Unmarshal(encoded, &digest); err != nil || !sha256Pattern.MatchString(digest) {
		return "", errors.New("source receipt has an invalid portable receipt digest")
	}
	return digest, nil
}

// MarshalCampaignArtifact emits the canonical representation required by the
// closure contract. Supported values are run specs, inventories, scorers, and
// common source receipt wrappers.
func MarshalCampaignArtifact(value any) ([]byte, error) {
	switch item := value.(type) {
	case CampaignRunSpec:
		if err := item.Validate(); err != nil {
			return nil, err
		}
	case CampaignTaskInventory:
		if err := item.Validate(); err != nil {
			return nil, err
		}
	case CampaignScorerManifest:
		if err := item.Validate(); err != nil {
			return nil, err
		}
	case CampaignArtifactReceipt:
		if err := item.Validate(); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported campaign artifact type")
	}
	return marshalCampaignCanonical(value)
}

func validateCampaignNamedValues(values []CampaignNamedValue) error {
	if len(values) > 4096 {
		return errors.New("campaign run environment has too many entries")
	}
	previous := ""
	for _, item := range values {
		if !campaignNamePattern.MatchString(item.Name) {
			return errors.New("campaign run environment has an invalid name")
		}
		if previous != "" && previous >= item.Name {
			return errors.New("campaign run environment must be unique and sorted")
		}
		if err := validateCampaignText(item.Value, 64<<10, true); err != nil {
			return fmt.Errorf("campaign run environment %q: %w", item.Name, err)
		}
		previous = item.Name
	}
	return nil
}

func validateCampaignNamedDigests(values []CampaignNamedDigest) error {
	if len(values) > 4096 {
		return errors.New("campaign named digest set has too many entries")
	}
	previous := ""
	for _, item := range values {
		if !campaignNamePattern.MatchString(item.Name) || !sha256Pattern.MatchString(item.SHA256) {
			return errors.New("campaign named digest has an invalid name or digest")
		}
		if previous != "" && previous >= item.Name {
			return errors.New("campaign named digests must be unique and sorted")
		}
		previous = item.Name
	}
	return nil
}

func validateCampaignText(value string, maximum int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return errors.New("value is empty, oversized, or noncanonical text")
	}
	for _, symbol := range value {
		if unicode.IsControl(symbol) {
			return errors.New("value contains a control character")
		}
	}
	return nil
}

func digestTaskPopulation(tasks []bench.TaskOutcome) string {
	ordered := slices.Clone(tasks)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	payload, err := json.Marshal(ordered)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func campaignClosureDigest(closure CampaignClosure) string {
	closure.ClosureSHA256 = ""
	payload, err := marshalCampaignCanonical(closure)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func decodeCampaignCanonical(payload []byte, target any, maximum int64) error {
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: int(maximum), MaxDepth: 64, MaxTokens: 2_000_000,
		MaxObjectMembers: 100_000, MaxArrayElements: 1_000_000,
		MaxTotalKeyBytes: 32 << 20, MaxWorkBytes: 256 << 20,
	}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	canonical, err := marshalCampaignCanonical(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, canonical) {
		return errors.New("artifact is not canonical JSON")
	}
	return nil
}

func marshalCampaignCanonical(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	return payload, nil
}

func readCampaignArtifact(directory string, artifact CampaignArtifact) ([]byte, error) {
	path, err := campaignArtifactPath(directory, artifact)
	if err != nil {
		return nil, err
	}
	payload, err := readBoundedRegularFile(path, maximumCampaignManifestBytes, "campaign artifact")
	if err != nil {
		return nil, err
	}
	if digestBytes(payload) != artifact.ArtifactSHA256 {
		return nil, errors.New("artifact digest mismatch")
	}
	return payload, nil
}

func campaignArtifactPath(directory string, artifact CampaignArtifact) (string, error) {
	if err := artifact.validate(); err != nil {
		return "", err
	}
	return resolveCampaignPath(directory, artifact.Path)
}

func resolveCampaignPath(directory, relative string) (string, error) {
	clean, err := validateCampaignRelativePath(relative)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", errors.New("resolve campaign artifact root")
	}
	path := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("campaign artifact escapes closure directory")
	}
	return path, nil
}

func validateCampaignRelativePath(path string) (string, error) {
	if path == "" || len(path) > 4096 || path != filepath.ToSlash(path) || filepath.IsAbs(path) ||
		strings.Contains(path, "\\") || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("campaign artifact path is not a canonical relative slash path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean != path || clean == ".." || strings.HasPrefix(clean, "../") ||
		filepath.Base(filepath.FromSlash(clean)) != filepath.FromSlash(clean) {
		return "", errors.New("campaign artifact path must be one canonical flat relative name")
	}
	return clean, nil
}

func readBoundedRegularFile(path string, maximum int64, label string) ([]byte, error) {
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a direct regular file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(linked, before) ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, fmt.Errorf("%s is not a bounded non-empty regular file", label)
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, fmt.Errorf("read complete %s", label)
	}
	after, err := file.Stat()
	linkedAfter, linkedErr := os.Lstat(path)
	if err != nil || linkedErr != nil || !linkedAfter.Mode().IsRegular() ||
		!os.SameFile(before, after) || !os.SameFile(before, linkedAfter) ||
		before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, fmt.Errorf("%s changed while it was read", label)
	}
	return payload, nil
}

func writeCampaignCreateOnly(path string, payload []byte) error {
	if len(payload) == 0 || len(payload) > maximumCampaignClosureBytes {
		return errors.New("campaign closure payload is empty or oversized")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create campaign closure directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create campaign closure: %w", err)
	}
	written, writeErr := file.Write(payload)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || written != len(payload) || syncErr != nil || closeErr != nil {
		return errors.Join(writeErr, syncErr, closeErr, errors.New("campaign closure was not written completely"))
	}
	return nil
}
