package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const RegistrationVersion = 1

const EvidenceKindRegistration = "migration-registration"

// StudyRegistration proves that the census, statistical manifest, importer
// rules, and baseline-only acceptance evidence were all immutable before a
// candidate launch. It is itself create-only in LocalStore.
type StudyRegistration struct {
	Version         int           `json:"version"`
	RegistrationID  string        `json:"registration_id"`
	RegisteredAt    string        `json:"registered_at"`
	ManifestID      string        `json:"manifest_id"`
	Manifest        EvidenceRef   `json:"manifest"`
	Census          EvidenceRef   `json:"census"`
	ImportProfiles  []EvidenceRef `json:"import_profiles"`
	AcceptanceBases []EvidenceRef `json:"acceptance_bases"`
}

func MarshalManifest(manifest Manifest) ([]byte, error) {
	canonical := canonicalManifest(manifest)
	if err := canonical.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonical, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeManifest(reader io.Reader) (Manifest, error) {
	if reader == nil {
		return Manifest{}, errors.New("decode migration manifest: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "migration manifest")
	if err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode migration manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Manifest{}, errors.New("decode migration manifest: trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("decode migration manifest trailing content: %w", err)
	}
	manifest = canonicalManifest(manifest)
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	canonical, err := MarshalManifest(manifest)
	if err != nil || !bytes.Equal(payload, canonical) {
		return Manifest{}, errors.New("decode migration manifest: artifact bytes are not canonical")
	}
	return manifest, nil
}

func (registration StudyRegistration) ID() string {
	registration = canonicalRegistration(registration)
	registration.RegistrationID = ""
	return digestJSON(registration)
}

func canonicalRegistration(input StudyRegistration) StudyRegistration {
	result := input
	result.ImportProfiles = append([]EvidenceRef(nil), input.ImportProfiles...)
	result.AcceptanceBases = append([]EvidenceRef(nil), input.AcceptanceBases...)
	sortEvidence := func(values []EvidenceRef) {
		sort.Slice(values, func(left, right int) bool {
			if values[left].Kind != values[right].Kind {
				return values[left].Kind < values[right].Kind
			}
			if values[left].Location != values[right].Location {
				return values[left].Location < values[right].Location
			}
			return values[left].SHA256 < values[right].SHA256
		})
	}
	sortEvidence(result.ImportProfiles)
	sortEvidence(result.AcceptanceBases)
	return result
}

// RegisterStudy verifies every referenced byte and writes a create-only
// registration. The caller supplies already archived manifest, census, and
// profile references; registration never reaches outside the evidence store.
func RegisterStudy(
	store LocalStore, location string, manifestReference, censusReference EvidenceRef,
	profileReferences []EvidenceRef, registeredAt time.Time,
) (StudyRegistration, EvidenceRef, error) {
	if registeredAt.IsZero() {
		return StudyRegistration{}, EvidenceRef{}, errors.New("study registration needs an explicit time")
	}
	manifest, census, profiles, err := loadStudyInputs(
		store, manifestReference, censusReference, profileReferences)
	if err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	if err := ValidateManifestCensus(manifest, census); err != nil {
		return StudyRegistration{}, EvidenceRef{}, fmt.Errorf("register migration study: %w", err)
	}
	if err := validateProfileCoverage(census, profiles); err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	if err := validateProfilesAgainstManifest(manifest, profiles); err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	bases := acceptanceBases(manifest)
	if err := validateAcceptanceBases(store, manifest, bases, registeredAt); err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	for _, reference := range census.Sources {
		if _, err := store.Resolve(reference); err != nil {
			return StudyRegistration{}, EvidenceRef{}, fmt.Errorf(
				"resolve census source %q: %w", reference.Location, err)
		}
	}
	registration := canonicalRegistration(StudyRegistration{
		Version: RegistrationVersion, RegisteredAt: registeredAt.UTC().Format(time.RFC3339Nano),
		ManifestID: manifest.ID(), Manifest: manifestReference, Census: censusReference,
		ImportProfiles: profileReferences, AcceptanceBases: bases,
	})
	registration.RegistrationID = registration.ID()
	payload, err := MarshalRegistration(registration)
	if err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	reference, err := store.Put(EvidenceKindRegistration, location, payload)
	if err != nil {
		return StudyRegistration{}, EvidenceRef{}, err
	}
	return registration, reference, nil
}

func (registration StudyRegistration) Validate() error {
	if registration.Version != RegistrationVersion {
		return fmt.Errorf("study registration version must be %d, got %d",
			RegistrationVersion, registration.Version)
	}
	if !validSHA256(registration.RegistrationID) || registration.ID() != registration.RegistrationID {
		return errors.New("study registration digest does not match its content")
	}
	if !validSHA256(registration.ManifestID) {
		return errors.New("study registration has no migration manifest ID")
	}
	when, err := time.Parse(time.RFC3339Nano, registration.RegisteredAt)
	if err != nil || when.Location() != time.UTC {
		return errors.New("study registration time must be canonical UTC RFC3339")
	}
	if registration.Manifest.Kind != EvidenceKindManifest ||
		registration.Census.Kind != EvidenceKindCensus {
		return errors.New("study registration has the wrong manifest or census artifact kind")
	}
	if len(registration.ImportProfiles) == 0 || len(registration.AcceptanceBases) == 0 {
		return errors.New("study registration needs import profiles and acceptance bases")
	}
	if len(registration.ImportProfiles) > maxManifestSuites ||
		len(registration.AcceptanceBases) > maxManifestSuites {
		return errors.New("study registration evidence count exceeds the schema limit")
	}
	seen := map[string]bool{}
	for _, reference := range append(append([]EvidenceRef(nil), registration.ImportProfiles...),
		registration.AcceptanceBases...) {
		if strings.TrimSpace(reference.Location) == "" || !validSHA256(reference.SHA256) {
			return errors.New("study registration contains an invalid evidence reference")
		}
		identity := reference.Kind + "\x00" + reference.Location
		if seen[identity] {
			return fmt.Errorf("study registration repeats evidence %q", reference.Location)
		}
		seen[identity] = true
	}
	for _, reference := range registration.ImportProfiles {
		if reference.Kind != EvidenceKindImportProfile {
			return errors.New("study registration contains a non-profile import reference")
		}
	}
	for _, reference := range registration.AcceptanceBases {
		if reference.Kind != AcceptanceBasisKind {
			return errors.New("study registration contains a non-baseline acceptance basis")
		}
	}
	return nil
}

func MarshalRegistration(registration StudyRegistration) ([]byte, error) {
	if err := registration.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalRegistration(registration), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeRegistration(reader io.Reader) (StudyRegistration, error) {
	if reader == nil {
		return StudyRegistration{}, errors.New("decode study registration: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "study registration")
	if err != nil {
		return StudyRegistration{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var registration StudyRegistration
	if err := decoder.Decode(&registration); err != nil {
		return StudyRegistration{}, fmt.Errorf("decode study registration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return StudyRegistration{}, errors.New("decode study registration: trailing JSON value")
		}
		return StudyRegistration{}, fmt.Errorf("decode study registration trailing content: %w", err)
	}
	if err := registration.Validate(); err != nil {
		return StudyRegistration{}, err
	}
	registration = canonicalRegistration(registration)
	canonical, err := MarshalRegistration(registration)
	if err != nil || !bytes.Equal(payload, canonical) {
		return StudyRegistration{}, errors.New("decode study registration: artifact bytes are not canonical")
	}
	return registration, nil
}

func readBoundedJSONArtifact(reader io.Reader, label string) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: MaxReportArtifactBytes + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if int64(len(payload)) > MaxReportArtifactBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", label, MaxReportArtifactBytes)
	}
	if err := strictjson.Validate(payload); err != nil {
		return nil, fmt.Errorf("decode %s: %w", label, err)
	}
	return payload, nil
}

// VerifyRegistration reloads every preregistered input and proves the
// registration timestamp predates the supplied observation time.
func VerifyRegistration(
	store LocalStore, registration StudyRegistration, observedAt time.Time,
) (Manifest, Census, map[string]ImportProfile, error) {
	if err := registration.Validate(); err != nil {
		return Manifest{}, Census{}, nil, err
	}
	registeredAt, _ := time.Parse(time.RFC3339Nano, registration.RegisteredAt)
	if observedAt.IsZero() || !observedAt.After(registeredAt) {
		return Manifest{}, Census{}, nil, errors.New(
			"candidate observation time precedes the preregistered study")
	}
	manifest, census, profiles, err := loadStudyInputs(
		store, registration.Manifest, registration.Census, registration.ImportProfiles)
	if err != nil {
		return Manifest{}, Census{}, nil, err
	}
	if manifest.ID() != registration.ManifestID ||
		canonicalJSON(acceptanceBases(manifest)) != canonicalJSON(registration.AcceptanceBases) {
		return Manifest{}, Census{}, nil, errors.New("study registration no longer matches its manifest")
	}
	if err := ValidateManifestCensus(manifest, census); err != nil {
		return Manifest{}, Census{}, nil, err
	}
	if err := validateProfileCoverage(census, profiles); err != nil {
		return Manifest{}, Census{}, nil, err
	}
	if err := validateProfilesAgainstManifest(manifest, profiles); err != nil {
		return Manifest{}, Census{}, nil, err
	}
	if err := validateAcceptanceBases(store, manifest, registration.AcceptanceBases, registeredAt); err != nil {
		return Manifest{}, Census{}, nil, err
	}
	for _, reference := range append(append([]EvidenceRef(nil), census.Sources...),
		registration.AcceptanceBases...) {
		if _, err := store.Resolve(reference); err != nil {
			return Manifest{}, Census{}, nil, err
		}
	}
	return manifest, census, profiles, nil
}

func validateAcceptanceBases(
	store LocalStore, manifest Manifest, references []EvidenceRef, registeredAt time.Time,
) error {
	if len(references) != len(manifest.Suites) {
		return fmt.Errorf("migration study has %d acceptance bases for %d suites; each suite needs its own",
			len(references), len(manifest.Suites))
	}
	byReference := make(map[string]SuiteSpec, len(manifest.Suites))
	for _, suite := range manifest.Suites {
		reference := suite.Policy.AcceptanceBasis
		identity := reference.Kind + "\x00" + reference.Location + "\x00" + reference.SHA256
		if _, duplicate := byReference[identity]; duplicate {
			return fmt.Errorf("suites share acceptance basis %q", reference.Location)
		}
		byReference[identity] = suite
	}
	seenSuites := map[string]bool{}
	for _, reference := range references {
		identity := reference.Kind + "\x00" + reference.Location + "\x00" + reference.SHA256
		suite, exists := byReference[identity]
		if !exists {
			return fmt.Errorf("registration contains acceptance basis %q absent from the manifest", reference.Location)
		}
		payload, err := store.Resolve(reference)
		if err != nil {
			return fmt.Errorf("resolve preregistered acceptance basis %q: %w", reference.Location, err)
		}
		basis, err := DecodeAcceptanceBasis(bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("decode preregistered acceptance basis %q: %w", reference.Location, err)
		}
		createdAt, _ := time.Parse(time.RFC3339Nano, basis.CreatedAt)
		if !createdAt.Before(registeredAt) {
			return fmt.Errorf("suite %q acceptance basis was not frozen before registration", suite.Name)
		}
		if basis.Suite != suite.Name || basis.MinimumRepetitions != suite.MinimumRepetitions ||
			canonicalJSON(basis.Decision) != canonicalJSON(acceptanceDecision(suite.Policy)) {
			return fmt.Errorf("suite %q manifest policy differs from its baseline-only acceptance basis", suite.Name)
		}
		for _, evidence := range basis.BaselineEvidence {
			if _, err := store.Resolve(evidence); err != nil {
				return fmt.Errorf("resolve acceptance-basis evidence %q: %w", evidence.Location, err)
			}
			if evidence.Kind == EvidenceKindLaunchOutcome {
				_, intent, err := ResolveLaunchOutcome(store, evidence)
				if err != nil || intent.Arm != ArmBaseline {
					return fmt.Errorf("suite %q acceptance basis contains non-baseline launch evidence", suite.Name)
				}
			}
		}
		seenSuites[suite.Name] = true
	}
	if len(seenSuites) != len(manifest.Suites) {
		return errors.New("acceptance bases do not cover every manifest suite")
	}
	return nil
}

func loadStudyInputs(
	store LocalStore, manifestReference, censusReference EvidenceRef,
	profileReferences []EvidenceRef,
) (Manifest, Census, map[string]ImportProfile, error) {
	if manifestReference.Kind != EvidenceKindManifest || censusReference.Kind != EvidenceKindCensus {
		return Manifest{}, Census{}, nil, errors.New("study inputs use the wrong manifest or census artifact kind")
	}
	manifestPayload, err := store.Resolve(manifestReference)
	if err != nil {
		return Manifest{}, Census{}, nil, err
	}
	manifest, err := DecodeManifest(bytes.NewReader(manifestPayload))
	if err != nil {
		return Manifest{}, Census{}, nil, err
	}
	canonicalManifestPayload, err := MarshalManifest(manifest)
	if err != nil || !bytes.Equal(manifestPayload, canonicalManifestPayload) {
		return Manifest{}, Census{}, nil, errors.New("migration manifest artifact is not canonical")
	}
	censusPayload, err := store.Resolve(censusReference)
	if err != nil {
		return Manifest{}, Census{}, nil, err
	}
	census, err := DecodeCensus(bytes.NewReader(censusPayload))
	if err != nil {
		return Manifest{}, Census{}, nil, err
	}
	canonicalCensusPayload, err := MarshalCensus(census)
	if err != nil || !bytes.Equal(censusPayload, canonicalCensusPayload) {
		return Manifest{}, Census{}, nil, errors.New("suite census artifact is not canonical")
	}
	profiles := make(map[string]ImportProfile, len(profileReferences))
	for _, reference := range profileReferences {
		if reference.Kind != EvidenceKindImportProfile {
			return Manifest{}, Census{}, nil, errors.New("study input contains a non-profile reference")
		}
		payload, err := store.Resolve(reference)
		if err != nil {
			return Manifest{}, Census{}, nil, err
		}
		profile, err := DecodeImportProfile(bytes.NewReader(payload))
		if err != nil {
			return Manifest{}, Census{}, nil, err
		}
		canonicalProfilePayload, err := MarshalImportProfile(profile)
		if err != nil || !bytes.Equal(payload, canonicalProfilePayload) {
			return Manifest{}, Census{}, nil, errors.New("migration import profile artifact is not canonical")
		}
		if _, duplicate := profiles[profile.Suite]; duplicate {
			return Manifest{}, Census{}, nil, fmt.Errorf("study repeats import profile for %q", profile.Suite)
		}
		for _, evidence := range profile.Evidence {
			if _, err := store.Resolve(evidence); err != nil {
				return Manifest{}, Census{}, nil, fmt.Errorf(
					"resolve profile evidence %q: %w", evidence.Location, err)
			}
		}
		profiles[profile.Suite] = profile
	}
	return manifest, census, profiles, nil
}

func acceptanceBases(manifest Manifest) []EvidenceRef {
	seen := map[string]bool{}
	var result []EvidenceRef
	for _, suite := range canonicalManifest(manifest).Suites {
		reference := suite.Policy.AcceptanceBasis
		identity := reference.Kind + "\x00" + reference.Location + "\x00" + reference.SHA256
		if !seen[identity] {
			seen[identity] = true
			result = append(result, reference)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Location != result[right].Location {
			return result[left].Location < result[right].Location
		}
		return result[left].SHA256 < result[right].SHA256
	})
	return result
}

func validateProfileCoverage(census Census, profiles map[string]ImportProfile) error {
	if len(profiles) != len(census.Suites) {
		return fmt.Errorf("study has %d import profiles for %d census suites", len(profiles), len(census.Suites))
	}
	for _, suite := range census.Suites {
		profile, exists := profiles[suite.Name]
		if !exists {
			return fmt.Errorf("study has no preregistered import profile for %q", suite.Name)
		}
		if err := profile.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateProfilesAgainstManifest(manifest Manifest, profiles map[string]ImportProfile) error {
	declaredAxes := make(map[string]bool, len(manifest.FixedAxes)+len(manifest.Treatment))
	for _, axis := range manifest.FixedAxes {
		declaredAxes[axis.Name] = true
	}
	for _, treatment := range manifest.Treatment {
		declaredAxes[treatment.Axis] = true
	}
	for _, suite := range manifest.Suites {
		profile, exists := profiles[suite.Name]
		if !exists {
			return fmt.Errorf("manifest suite %q has no import profile", suite.Name)
		}
		for name, state := range map[string]struct {
			required bool
			rule     StateRule
		}{
			"interaction": {suite.Policy.Interaction.Required, profile.Interaction},
			"deadline":    {suite.Policy.Deadline.Required, profile.Deadline},
			"safety":      {suite.Policy.Safety.Required, profile.Safety},
		} {
			if state.required && state.rule.Kind == RuleNotApplicable {
				return fmt.Errorf("suite %q requires %s outcomes but its importer declares them not applicable",
					suite.Name, name)
			}
		}
		policyLatencies := make(map[string]string, len(suite.Policy.Latencies))
		for _, latency := range suite.Policy.Latencies {
			policyLatencies[latency.Name] = latency.Unit
		}
		if len(profile.Latencies) != len(policyLatencies) {
			return fmt.Errorf("suite %q importer maps %d latencies, manifest declares %d",
				suite.Name, len(profile.Latencies), len(policyLatencies))
		}
		for _, mapping := range profile.Latencies {
			unit, exists := policyLatencies[mapping.Name]
			if !exists || unit != mapping.Unit {
				return fmt.Errorf("suite %q latency mapping %q/%q is absent from the manifest",
					suite.Name, mapping.Name, mapping.Unit)
			}
		}
		availableEvidence := map[string]bool{
			EvidenceKindResult: true, EvidenceKindImportProfile: true, EvidenceKindExecution: true,
		}
		for _, evidence := range profile.Evidence {
			availableEvidence[evidence.Kind] = true
		}
		for _, kind := range suite.Policy.RequiredEvidenceKinds {
			if !availableEvidence[kind] {
				return fmt.Errorf("suite %q requires evidence kind %q its importer cannot supply",
					suite.Name, kind)
			}
		}
		observedAxes := make(map[string]bool, len(profile.Axes)+len(standardProvenanceAxes))
		for _, axis := range profile.Axes {
			observedAxes[axis.Name] = true
		}
		for axis := range standardProvenanceAxes {
			observedAxes[axis] = true
		}
		for axis := range observedAxes {
			if !declaredAxes[axis] {
				return fmt.Errorf("suite %q importer axis %q is absent from the manifest", suite.Name, axis)
			}
		}
		for axis := range declaredAxes {
			if !observedAxes[axis] {
				return fmt.Errorf("manifest axis %q cannot be produced by suite %q importer", axis, suite.Name)
			}
		}
		for _, arm := range []Arm{ArmBaseline, ArmCandidate} {
			expectedKind, err := profile.executionKind(arm)
			if err != nil {
				return err
			}
			if value, present := manifestAxisValue(manifest, "execution_kind", arm); !present || value != string(expectedKind) {
				return fmt.Errorf("suite %q %s execution kind is not preregistered exactly", suite.Name, arm)
			}
			if expectedKind == bench.ExecutionGraphNative {
				if value, present := manifestAxisValue(manifest, "legacy_runtime_sha256", arm); !present || value != notApplicableIdentitySHA256 {
					return fmt.Errorf("suite %q %s graph-native arm needs the canonical legacy-runtime sentinel",
						suite.Name, arm)
				}
			}
		}
		for _, axis := range profile.Axes {
			for _, arm := range []Arm{ArmBaseline, ArmCandidate} {
				if value, present := manifestAxisValue(manifest, axis.Name, arm); !present || value != axis.Value {
					return fmt.Errorf("suite %q importer axis %q differs from the %s manifest arm",
						suite.Name, axis.Name, arm)
				}
			}
		}
	}
	return nil
}

func manifestAxisValue(manifest Manifest, name string, arm Arm) (string, bool) {
	for _, axis := range manifest.FixedAxes {
		if axis.Name == name {
			return axis.Value, true
		}
	}
	for _, treatment := range manifest.Treatment {
		if treatment.Axis == name {
			if arm == ArmBaseline {
				return treatment.Baseline, true
			}
			if arm == ArmCandidate {
				return treatment.Candidate, true
			}
		}
	}
	return "", false
}
