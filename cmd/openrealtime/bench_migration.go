package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/migration"
)

func runMigration(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime bench migration <archive|census|register|compare> [flags]")
	}
	switch strings.ToLower(strings.TrimSpace(arguments[0])) {
	case "archive":
		return runMigrationArchive(arguments[1:], output)
	case "census":
		return runMigrationCensus(arguments[1:], output)
	case "register":
		return runMigrationRegister(arguments[1:], output)
	case "compare":
		return runMigrationCompare(arguments[1:], output)
	default:
		return fmt.Errorf("unknown migration command %q", arguments[0])
	}
}

func runMigrationArchive(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench migration archive", flag.ContinueOnError)
	storePath := flags.String("store", "", "create-only migration evidence-store root")
	kind := flags.String("kind", "", "immutable evidence kind")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*storePath) == "" || strings.TrimSpace(*kind) == "" || flags.NArg() == 0 {
		return errors.New("migration archive requires -store, -kind, and one or more artifact paths")
	}
	store := migration.LocalStore{Root: *storePath}
	for _, path := range flags.Args() {
		reference, err := store.Archive(*kind, path)
		if err != nil {
			return err
		}
		if err := writeMigrationReference(output, reference); err != nil {
			return err
		}
	}
	return nil
}

func runMigrationCensus(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench migration census", flag.ContinueOnError)
	storePath := flags.String("store", "", "create-only migration evidence-store root")
	fdbRoot := flags.String("fdb-dataset", ".runtime/full-duplex-bench-v1.5/dataset", "prepared FDB v1.5 dataset root")
	fdbManifest := flags.String("fdb-manifest", "datasets/manifests/full-duplex-bench-v1.5.json", "reviewed FDB v1.5 population manifest")
	fdbv3Root := flags.String("fdbv3-dataset", ".runtime/full-duplex-bench-v3/dataset/fdb_v3_data_released", "prepared FDB v3 dataset root")
	fdbv3Manifest := flags.String("fdbv3-manifest", "datasets/manifests/full-duplex-bench-v3.json", "reviewed FDB v3 population manifest")
	fdbenchRoot := flags.String("fdbench-dataset", ".runtime/fd-bench/dataset", "prepared FD-Bench dataset root")
	fdbenchManifest := flags.String("fdbench-manifest", "datasets/manifests/fd-bench.json", "reviewed FD-Bench population manifest")
	tauManifest := flags.String("tau-manifest", "datasets/manifests/tau-voice.json", "reviewed tau-Voice population manifest")
	tauInventory := flags.String("tau-inventory", "", "exact 278-task inventory exported from the pinned tau2 checkout")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*storePath) == "" || strings.TrimSpace(*tauInventory) == "" {
		return errors.New("migration census requires -store and -tau-inventory and accepts flags only")
	}
	store := migration.LocalStore{Root: *storePath}
	readAndArchive := func(path string) ([]byte, migration.EvidenceRef, error) {
		payload, err := os.ReadFile(path)
		if err != nil {
			return nil, migration.EvidenceRef{}, err
		}
		reference, err := store.Archive("census-source", path)
		return payload, reference, err
	}
	fdbBytes, fdbRef, err := readAndArchive(*fdbManifest)
	if err != nil {
		return err
	}
	fdbv3Bytes, fdbv3Ref, err := readAndArchive(*fdbv3Manifest)
	if err != nil {
		return err
	}
	fdbenchBytes, fdbenchRef, err := readAndArchive(*fdbenchManifest)
	if err != nil {
		return err
	}
	tauBytes, tauRef, err := readAndArchive(*tauManifest)
	if err != nil {
		return err
	}
	inventoryBytes, inventoryRef, err := readAndArchive(*tauInventory)
	if err != nil {
		return err
	}
	external, err := migration.BuildExternalCensus(migration.ExternalCensusInput{
		FDB15Root: *fdbRoot, FDB15Manifest: fdbBytes, FDB15Source: fdbRef,
		FDBV3Root: *fdbv3Root, FDBV3Manifest: fdbv3Bytes, FDBV3Source: fdbv3Ref,
		FDBenchRoot: *fdbenchRoot, FDBenchManifest: fdbenchBytes, FDBenchSource: fdbenchRef,
		TauManifest: tauBytes, TauSource: tauRef,
		TauInventory: inventoryBytes, TauInventorySource: inventoryRef,
	})
	if err != nil {
		return err
	}
	census := migration.MergeCensuses(migration.RepositoryCensus(), external)
	if err := migration.ValidateRequiredMatrix(census); err != nil {
		return err
	}
	payload, err := migration.MarshalCensus(census)
	if err != nil {
		return err
	}
	reference, err := store.ArchiveBytes(migration.EvidenceKindCensus, ".json", payload)
	if err != nil {
		return err
	}
	return writeMigrationReference(output, reference)
}

type migrationPaths []string

func (paths *migrationPaths) String() string { return strings.Join(*paths, ",") }
func (paths *migrationPaths) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("path must not be empty")
	}
	*paths = append(*paths, value)
	return nil
}

func runMigrationRegister(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench migration register", flag.ContinueOnError)
	storePath := flags.String("store", "", "create-only migration evidence-store root")
	manifestPath := flags.String("manifest", "", "canonical migration manifest JSON")
	censusPath := flags.String("census", "", "canonical required census JSON")
	location := flags.String("location", "", "new logical registration location inside the store")
	var profilePaths migrationPaths
	flags.Var(&profilePaths, "profile", "canonical suite import profile JSON; repeat for all eight suites")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*storePath) == "" ||
		strings.TrimSpace(*manifestPath) == "" || strings.TrimSpace(*censusPath) == "" ||
		strings.TrimSpace(*location) == "" || len(profilePaths) == 0 {
		return errors.New("migration register requires -store, -manifest, -census, -location, and repeated -profile")
	}
	store := migration.LocalStore{Root: *storePath}
	manifestReference, err := store.Archive(migration.EvidenceKindManifest, *manifestPath)
	if err != nil {
		return err
	}
	censusReference, err := store.Archive(migration.EvidenceKindCensus, *censusPath)
	if err != nil {
		return err
	}
	profileReferences := make([]migration.EvidenceRef, 0, len(profilePaths))
	for _, path := range profilePaths {
		reference, err := store.Archive(migration.EvidenceKindImportProfile, path)
		if err != nil {
			return err
		}
		profileReferences = append(profileReferences, reference)
	}
	_, reference, err := migration.RegisterStudy(store, *location, manifestReference,
		censusReference, profileReferences, time.Now().UTC())
	if err != nil {
		return err
	}
	return writeMigrationReference(output, reference)
}

func runMigrationCompare(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime bench migration compare", flag.ContinueOnError)
	storePath := flags.String("store", "", "create-only migration evidence-store root")
	registrationLocation := flags.String("registration", "", "registration location inside the store")
	registrationSHA := flags.String("registration-sha256", "", "registration artifact SHA-256")
	reportLocation := flags.String("report", "", "new logical report location inside the store")
	out := flags.String("out", "", "also write the canonical retained report to this create-only path")
	var baselinePaths, candidatePaths, baselineOutcomes, candidateOutcomes, historyPaths migrationPaths
	flags.Var(&baselinePaths, "baseline", "unwrapped suite[:repetition]=result.json; retained as a comparison refusal")
	flags.Var(&candidatePaths, "candidate", "unwrapped suite[:repetition]=result.json; retained as a comparison refusal")
	flags.Var(&baselineOutcomes, "baseline-outcome", "suite=launch-outcome-location@sha256")
	flags.Var(&candidateOutcomes, "candidate-outcome", "suite=launch-outcome-location@sha256")
	flags.Var(&historyPaths, "history", "canonical predecessor report JSON; repeat for complete lineage")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*storePath) == "" ||
		strings.TrimSpace(*registrationLocation) == "" || strings.TrimSpace(*registrationSHA) == "" ||
		strings.TrimSpace(*reportLocation) == "" {
		return errors.New("migration compare requires store, registration, and report flags")
	}
	store := migration.LocalStore{Root: *storePath}
	registration, err := migrationRegistrationReference(*registrationLocation, *registrationSHA)
	if err != nil {
		return err
	}
	archiveInputs := func(values []string) ([]migration.ResultImport, error) {
		result := make([]migration.ResultImport, 0, len(values))
		for _, value := range values {
			suite, repetition, path, err := parseMigrationResultInput(value)
			if err != nil {
				return nil, err
			}
			reference, err := store.Archive(migration.EvidenceKindResult, path)
			if err != nil {
				return nil, err
			}
			result = append(result, migration.ResultImport{
				Suite: suite, Repetition: repetition, Reference: reference,
			})
		}
		return result, nil
	}
	baseline, err := archiveInputs(baselinePaths)
	if err != nil {
		return err
	}
	candidate, err := archiveInputs(candidatePaths)
	if err != nil {
		return err
	}
	appendOutcomes := func(target []migration.ResultImport, values []string) ([]migration.ResultImport, error) {
		for _, value := range values {
			suite, reference, err := parseMigrationOutcomeInput(value)
			if err != nil {
				return nil, err
			}
			target = append(target, migration.ResultImport{Suite: suite, Outcome: reference})
		}
		return target, nil
	}
	baseline, err = appendOutcomes(baseline, baselineOutcomes)
	if err != nil {
		return err
	}
	candidate, err = appendOutcomes(candidate, candidateOutcomes)
	if err != nil {
		return err
	}
	history := make([]migration.EvidenceRef, 0, len(historyPaths))
	for _, path := range historyPaths {
		reference, err := store.Archive(migration.EvidenceKindReport, path)
		if err != nil {
			return err
		}
		history = append(history, reference)
	}
	report, reference, err := migration.CompareRegistered(store, registration,
		baseline, candidate, history, *reportLocation)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*out) != "" {
		resolvedHistory, err := migration.ResolveReportHistory(store, history)
		if err != nil {
			return fmt.Errorf("resolve migration report export history: %w", err)
		}
		if err := report.WriteWithHistory(*out, resolvedHistory); err != nil {
			return fmt.Errorf("write migration report export: %w", err)
		}
	}
	if err := writeMigrationReference(output, reference); err != nil {
		return err
	}
	fmt.Fprintf(output, "reportable=%t accepted=%t report_id=%s\n",
		report.Reportable, report.Accepted, report.ReportID)
	if !report.Reportable || !report.Accepted {
		return errors.New("migration comparison was retained but did not pass")
	}
	return nil
}

func parseMigrationResultInput(value string) (suite, repetition, path string, err error) {
	left, path, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(left) == "" || strings.TrimSpace(path) == "" {
		return "", "", "", fmt.Errorf("invalid result input %q; want suite[:repetition]=path", value)
	}
	suite = strings.TrimSpace(left)
	if before, after, found := strings.Cut(left, ":"); found {
		suite, repetition = strings.TrimSpace(before), strings.TrimSpace(after)
		if suite == "" || repetition == "" {
			return "", "", "", fmt.Errorf("invalid result input %q; want suite[:repetition]=path", value)
		}
	}
	return suite, repetition, strings.TrimSpace(path), nil
}

func parseMigrationOutcomeInput(value string) (string, migration.EvidenceRef, error) {
	suite, encoded, found := strings.Cut(value, "=")
	if !found || strings.TrimSpace(suite) == "" {
		return "", migration.EvidenceRef{}, fmt.Errorf(
			"invalid launch outcome %q; want suite=location@sha256", value)
	}
	separator := strings.LastIndex(encoded, "@")
	if separator <= 0 || separator == len(encoded)-1 {
		return "", migration.EvidenceRef{}, fmt.Errorf(
			"invalid launch outcome %q; want suite=location@sha256", value)
	}
	reference := migration.EvidenceRef{
		Kind:     migration.EvidenceKindLaunchOutcome,
		Location: strings.TrimSpace(encoded[:separator]), SHA256: strings.TrimSpace(encoded[separator+1:]),
	}
	if reference.Location == "" || len(reference.SHA256) != 64 ||
		reference.SHA256 != strings.ToLower(reference.SHA256) {
		return "", migration.EvidenceRef{}, fmt.Errorf("invalid launch outcome %q", value)
	}
	if _, err := hex.DecodeString(reference.SHA256); err != nil {
		return "", migration.EvidenceRef{}, fmt.Errorf("invalid launch outcome %q", value)
	}
	return strings.TrimSpace(suite), reference, nil
}

func migrationRegistrationReference(location, digest string) (migration.EvidenceRef, error) {
	reference := migration.EvidenceRef{
		Kind:     migration.EvidenceKindRegistration,
		Location: strings.TrimSpace(location), SHA256: strings.TrimSpace(digest),
	}
	if reference.Location == "" || len(reference.SHA256) != 64 {
		return migration.EvidenceRef{}, errors.New("migration registration needs a location and lowercase SHA-256")
	}
	if reference.SHA256 != strings.ToLower(reference.SHA256) {
		return migration.EvidenceRef{}, errors.New("migration registration SHA-256 must be lowercase hexadecimal")
	}
	if _, err := hex.DecodeString(reference.SHA256); err != nil {
		return migration.EvidenceRef{}, errors.New("migration registration SHA-256 must be lowercase hexadecimal")
	}
	return reference, nil
}

func writeMigrationReference(output io.Writer, reference migration.EvidenceRef) error {
	payload, err := json.Marshal(reference)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(payload))
	return err
}

type migrationLaunchFlags struct {
	storePath            string
	registrationLocation string
	registrationSHA      string
	arm                  string
	repetition           string
	intentReference      migration.EvidenceRef
}

func (settings *migrationLaunchFlags) bind(flags *flag.FlagSet) {
	flags.StringVar(&settings.storePath, "migration-store", "", "enable preregistered migration mode with this evidence store")
	flags.StringVar(&settings.registrationLocation, "migration-registration", "", "create-only registration location inside the migration store")
	flags.StringVar(&settings.registrationSHA, "migration-registration-sha256", "", "registration artifact SHA-256")
	flags.StringVar(&settings.arm, "migration-arm", "", "registered arm: baseline or candidate")
	flags.StringVar(&settings.repetition, "migration-repetition", "", "registered repetition for a one-shot suite result")
}

func (settings migrationLaunchFlags) enabled() bool {
	return settings.storePath != "" || settings.registrationLocation != "" ||
		settings.registrationSHA != "" || settings.arm != "" || settings.repetition != ""
}

type migrationLaunchCase struct {
	Condition string
	ID        string
}

func (settings *migrationLaunchFlags) preflight(
	suite string, selected []migrationLaunchCase, repetitions []string, cell bench.Cell,
) error {
	if !settings.enabled() {
		return nil
	}
	if strings.TrimSpace(settings.storePath) == "" || strings.TrimSpace(settings.registrationLocation) == "" ||
		strings.TrimSpace(settings.registrationSHA) == "" || strings.TrimSpace(settings.arm) == "" {
		return errors.New("migration launch requires store, registration, registration SHA-256, and arm flags")
	}
	arm := migration.Arm(strings.ToLower(strings.TrimSpace(settings.arm)))
	if arm != migration.ArmBaseline && arm != migration.ArmCandidate {
		return fmt.Errorf("migration arm must be baseline or candidate, got %q", settings.arm)
	}
	if len(repetitions) == 0 {
		if strings.TrimSpace(settings.repetition) == "" {
			return errors.New("one-shot migration suite requires -migration-repetition")
		}
		repetitions = []string{strings.TrimSpace(settings.repetition)}
	} else if strings.TrimSpace(settings.repetition) != "" {
		return errors.New("suite-managed trials cannot also set -migration-repetition")
	}
	registration, err := migrationRegistrationReference(settings.registrationLocation, settings.registrationSHA)
	if err != nil {
		return err
	}
	store := migration.LocalStore{Root: settings.storePath}
	observedAt := time.Now().UTC()
	expected, err := migration.RegisteredSuiteKeys(store, registration, suite, observedAt)
	if err != nil {
		return err
	}
	wantedRepetitions := map[string]bool{}
	for _, repetition := range repetitions {
		wantedRepetitions[strings.TrimSpace(repetition)] = true
	}
	wantedCases := map[string]bool{}
	for _, item := range selected {
		wantedCases[item.Condition+"\x00"+item.ID] = true
	}
	foundCases := map[string]bool{}
	keys := make([]migration.AttemptKey, 0)
	for _, key := range expected {
		if !wantedRepetitions[key.Repetition] {
			continue
		}
		identity := key.Condition + "\x00" + key.Case
		if len(wantedCases) > 0 && !wantedCases[identity] {
			continue
		}
		foundCases[identity] = true
		keys = append(keys, key)
	}
	for identity := range wantedCases {
		if !foundCases[identity] {
			return fmt.Errorf("selected migration case %q is absent from the registered matrix", identity)
		}
	}
	_, intentReference, err := migration.RegisterLaunchIntent(store, registration, migration.LaunchRequest{
		Arm: arm, Suite: suite, Keys: keys, Cell: cell,
		Provenance: bench.Capture(), ObservedAt: observedAt,
	})
	if err != nil {
		return err
	}
	settings.intentReference = intentReference
	return nil
}

func (settings *migrationLaunchFlags) retain(result any, output io.Writer) error {
	if !settings.enabled() {
		return nil
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if settings.intentReference.Kind != migration.EvidenceKindLaunchIntent {
		return errors.New("migration result cannot be retained without its preregistered launch intent")
	}
	resultReference, outcomeReference, err := migration.RetainLaunchOutcome(
		migration.LocalStore{Root: settings.storePath}, settings.intentReference, payload)
	if err != nil {
		if resultReference.Kind != "" {
			_ = writeMigrationReference(output, resultReference)
		}
		return err
	}
	if err := writeMigrationReference(output, resultReference); err != nil {
		return err
	}
	return writeMigrationReference(output, outcomeReference)
}
