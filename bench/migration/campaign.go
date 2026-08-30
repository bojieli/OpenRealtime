package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

const EvidenceKindReport = "migration-report"

// LaunchRequest is the side-effect-free declaration an existing suite CLI
// validates immediately before opening an endpoint or starting a model task.
// Keys are every attempt that invocation will produce for this suite.
type LaunchRequest struct {
	Arm        Arm
	Suite      string
	Keys       []AttemptKey
	Cell       bench.Cell
	Provenance bench.Provenance
	ObservedAt time.Time
}

// ValidateLaunch proves a launch is one or more complete repetition slabs of
// the preregistered full matrix. A migration invocation may cover one suite at
// a time and may split preregistered repetitions across invocations, but it may
// never filter cases within a repetition. Results from all suite invocations
// are joined only by CompareRegistered, which still requires the complete
// global matrix.
func ValidateLaunch(
	store LocalStore, registrationReference EvidenceRef, request LaunchRequest,
) error {
	registration, err := ResolveRegistration(store, registrationReference)
	if err != nil {
		return fmt.Errorf("migration launch preflight: %w", err)
	}
	manifest, census, profiles, err := VerifyRegistration(store, registration, request.ObservedAt)
	if err != nil {
		return fmt.Errorf("migration launch preflight: %w", err)
	}
	if request.Arm != ArmBaseline && request.Arm != ArmCandidate {
		return fmt.Errorf("migration launch preflight: invalid arm %q", request.Arm)
	}
	profile, exists := profiles[request.Suite]
	if !exists {
		return fmt.Errorf("migration launch preflight: no registered importer for suite %q", request.Suite)
	}
	censusSuite, exists := censusSuiteNamed(census, request.Suite)
	if !exists {
		return fmt.Errorf("migration launch preflight: no census suite %q", request.Suite)
	}
	manifestSuite, exists := suiteNamed(manifest, request.Suite)
	if !exists {
		return fmt.Errorf("migration launch preflight: manifest omits suite %q", request.Suite)
	}
	expectedKeys := suiteKeys(manifestSuite)
	actualKeys := make(map[AttemptKey]bool, len(request.Keys))
	repetitions := make(map[string]bool)
	for _, key := range request.Keys {
		if key.Suite != request.Suite {
			return fmt.Errorf("migration launch preflight: attempt key belongs to suite %q", key.Suite)
		}
		if actualKeys[key] {
			return fmt.Errorf("migration launch preflight: duplicate attempt %s", keyScope(key))
		}
		if !expectedKeys[key] {
			return fmt.Errorf("migration launch preflight: attempt %s is absent from the preregistered matrix",
				keyScope(key))
		}
		actualKeys[key] = true
		repetitions[key.Repetition] = true
	}
	if len(actualKeys) == 0 {
		return fmt.Errorf("migration launch preflight: suite %q will run no attempts", request.Suite)
	}
	for repetition := range repetitions {
		for _, item := range censusSuite.Cases {
			key := AttemptKey{Suite: request.Suite, Condition: item.Condition,
				Case: item.ID, Repetition: repetition}
			if expectedKeys[key] && !actualKeys[key] {
				return fmt.Errorf("migration launch preflight: repetition %q filters out %s",
					repetition, keyScope(key))
			}
		}
	}
	if len(censusSuite.Cases) != manifestSuite.ExpectedCases {
		return fmt.Errorf("migration launch preflight: suite %q census and manifest case counts differ", request.Suite)
	}
	if err := request.Cell.Execution.Validate(); err != nil {
		return fmt.Errorf("migration launch preflight: execution requirement: %w", err)
	}
	executionKind, err := profile.executionKind(request.Arm)
	if err != nil {
		return fmt.Errorf("migration launch preflight: %w", err)
	}
	if !request.Cell.Execution.Required() || request.Cell.Execution.Kind != executionKind {
		return fmt.Errorf("migration launch preflight: suite %q requires %s execution, got %s",
			request.Suite, executionKind, request.Cell.Execution.Kind)
	}
	if err := request.Provenance.Reproducible(); err != nil {
		return fmt.Errorf("migration launch preflight: provenance is not reproducible: %w", err)
	}
	axes, err := launchAxes(request.Cell, request.Provenance, profile, request.Arm)
	if err != nil {
		return fmt.Errorf("migration launch preflight: %w", err)
	}
	expectedAxes := make(map[string]string, len(manifest.FixedAxes)+len(manifest.Treatment))
	for _, axis := range manifest.FixedAxes {
		expectedAxes[axis.Name] = axis.Value
	}
	for _, treatment := range manifest.Treatment {
		if request.Arm == ArmBaseline {
			expectedAxes[treatment.Axis] = treatment.Baseline
		} else {
			expectedAxes[treatment.Axis] = treatment.Candidate
		}
	}
	observedAxes := make(map[string]string, len(axes))
	for _, axis := range axes {
		observedAxes[axis.Name] = axis.Value
	}
	for name, expected := range expectedAxes {
		observed, present := observedAxes[name]
		// Legacy deployment/runtime digests are observed only after handshake.
		// Its exact binding/topology requirement and cell configuration still
		// match before launch; the imported row checks the live fields later.
		if executionKind == bench.ExecutionLegacy &&
			(name == "deployment_sha256" || name == "runtime_sha256" ||
				name == "legacy_runtime_sha256") {
			continue
		}
		if !present || observed != expected {
			return fmt.Errorf("migration launch preflight: axis %q is %q, preregistered %q",
				name, observed, expected)
		}
	}
	for name := range observedAxes {
		if _, declared := expectedAxes[name]; !declared {
			return fmt.Errorf("migration launch preflight: observed axis %q is absent from the manifest", name)
		}
	}
	return nil
}

func launchAxes(
	cell bench.Cell, provenance bench.Provenance, profile ImportProfile, arm Arm,
) ([]Axis, error) {
	executionKind, err := profile.executionKind(arm)
	if err != nil {
		return nil, err
	}
	axes := append([]Axis(nil), profile.Axes...)
	axes = append(axes,
		Axis{Name: "source_revision", Value: provenance.Revision},
		Axis{Name: "source_modified", Value: fmt.Sprint(provenance.Modified)},
		Axis{Name: "executable_sha256", Value: provenance.ExecutableSHA256},
		Axis{Name: "machine_sha256", Value: digestJSON(provenance.Machine)},
		Axis{Name: "cell_sha256", Value: digestJSON(cell)},
		Axis{Name: "execution_kind", Value: string(executionKind)},
	)
	switch executionKind {
	case bench.ExecutionGraphNative:
		if cell.Execution.Graph == nil {
			return nil, errors.New("graph-native launch has no exact graph requirement")
		}
		graph := cell.Execution.Graph
		evidence := bench.ExecutionEvidence{
			FormatVersion: bench.AttestationFormatVersion,
			Kind:          bench.ExecutionGraphNative, Graph: graph,
		}
		axes = append(axes,
			Axis{Name: "graph_sha256", Value: digestJSON(graph.Graph)},
			Axis{Name: "configuration_sha256", Value: digestJSON(graph.Configuration)},
			Axis{Name: "deployment_sha256", Value: digestJSON(graph.Nodes)},
			Axis{Name: "runtime_sha256", Value: stableRuntimeDigest(evidence)},
			Axis{Name: "legacy_runtime_sha256", Value: notApplicableIdentitySHA256},
		)
	case bench.ExecutionLegacy:
		if cell.Execution.Legacy == nil {
			return nil, errors.New("legacy launch has no exact compatibility requirement")
		}
		axes = append(axes,
			Axis{Name: "graph_sha256", Value: digestJSON(cell.Execution.Legacy)},
			Axis{Name: "configuration_sha256", Value: digestJSON(cell)},
		)
	default:
		return nil, fmt.Errorf("unsupported execution kind %q", executionKind)
	}
	sort.Slice(axes, func(left, right int) bool { return axes[left].Name < axes[right].Name })
	return axes, nil
}

// CompareRegistered imports the existing result formats, performs the strict
// history-aware comparison, and archives the canonical report create-only.
// Even a refused or regressed comparison is archived and returned.
func CompareRegistered(
	store LocalStore, registrationReference EvidenceRef,
	baselineInputs, candidateInputs []ResultImport,
	historyReferences []EvidenceRef, reportLocation string,
) (Report, EvidenceRef, error) {
	if strings.TrimSpace(reportLocation) == "" {
		return Report{}, EvidenceRef{}, errors.New("registered comparison needs a report location")
	}
	registration, err := ResolveRegistration(store, registrationReference)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	baselineInputs, orphanedBaselineIntents, err := completeArmImports(
		store, registrationReference, ArmBaseline, baselineInputs)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	candidateInputs, orphanedCandidateIntents, err := completeArmImports(
		store, registrationReference, ArmCandidate, candidateInputs)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	baselinePrepared, err := prepareResultImports(
		store, registrationReference, ArmBaseline, baselineInputs)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	candidatePrepared, err := prepareResultImports(
		store, registrationReference, ArmCandidate, candidateInputs)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	candidateStarted, err := earliestPreparedObservation(
		store, registration, candidatePrepared, orphanedCandidateIntents)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	manifest, census, profiles, err := VerifyRegistration(
		store, registration, candidateStarted)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	history, err := ResolveReportHistory(store, historyReferences)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	profileReferences, err := registeredProfileReferences(store, registration)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	importArm := func(arm Arm, inputs []preparedResultImport) ([]Attempt, error) {
		var attempts []Attempt
		for _, prepared := range inputs {
			input := prepared.ResultImport
			profile, exists := profiles[input.Suite]
			if !exists {
				return nil, fmt.Errorf("no registered profile for result suite %q", input.Suite)
			}
			rows, err := ImportBenchResult(store, census, profile,
				profileReferences[input.Suite], arm, input)
			if err != nil {
				return nil, err
			}
			if prepared.Intent == nil {
				for index := range rows {
					markImportFailure(&rows[index], fmt.Errorf(
						"%s result has no preregistered launch-outcome sidecar", arm))
				}
			}
			if prepared.Intent != nil {
				intended := make(map[AttemptKey]bool, len(prepared.Intent.Keys))
				for _, key := range prepared.Intent.Keys {
					intended[key] = true
				}
				for index := range rows {
					rows[index].Evidence = append(rows[index].Evidence,
						prepared.OutcomeReference, prepared.IntentReference)
					if rows[index].Key.Condition != "import-refusal" && !intended[rows[index].Key] {
						markImportFailure(&rows[index], fmt.Errorf(
							"attempt %s is absent from its preregistered launch intent",
							keyScope(rows[index].Key)))
					}
				}
			}
			attempts = append(attempts, rows...)
		}
		return attempts, nil
	}
	baseline, err := importArm(ArmBaseline, baselinePrepared)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	candidate, err := importArm(ArmCandidate, candidatePrepared)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	appendOrphans := func(target []Attempt, orphans []orphanedLaunchIntent) []Attempt {
		for _, orphan := range orphans {
			for index, key := range orphan.Intent.Keys {
				target = append(target, Attempt{
					ID: fmt.Sprintf("%s/orphan/%d", orphan.Intent.IntentID, index), Key: key,
					Axes: append([]Axis(nil), orphan.Intent.Axes...), Completed: false,
					Error: "migration launch intent has no retained result outcome",
					Outcomes: Outcomes{Interaction: OutcomeNotApplicable,
						Deadline: OutcomeNotApplicable, Safety: OutcomeNotApplicable},
					Evidence: []EvidenceRef{orphan.Reference},
				})
			}
		}
		return target
	}
	baseline = appendOrphans(baseline, orphanedBaselineIntents)
	candidate = appendOrphans(candidate, orphanedCandidateIntents)
	report := CompareWithHistory(manifest, baseline, candidate, history)
	// Always use the history-aware path, including the predecessor-free first
	// run, so integrations cannot accidentally switch APIs when lineage appears.
	payload, err := report.MarshalWithHistory(history)
	if err != nil {
		return Report{}, EvidenceRef{}, fmt.Errorf("seal registered comparison: %w", err)
	}
	reference, err := store.Put(EvidenceKindReport, reportLocation, payload)
	if err != nil {
		return Report{}, EvidenceRef{}, err
	}
	if reference.SHA256 != report.ArtifactSHA256() {
		return Report{}, EvidenceRef{}, errors.New("archived report digest differs from canonical report")
	}
	return report, reference, nil
}

type preparedResultImport struct {
	ResultImport
	Intent           *LaunchIntent
	IntentReference  EvidenceRef
	OutcomeReference EvidenceRef
}

type orphanedLaunchIntent struct {
	Intent    LaunchIntent
	Reference EvidenceRef
}

func completeArmImports(
	store LocalStore, registration EvidenceRef, arm Arm, inputs []ResultImport,
) ([]ResultImport, []orphanedLaunchIntent, error) {
	if arm != ArmBaseline && arm != ArmCandidate {
		return nil, nil, fmt.Errorf("discover migration imports: invalid arm %q", arm)
	}
	intentReferences, err := store.List(EvidenceKindLaunchIntent)
	if err != nil {
		return nil, nil, err
	}
	intents := map[EvidenceRef]LaunchIntent{}
	for _, reference := range intentReferences {
		payload, err := store.Resolve(reference)
		if err != nil {
			return nil, nil, err
		}
		intent, err := DecodeLaunchIntent(bytes.NewReader(payload))
		if err != nil {
			return nil, nil, err
		}
		if intent.Registration == registration && intent.Arm == arm {
			intents[reference] = intent
		}
	}
	existingOutcomes := map[EvidenceRef]bool{}
	for _, input := range inputs {
		if input.Outcome.Kind != "" {
			existingOutcomes[input.Outcome] = true
		}
	}
	usedIntents := map[EvidenceRef]bool{}
	outcomeReferences, err := store.List(EvidenceKindLaunchOutcome)
	if err != nil {
		return nil, nil, err
	}
	for _, reference := range outcomeReferences {
		outcome, intent, err := ResolveLaunchOutcome(store, reference)
		if err != nil {
			return nil, nil, err
		}
		if intent.Registration != registration || intent.Arm != arm {
			continue
		}
		usedIntents[outcome.Intent] = true
		if !existingOutcomes[reference] {
			inputs = append(inputs, ResultImport{Suite: intent.Suite, Outcome: reference})
			existingOutcomes[reference] = true
		}
	}
	var orphaned []orphanedLaunchIntent
	for reference, intent := range intents {
		if !usedIntents[reference] {
			orphaned = append(orphaned, orphanedLaunchIntent{Intent: intent, Reference: reference})
		}
	}
	sort.Slice(orphaned, func(left, right int) bool {
		return orphaned[left].Intent.IntentID < orphaned[right].Intent.IntentID
	})
	return inputs, orphaned, nil
}

func earliestPreparedObservation(
	store LocalStore, registration StudyRegistration, inputs []preparedResultImport,
	orphaned []orphanedLaunchIntent,
) (time.Time, error) {
	if len(inputs) == 0 && len(orphaned) == 0 {
		return time.Time{}, errors.New("registered comparison has no candidate result artifacts")
	}
	earliest := time.Time{}
	for _, input := range inputs {
		if input.Intent != nil {
			when, _ := time.Parse(time.RFC3339Nano, input.Intent.ObservedAt)
			if earliest.IsZero() || when.Before(earliest) {
				earliest = when
			}
			continue
		}
		when, err := earliestResultStart(store, registration, []ResultImport{input.ResultImport})
		if err != nil {
			return time.Time{}, err
		}
		if earliest.IsZero() || when.Before(earliest) {
			earliest = when
		}
	}
	for _, orphan := range orphaned {
		when, _ := time.Parse(time.RFC3339Nano, orphan.Intent.ObservedAt)
		if earliest.IsZero() || when.Before(earliest) {
			earliest = when
		}
	}
	return earliest, nil
}

func prepareResultImports(
	store LocalStore, registration EvidenceRef, arm Arm, inputs []ResultImport,
) ([]preparedResultImport, error) {
	registered, err := ResolveRegistration(store, registration)
	if err != nil {
		return nil, err
	}
	_, _, profiles, err := loadStudyInputs(
		store, registered.Manifest, registered.Census, registered.ImportProfiles)
	if err != nil {
		return nil, err
	}
	result := make([]preparedResultImport, len(inputs))
	for index, input := range inputs {
		result[index].ResultImport = input
		if input.Outcome.Kind == "" {
			continue
		}
		outcome, intent, err := ResolveLaunchOutcome(store, input.Outcome)
		if err != nil {
			return nil, err
		}
		if intent.Registration != registration || intent.Arm != arm || intent.Suite != input.Suite {
			return nil, fmt.Errorf("launch outcome for %q does not match the registration, arm, or suite", input.Suite)
		}
		profile, exists := profiles[input.Suite]
		if !exists {
			return nil, fmt.Errorf("launch outcome has no registered profile for %q", input.Suite)
		}
		resultPayload, err := store.Resolve(outcome.Result)
		if err != nil {
			return nil, err
		}
		observed, _, err := decodeCompatibleResult(resultPayload, profile.Format)
		if err != nil {
			return nil, err
		}
		if !compatibleSuiteName(input.Suite, observed.Suite) {
			return nil, fmt.Errorf("launch outcome result suite %q does not match %q",
				observed.Suite, input.Suite)
		}
		started, err := time.Parse(time.RFC3339Nano, observed.Provenance.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("launch outcome result has invalid start time: %w", err)
		}
		intentAt, _ := time.Parse(time.RFC3339Nano, intent.ObservedAt)
		// Existing suite provenance is second-resolution. Permit only that
		// rounding interval; an artifact observably older than its intent is a
		// post-hoc repetition assignment and is refused.
		if started.Add(time.Second).Before(intentAt) {
			return nil, errors.New("launch outcome result predates its preregistered intent")
		}
		axes, err := launchAxes(observed.Cell, observed.Provenance, profile, arm)
		if err != nil {
			return nil, fmt.Errorf("launch outcome result axes: %w", err)
		}
		if canonicalJSON(axes) != canonicalJSON(intent.Axes) {
			return nil, errors.New("launch outcome result differs from its prelaunch axes")
		}
		if input.Reference.Kind != "" && input.Reference != outcome.Result {
			return nil, fmt.Errorf("launch outcome for %q names a different result artifact", input.Suite)
		}
		result[index].Reference = outcome.Result
		result[index].OutcomeReference = input.Outcome
		result[index].IntentReference = outcome.Intent
		result[index].Intent = &intent
		if input.Suite != SuiteScenario && input.Suite != SuiteTauControl && input.Suite != SuiteTauRegular {
			repetitions := map[string]bool{}
			for _, key := range intent.Keys {
				repetitions[key.Repetition] = true
			}
			if len(repetitions) != 1 {
				return nil, fmt.Errorf("one-shot suite %q launch outcome contains %d repetitions",
					input.Suite, len(repetitions))
			}
			for repetition := range repetitions {
				if input.Repetition != "" && input.Repetition != repetition {
					return nil, fmt.Errorf("result repetition %q differs from launch intent %q",
						input.Repetition, repetition)
				}
				result[index].Repetition = repetition
			}
		}
	}
	return result, nil
}

// ResolveReportHistory reconstructs canonical predecessor reports from their
// create-only store references. It accepts references in any order and only
// admits a report after all of its own predecessors can be verified.
func ResolveReportHistory(store LocalStore, references []EvidenceRef) ([]Report, error) {
	payloads := make([][]byte, len(references))
	for index, reference := range references {
		if reference.Kind != EvidenceKindReport {
			return nil, fmt.Errorf("history reference kind must be %q", EvidenceKindReport)
		}
		payload, err := store.Resolve(reference)
		if err != nil {
			return nil, err
		}
		payloads[index] = payload
	}
	resolved := make([]Report, 0, len(payloads))
	remaining := append([][]byte(nil), payloads...)
	for len(remaining) > 0 {
		progress := false
		next := make([][]byte, 0, len(remaining))
		for _, payload := range remaining {
			var envelope struct {
				SuppliedHistory []ReportReference `json:"supplied_history"`
			}
			if err := json.Unmarshal(payload, &envelope); err != nil {
				return nil, fmt.Errorf("decode history envelope: %w", err)
			}
			needed := make([]Report, 0, len(envelope.SuppliedHistory))
			for _, reference := range envelope.SuppliedHistory {
				for _, existing := range resolved {
					if existing.ReportID == reference.ReportID {
						needed = append(needed, existing)
						break
					}
				}
			}
			if len(needed) != len(envelope.SuppliedHistory) {
				next = append(next, payload)
				continue
			}
			report, err := DecodeWithHistory(bytes.NewReader(payload), needed)
			if err != nil {
				next = append(next, payload)
				continue
			}
			duplicate := false
			for _, existing := range resolved {
				if existing.ReportID == report.ReportID {
					duplicate = true
					break
				}
			}
			if duplicate {
				return nil, fmt.Errorf("history repeats report %s", report.ReportID)
			}
			resolved = append(resolved, report)
			progress = true
		}
		if !progress {
			return nil, errors.New("history artifacts cannot be reconstructed from the supplied complete lineage")
		}
		remaining = next
	}
	return resolved, nil
}

// ResolveRegistration proves that the supplied registration object is the
// exact canonical byte artifact retained by the create-only evidence store.
func ResolveRegistration(store LocalStore, reference EvidenceRef) (StudyRegistration, error) {
	if reference.Kind != EvidenceKindRegistration {
		return StudyRegistration{}, fmt.Errorf("registration reference kind must be %q", EvidenceKindRegistration)
	}
	payload, err := store.Resolve(reference)
	if err != nil {
		return StudyRegistration{}, err
	}
	registration, err := DecodeRegistration(bytes.NewReader(payload))
	if err != nil {
		return StudyRegistration{}, err
	}
	return registration, nil
}

// RegisteredSuiteKeys returns the complete preregistered attempt population
// for one suite. Launch compatibility adapters select whole repetition slabs
// from this set and pass their actual selection back to ValidateLaunch.
func RegisteredSuiteKeys(
	store LocalStore, registrationReference EvidenceRef, suite string, observedAt time.Time,
) ([]AttemptKey, error) {
	registration, err := ResolveRegistration(store, registrationReference)
	if err != nil {
		return nil, err
	}
	manifest, _, _, err := VerifyRegistration(store, registration, observedAt)
	if err != nil {
		return nil, err
	}
	spec, exists := suiteNamed(manifest, suite)
	if !exists {
		return nil, fmt.Errorf("registered matrix has no suite %q", suite)
	}
	keys := make([]AttemptKey, 0, spec.ExpectedAttempts)
	for key := range suiteKeys(spec) {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return keyScope(keys[left]) < keyScope(keys[right]) })
	return keys, nil
}

func earliestResultStart(
	store LocalStore, registration StudyRegistration, inputs []ResultImport,
) (time.Time, error) {
	if len(inputs) == 0 {
		return time.Time{}, errors.New("registered comparison has no candidate result artifacts")
	}
	_, _, profiles, err := loadStudyInputs(
		store, registration.Manifest, registration.Census, registration.ImportProfiles)
	if err != nil {
		return time.Time{}, err
	}
	earliest := time.Time{}
	for _, input := range inputs {
		profile, exists := profiles[input.Suite]
		if !exists {
			return time.Time{}, fmt.Errorf("no registered profile for candidate suite %q", input.Suite)
		}
		if input.Reference.Kind != EvidenceKindResult {
			return time.Time{}, fmt.Errorf("candidate result %q has evidence kind %q",
				input.Reference.Location, input.Reference.Kind)
		}
		payload, err := store.Resolve(input.Reference)
		if err != nil {
			return time.Time{}, err
		}
		result, _, err := decodeCompatibleResult(payload, profile.Format)
		if err != nil {
			return time.Time{}, fmt.Errorf("read candidate provenance: %w", err)
		}
		if !compatibleSuiteName(input.Suite, result.Suite) {
			return time.Time{}, fmt.Errorf("candidate result suite %q does not match input suite %q",
				result.Suite, input.Suite)
		}
		started, err := time.Parse(time.RFC3339Nano, result.Provenance.StartedAt)
		if err != nil {
			return time.Time{}, fmt.Errorf("candidate result %q has invalid start time: %w",
				input.Reference.Location, err)
		}
		if earliest.IsZero() || started.Before(earliest) {
			earliest = started
		}
	}
	return earliest, nil
}

func registeredProfileReferences(
	store LocalStore, registration StudyRegistration,
) (map[string]EvidenceRef, error) {
	result := make(map[string]EvidenceRef, len(registration.ImportProfiles))
	for _, reference := range registration.ImportProfiles {
		payload, err := store.Resolve(reference)
		if err != nil {
			return nil, err
		}
		profile, err := DecodeImportProfile(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		result[profile.Suite] = reference
	}
	return result, nil
}
