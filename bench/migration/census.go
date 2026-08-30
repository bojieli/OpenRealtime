package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench/fdb"
	"github.com/bojieli/OpenRealtime/bench/fdbench"
	"github.com/bojieli/OpenRealtime/bench/fdbv3"
	"github.com/bojieli/OpenRealtime/bench/meeting"
	"github.com/bojieli/OpenRealtime/bench/realtimecu"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const CensusVersion = 1

const (
	SuiteScenario   = "scenario"
	SuiteMeeting    = meeting.SuiteName
	SuiteRealtimeCU = realtimecu.SuiteName
	SuiteFDB15      = "fdb-v1.5"
	SuiteFDBV3      = "fdb-v3"
	SuiteFDBench    = "fd-bench"
	SuiteTauControl = "tau-voice/control"
	SuiteTauRegular = "tau-voice/regular"
)

// Census is the immutable task universe a migration manifest must enumerate.
// It deliberately contains exact case IDs rather than only published totals:
// a count of 6,147 cannot reveal that one FD-Bench conversation was replaced
// by a duplicate from another condition.
type Census struct {
	Version  int           `json:"version"`
	CensusID string        `json:"census_id"`
	Sources  []EvidenceRef `json:"sources"`
	Suites   []CensusSuite `json:"suites"`
}

type CensusSuite struct {
	Name               string       `json:"name"`
	MinimumRepetitions int          `json:"minimum_repetitions"`
	Cases              []CensusCase `json:"cases"`
}

type CensusCase struct {
	Condition string `json:"condition"`
	ID        string `json:"id"`
}

// TauTaskInventory and TauTask retain the migration package's public names
// while sharing the canonical contract with the upstream inventory exporter.
type TauTaskInventory = tauvoice.TaskInventory
type TauTask = tauvoice.TaskIdentity

// RepositoryCensus returns the exact repository-owned portion of the matrix.
// External suites are intentionally absent until their prepared datasets have
// been enumerated by BuildExternalCensus.
func RepositoryCensus() Census {
	result := Census{Version: CensusVersion, Sources: []EvidenceRef{}, Suites: []CensusSuite{
		{Name: SuiteScenario, MinimumRepetitions: 15},
		{Name: SuiteMeeting, MinimumRepetitions: 1},
		{Name: SuiteRealtimeCU, MinimumRepetitions: 1},
	}}
	for _, item := range scenario.Suite() {
		result.Suites[0].Cases = append(result.Suites[0].Cases,
			CensusCase{Condition: item.Name, ID: item.Name})
	}
	for _, task := range meeting.Suite() {
		result.Suites[1].Cases = append(result.Suites[1].Cases,
			CensusCase{Condition: task.Category, ID: task.ID})
	}
	cases, err := realtimecu.Select(nil, []realtimecu.Grounding{
		realtimecu.GroundingPixel, realtimecu.GroundingSetOfMark,
	})
	if err != nil {
		panic("repository Realtime-CU suite is invalid: " + err.Error())
	}
	for _, item := range cases {
		result.Suites[2].Cases = append(result.Suites[2].Cases,
			CensusCase{Condition: string(item.Grounding), ID: item.ID()})
	}
	return sealCensus(result)
}

// ExternalCensusInput supplies already pinned dataset-manifest evidence and
// exact prepared dataset roots. Source references are retained in the census;
// callers must resolve their digests before registration.
type ExternalCensusInput struct {
	FDB15Root          string
	FDB15Manifest      []byte
	FDB15Source        EvidenceRef
	FDBV3Root          string
	FDBV3Manifest      []byte
	FDBV3Source        EvidenceRef
	FDBenchRoot        string
	FDBenchManifest    []byte
	FDBenchSource      EvidenceRef
	TauManifest        []byte
	TauSource          EvidenceRef
	TauInventory       []byte
	TauInventorySource EvidenceRef
}

// BuildExternalCensus enumerates exact task IDs from prepared datasets and
// verifies their released population manifests. It performs no model or
// network work.
func BuildExternalCensus(input ExternalCensusInput) (Census, error) {
	fdbExpected, err := parseFDB15Manifest(input.FDB15Manifest)
	if err != nil {
		return Census{}, err
	}
	fdbSamples, err := fdb.Load(input.FDB15Root, fdb.Categories(), 0)
	if err != nil {
		return Census{}, fmt.Errorf("enumerate FDB v1.5: %w", err)
	}
	fdbSuite := CensusSuite{Name: SuiteFDB15, MinimumRepetitions: 1}
	for _, sample := range fdbSamples {
		fdbSuite.Cases = append(fdbSuite.Cases,
			CensusCase{Condition: string(sample.Category), ID: sample.ID})
	}
	if err := validateConditionCounts(fdbSuite, fdbExpected); err != nil {
		return Census{}, fmt.Errorf("FDB v1.5 census: %w", err)
	}

	fdbv3Expected, err := parseFDBV3Manifest(input.FDBV3Manifest)
	if err != nil {
		return Census{}, err
	}
	fdbv3Tasks, err := fdbv3.Load(input.FDBV3Root, 0)
	if err != nil {
		return Census{}, fmt.Errorf("enumerate FDB v3: %w", err)
	}
	fdbv3Suite := CensusSuite{Name: SuiteFDBV3, MinimumRepetitions: 1}
	for _, task := range fdbv3Tasks {
		fdbv3Suite.Cases = append(fdbv3Suite.Cases,
			CensusCase{Condition: task.Domain, ID: task.ID})
	}
	if err := validateConditionCounts(fdbv3Suite, fdbv3Expected); err != nil {
		return Census{}, fmt.Errorf("FDB v3 census: %w", err)
	}

	fdExpected, err := parseFDBenchManifest(input.FDBenchManifest)
	if err != nil {
		return Census{}, err
	}
	conditions := make([]string, 0, len(fdExpected))
	for condition := range fdExpected {
		conditions = append(conditions, condition)
	}
	sort.Strings(conditions)
	conversations, err := fdbench.Load(input.FDBenchRoot, conditions, 0)
	if err != nil {
		return Census{}, fmt.Errorf("enumerate FD-Bench: %w", err)
	}
	fdSuite := CensusSuite{Name: SuiteFDBench, MinimumRepetitions: 1}
	for _, conversation := range conversations {
		fdSuite.Cases = append(fdSuite.Cases,
			CensusCase{Condition: conversation.Condition, ID: conversation.ID})
	}
	if err := validateConditionCounts(fdSuite, fdExpected); err != nil {
		return Census{}, fmt.Errorf("FD-Bench census: %w", err)
	}

	if err := validateTauManifest(input.TauManifest); err != nil {
		return Census{}, err
	}
	inventory, err := parseTauInventory(input.TauInventory)
	if err != nil {
		return Census{}, err
	}
	tauControl := CensusSuite{Name: SuiteTauControl, MinimumRepetitions: 1}
	tauRegular := CensusSuite{Name: SuiteTauRegular, MinimumRepetitions: 1}
	for _, task := range inventory.Tasks {
		caseID := task.Domain + "/" + task.ID
		item := CensusCase{Condition: task.Domain, ID: caseID}
		tauControl.Cases = append(tauControl.Cases, item)
		tauRegular.Cases = append(tauRegular.Cases, item)
	}

	result := Census{
		Version: CensusVersion,
		Sources: []EvidenceRef{input.FDB15Source, input.FDBV3Source,
			input.FDBenchSource, input.TauSource, input.TauInventorySource},
		Suites: []CensusSuite{fdbSuite, fdbv3Suite, fdSuite, tauControl, tauRegular},
	}
	result = sealCensus(result)
	if err := ValidateRequiredMatrix(MergeCensuses(RepositoryCensus(), result)); err != nil {
		return Census{}, err
	}
	return result, nil
}

// MergeCensuses combines disjoint census fragments and seals the result.
func MergeCensuses(parts ...Census) Census {
	result := Census{Version: CensusVersion, Sources: []EvidenceRef{}, Suites: []CensusSuite{}}
	for _, part := range parts {
		result.Sources = append(result.Sources, part.Sources...)
		result.Suites = append(result.Suites, part.Suites...)
	}
	return sealCensus(result)
}

func (census Census) ID() string {
	census = canonicalCensus(census)
	census.CensusID = ""
	return digestJSON(census)
}

func sealCensus(census Census) Census {
	census = canonicalCensus(census)
	census.CensusID = ""
	census.CensusID = digestJSON(census)
	return census
}

func canonicalCensus(input Census) Census {
	result := Census{Version: input.Version, CensusID: input.CensusID,
		Sources: append([]EvidenceRef(nil), input.Sources...),
		Suites:  make([]CensusSuite, len(input.Suites))}
	sort.Slice(result.Sources, func(left, right int) bool {
		a, b := result.Sources[left], result.Sources[right]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		return a.SHA256 < b.SHA256
	})
	for index, suite := range input.Suites {
		result.Suites[index] = CensusSuite{Name: suite.Name,
			MinimumRepetitions: suite.MinimumRepetitions,
			Cases:              append([]CensusCase(nil), suite.Cases...)}
		sort.Slice(result.Suites[index].Cases, func(left, right int) bool {
			a, b := result.Suites[index].Cases[left], result.Suites[index].Cases[right]
			if a.Condition != b.Condition {
				return a.Condition < b.Condition
			}
			return a.ID < b.ID
		})
	}
	sort.Slice(result.Suites, func(left, right int) bool {
		return result.Suites[left].Name < result.Suites[right].Name
	})
	return result
}

func (census Census) Validate() error {
	if census.Version != CensusVersion {
		return fmt.Errorf("census version must be %d, got %d", CensusVersion, census.Version)
	}
	if !validSHA256(census.CensusID) || census.ID() != census.CensusID {
		return errors.New("census digest does not match its content")
	}
	if len(census.Sources) > maxFixedAndTreatmentAxes {
		return fmt.Errorf("census source count exceeds %d", maxFixedAndTreatmentAxes)
	}
	seenSources := map[string]bool{}
	for _, source := range census.Sources {
		if strings.TrimSpace(source.Kind) == "" || strings.TrimSpace(source.Location) == "" ||
			!validSHA256(source.SHA256) {
			return errors.New("census source needs kind, location, and a lowercase SHA-256")
		}
		if err := canonicalCensusText("source kind", source.Kind); err != nil {
			return err
		}
		if err := canonicalCensusText("source location", source.Location); err != nil {
			return err
		}
		identity := source.Kind + "\x00" + source.Location
		if seenSources[identity] {
			return fmt.Errorf("census repeats source %q", source.Location)
		}
		seenSources[identity] = true
	}
	if len(census.Suites) > maxManifestSuites {
		return fmt.Errorf("census suite count exceeds %d", maxManifestSuites)
	}
	seenSuites := map[string]bool{}
	for _, suite := range census.Suites {
		if strings.TrimSpace(suite.Name) == "" || suite.MinimumRepetitions <= 0 ||
			suite.MinimumRepetitions > maxRepetitionsPerCase {
			return errors.New("census suite needs a name and positive repetition minimum")
		}
		if err := canonicalCensusText("suite name", suite.Name); err != nil {
			return err
		}
		if seenSuites[suite.Name] {
			return fmt.Errorf("census repeats suite %q", suite.Name)
		}
		seenSuites[suite.Name] = true
		if len(suite.Cases) == 0 {
			return fmt.Errorf("census suite %q has no cases", suite.Name)
		}
		if len(suite.Cases) > maxCasesPerSuite {
			return fmt.Errorf("census suite %q exceeds %d cases", suite.Name, maxCasesPerSuite)
		}
		seenCases := map[string]bool{}
		seenIDs := map[string]bool{}
		for _, item := range suite.Cases {
			if strings.TrimSpace(item.Condition) == "" || strings.TrimSpace(item.ID) == "" {
				return fmt.Errorf("census suite %q contains an empty condition or case ID", suite.Name)
			}
			if err := canonicalCensusText("case condition", item.Condition); err != nil {
				return err
			}
			if err := canonicalCensusText("case ID", item.ID); err != nil {
				return err
			}
			identity := item.Condition + "\x00" + item.ID
			if seenCases[identity] {
				return fmt.Errorf("census suite %q repeats case %q", suite.Name, item.ID)
			}
			if seenIDs[item.ID] {
				return fmt.Errorf("census suite %q uses case ID %q in more than one condition", suite.Name, item.ID)
			}
			seenCases[identity] = true
			seenIDs[item.ID] = true
		}
	}
	return nil
}

func canonicalCensusText(label, value string) error {
	var findings []Finding
	validateCanonicalText(&findings, "census.text", "census", label, value)
	if len(findings) > 0 {
		return errors.New(findings[0].Message)
	}
	return nil
}

// ValidateRequiredMatrix enforces the release census described by the
// migration plan. tau domain counts are intentionally not invented: the two
// modes must contain the same exact 278 prepared IDs across all three domains.
func ValidateRequiredMatrix(census Census) error {
	if err := census.Validate(); err != nil {
		return err
	}
	if len(census.Sources) < 5 {
		return fmt.Errorf("required migration census has %d external source artifacts, want at least 5", len(census.Sources))
	}
	byName := make(map[string]CensusSuite, len(census.Suites))
	for _, suite := range census.Suites {
		byName[suite.Name] = suite
	}
	required := map[string]map[string]int{
		SuiteScenario:   nil,
		SuiteMeeting:    nil,
		SuiteRealtimeCU: {"pixel": 8, "set_of_mark": 8},
		SuiteFDB15: {"background_speech": 100, "talking_to_other": 100,
			"user_backchannel": 98, "user_interruption": 200},
		SuiteFDBV3: {"travel_identity": 20, "finance_billing": 25,
			"housing_location": 26, "ecommerce_support": 29},
		SuiteFDBench:    requiredFDBenchConditions(),
		SuiteTauControl: nil,
		SuiteTauRegular: nil,
	}
	if len(byName) != len(required) {
		return fmt.Errorf("census has %d suites, required migration matrix has %d", len(byName), len(required))
	}
	for name, conditions := range required {
		suite, exists := byName[name]
		if !exists {
			return fmt.Errorf("census omits required suite %q", name)
		}
		if conditions != nil {
			if err := validateConditionCounts(suite, conditions); err != nil {
				return fmt.Errorf("suite %s: %w", name, err)
			}
		}
	}
	repository := RepositoryCensus()
	for _, exact := range repository.Suites {
		observed := byName[exact.Name]
		if canonicalJSON(canonicalCensus(Census{Version: CensusVersion, Suites: []CensusSuite{observed}}).Suites[0].Cases) !=
			canonicalJSON(canonicalCensus(Census{Version: CensusVersion, Suites: []CensusSuite{exact}}).Suites[0].Cases) {
			return fmt.Errorf("suite %q differs from the repository-owned exact task census", exact.Name)
		}
	}
	if got := len(byName[SuiteScenario].Cases); got != 11 || byName[SuiteScenario].MinimumRepetitions < 15 {
		return fmt.Errorf("scenario census must contain 11 cases with at least 15 repetitions, got %d and %d",
			got, byName[SuiteScenario].MinimumRepetitions)
	}
	if got := len(byName[SuiteMeeting].Cases); got != 4 {
		return fmt.Errorf("Meeting census has %d cases, want 4", got)
	}
	if got := len(byName[SuiteRealtimeCU].Cases); got != 16 {
		return fmt.Errorf("Realtime-CU census has %d cases, want 16", got)
	}
	if got := len(byName[SuiteFDB15].Cases); got != 498 {
		return fmt.Errorf("FDB v1.5 census has %d cases, want 498", got)
	}
	if got := len(byName[SuiteFDBV3].Cases); got != 100 {
		return fmt.Errorf("FDB v3 census has %d cases, want 100", got)
	}
	if got := len(byName[SuiteFDBench].Cases); got != 6147 {
		return fmt.Errorf("FD-Bench census has %d cases, want 6147", got)
	}
	control, regular := byName[SuiteTauControl], byName[SuiteTauRegular]
	if len(control.Cases) != tauvoice.TaskCount || len(regular.Cases) != tauvoice.TaskCount {
		return fmt.Errorf("tau-Voice modes must each contain %d cases, got %d and %d",
			tauvoice.TaskCount, len(control.Cases), len(regular.Cases))
	}
	if canonicalJSON(control.Cases) != canonicalJSON(regular.Cases) {
		return errors.New("tau-Voice control and regular modes do not enumerate the same exact tasks")
	}
	domains := map[string]bool{}
	for _, item := range control.Cases {
		domains[item.Condition] = true
	}
	for _, domain := range tauvoice.Domains {
		if !domains[domain] {
			return fmt.Errorf("tau-Voice census omits domain %q", domain)
		}
		delete(domains, domain)
	}
	if len(domains) != 0 {
		return errors.New("tau-Voice census contains an unknown domain")
	}
	return nil
}

// ValidateManifestCensus proves that the statistical plan enumerates every
// census case and no substitute. Repetition IDs may exceed the floor, but the
// exact attempt totals and populations must remain internally consistent via
// Manifest.Validate.
func ValidateManifestCensus(manifest Manifest, census Census) error {
	if err := ValidateRequiredMatrix(census); err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	byName := make(map[string]SuiteSpec, len(manifest.Suites))
	for _, suite := range canonicalManifest(manifest).Suites {
		byName[suite.Name] = suite
	}
	if len(byName) != len(census.Suites) {
		return fmt.Errorf("manifest has %d suites, census has %d", len(byName), len(census.Suites))
	}
	for _, expected := range canonicalCensus(census).Suites {
		actual, exists := byName[expected.Name]
		if !exists {
			return fmt.Errorf("manifest omits census suite %q", expected.Name)
		}
		if actual.MinimumRepetitions < expected.MinimumRepetitions {
			return fmt.Errorf("suite %q declares %d repetitions, census requires at least %d",
				expected.Name, actual.MinimumRepetitions, expected.MinimumRepetitions)
		}
		actualCases := make([]CensusCase, len(actual.Cases))
		for index, item := range actual.Cases {
			actualCases[index] = CensusCase{Condition: item.Condition, ID: item.ID}
		}
		sort.Slice(actualCases, func(left, right int) bool {
			if actualCases[left].Condition != actualCases[right].Condition {
				return actualCases[left].Condition < actualCases[right].Condition
			}
			return actualCases[left].ID < actualCases[right].ID
		})
		if canonicalJSON(actualCases) != canonicalJSON(expected.Cases) {
			return fmt.Errorf("suite %q task census differs from the authoritative matrix", expected.Name)
		}
		if len(actual.Cases) > 0 {
			repetitions := canonicalJSON(actual.Cases[0].Repetitions)
			for _, item := range actual.Cases[1:] {
				if canonicalJSON(item.Repetitions) != repetitions {
					return fmt.Errorf("suite %q does not use one preregistered repetition set for every case",
						expected.Name)
				}
			}
		}
	}
	return nil
}

func MarshalCensus(census Census) ([]byte, error) {
	if err := census.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalCensus(census), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeCensus(reader io.Reader) (Census, error) {
	if reader == nil {
		return Census{}, errors.New("decode census: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "suite census")
	if err != nil {
		return Census{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var census Census
	if err := decoder.Decode(&census); err != nil {
		return Census{}, fmt.Errorf("decode census: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Census{}, errors.New("decode census: trailing JSON value")
		}
		return Census{}, fmt.Errorf("decode census trailing content: %w", err)
	}
	if err := census.Validate(); err != nil {
		return Census{}, err
	}
	canonical, err := MarshalCensus(census)
	if err != nil {
		return Census{}, err
	}
	if string(payload) != string(canonical) {
		return Census{}, errors.New("decode suite census: artifact bytes are not canonical")
	}
	return canonicalCensus(census), nil
}

func parseFDB15Manifest(payload []byte) (map[string]int, error) {
	var document struct {
		Archives []struct {
			Scenario string `json:"scenario"`
			Observed int    `json:"observed_complete_samples"`
		} `json:"archives"`
		Total int `json:"observed_complete_samples"`
	}
	if err := decodeStrictDocument(payload, &document); err != nil {
		return nil, fmt.Errorf("decode FDB v1.5 manifest: %w", err)
	}
	result := map[string]int{}
	for _, item := range document.Archives {
		result[item.Scenario] = item.Observed
	}
	if sumCounts(result) != document.Total || document.Total != 498 {
		return nil, errors.New("FDB v1.5 manifest does not pin the required 498 samples")
	}
	return result, nil
}

func parseFDBV3Manifest(payload []byte) (map[string]int, error) {
	var document struct {
		Released struct {
			Audio   int            `json:"audio_examples"`
			Domains map[string]int `json:"domains"`
		} `json:"released_artifact"`
	}
	if err := decodeStrictDocument(payload, &document); err != nil {
		return nil, fmt.Errorf("decode FDB v3 manifest: %w", err)
	}
	if document.Released.Audio != 100 || sumCounts(document.Released.Domains) != 100 {
		return nil, errors.New("FDB v3 manifest does not pin 100 released examples")
	}
	return document.Released.Domains, nil
}

func parseFDBenchManifest(payload []byte) (map[string]int, error) {
	var document struct {
		Populations map[string]int `json:"expected_cell_populations"`
		Cells       int            `json:"expected_cell_count"`
		Total       int            `json:"expected_released_conversations"`
	}
	if err := decodeStrictDocument(payload, &document); err != nil {
		return nil, fmt.Errorf("decode FD-Bench manifest: %w", err)
	}
	if document.Cells != 21 || len(document.Populations) != 21 || document.Total != 6147 || sumCounts(document.Populations) != 6147 {
		return nil, errors.New("FD-Bench manifest does not pin 6,147 conversations across 21 conditions")
	}
	if canonicalJSON(document.Populations) != canonicalJSON(requiredFDBenchConditions()) {
		return nil, errors.New("FD-Bench manifest condition populations differ from the reviewed release census")
	}
	return document.Populations, nil
}

func validateTauManifest(payload []byte) error {
	var document struct {
		Benchmark struct {
			Count      int      `json:"paper_task_count"`
			Conditions []string `json:"speech_conditions"`
			Domains    []string `json:"domains"`
		} `json:"benchmark"`
		Source struct {
			Revision string `json:"repository_revision"`
		} `json:"source"`
	}
	if err := decodeStrictDocument(payload, &document); err != nil {
		return fmt.Errorf("decode tau-Voice manifest: %w", err)
	}
	if document.Benchmark.Count != tauvoice.TaskCount || document.Source.Revision != tauvoice.PinnedRevision {
		return errors.New("tau-Voice manifest task count or pinned revision differs")
	}
	if canonicalJSON(sortedStrings(document.Benchmark.Conditions)) != canonicalJSON([]string{"control", "regular"}) ||
		canonicalJSON(sortedStrings(document.Benchmark.Domains)) != canonicalJSON([]string{"airline", "retail", "telecom"}) {
		return errors.New("tau-Voice manifest does not pin control/regular across all domains")
	}
	return nil
}

func parseTauInventory(payload []byte) (TauTaskInventory, error) {
	inventory, err := tauvoice.DecodeTaskInventory(bytes.NewReader(payload))
	if err != nil {
		return TauTaskInventory{}, err
	}
	return inventory, nil
}

func decodeStrictDocument(payload []byte, target any) error {
	if len(payload) == 0 {
		return errors.New("document is empty")
	}
	// Dataset pins contain intentionally richer fields. Unknown fields are
	// retained by their source artifact digest, while the population fields we
	// consume are decoded explicitly. Duplicate keys and trailing values are
	// rejected before typed decoding because encoding/json otherwise accepts a
	// later duplicate as an unsigned override of an earlier population field.
	if err := strictjson.Validate(payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, target)
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func validateConditionCounts(suite CensusSuite, expected map[string]int) error {
	actual := map[string]int{}
	for _, item := range suite.Cases {
		actual[item.Condition]++
	}
	if canonicalJSON(actual) != canonicalJSON(expected) {
		return fmt.Errorf("condition populations are %s, want %s", canonicalJSON(actual), canonicalJSON(expected))
	}
	return nil
}

func requiredFDBenchConditions() map[string]int {
	return map[string]int{
		"chattts-single-round-combine-easy":                   291,
		"chattts-single-round-combine-hard":                   291,
		"chattts-single-round-combine-med":                    291,
		"cosyvoice2-single-round-combine-easy":                293,
		"cosyvoice2-single-round-combine-easy-noisy-bg-0dB":   293,
		"cosyvoice2-single-round-combine-easy-noisy-bg-10dB":  293,
		"cosyvoice2-single-round-combine-easy-noisy-bg-20dB":  293,
		"cosyvoice2-single-round-combine-easy-noisy-gap-0dB":  293,
		"cosyvoice2-single-round-combine-easy-noisy-gap-10dB": 293,
		"cosyvoice2-single-round-combine-easy-noisy-gap-20dB": 293,
		"cosyvoice2-single-round-combine-hard":                293,
		"cosyvoice2-single-round-combine-med":                 293,
		"f5tts-single-round-combine-easy":                     293,
		"f5tts-single-round-combine-easy-noisy-bg-0dB":        293,
		"f5tts-single-round-combine-easy-noisy-bg-10dB":       293,
		"f5tts-single-round-combine-easy-noisy-bg-20dB":       293,
		"f5tts-single-round-combine-easy-noisy-gap-0dB":       293,
		"f5tts-single-round-combine-easy-noisy-gap-10dB":      293,
		"f5tts-single-round-combine-easy-noisy-gap-20dB":      293,
		"f5tts-single-round-combine-hard":                     293,
		"f5tts-single-round-combine-med":                      293,
	}
}

func sumCounts(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
