package dynacu

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	dynaCUEvidenceFormat        = "openrealtime.dynacu-wire-evidence"
	dynaCUEvidenceFormatVersion = 1
	dynaCUTimestampBasis        = "task_recorder_monotonic_elapsed_microseconds"
	maximumDynaCUEvidenceFile   = int64(128 << 20)
	maximumDynaCUTraceFile      = int64(64 << 20)
	maximumDynaCUAttempts       = 10_000
)

var errDynaCURawManifestMissing = errors.New("DynaCU raw attempt has no sealed manifest")

type dynaCURawAttempt struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	TaskID        string `json:"task_id"`
	Category      string `json:"category"`
	Difficulty    string `json:"difficulty"`
	Terminal      string `json:"terminal"`
}

type dynaCURawFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Kind      string `json:"kind,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Role      string `json:"role,omitempty"`
}

type dynaCURawManifest struct {
	Format             string                   `json:"format"`
	FormatVersion      int                      `json:"format_version"`
	Complete           bool                     `json:"complete"`
	TaskID             string                   `json:"task_id"`
	Category           string                   `json:"category"`
	Difficulty         string                   `json:"difficulty"`
	DurationUS         int64                    `json:"duration_us"`
	TimestampBasis     string                   `json:"timestamp_basis"`
	ProcessInterrupted bool                     `json:"process_interrupted"`
	AudioChunkCount    int                      `json:"audio_chunk_count"`
	FrameCount         int                      `json:"frame_count"`
	ActionCount        int                      `json:"action_count"`
	CaptureErrors      []string                 `json:"capture_errors"`
	Files              map[string]dynaCURawFile `json:"files"`
	Terminal           string                   `json:"terminal"`
}

type dynaCUWireEvent struct {
	Kind        string `json:"kind"`
	AtUS        int64  `json:"at_us"`
	Path        string `json:"path,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	SampleCount int64  `json:"sample_count,omitempty"`
	EventType   string `json:"event_type,omitempty"`
}

type dynaCUAction struct {
	AtUS      int64           `json:"at_us"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Error     bool            `json:"error"`
}

type dynaCUTranscript struct {
	AtUS int64  `json:"at_us"`
	Text string `json:"text"`
}

type dynaCURawTrace struct {
	Format              string             `json:"format"`
	FormatVersion       int                `json:"format_version"`
	TaskID              string             `json:"task_id"`
	Category            string             `json:"category"`
	Difficulty          string             `json:"difficulty"`
	DurationUS          int64              `json:"duration_us"`
	TimestampBasis      string             `json:"timestamp_basis"`
	ProcessInterrupted  bool               `json:"process_interrupted"`
	Actions             []dynaCUAction     `json:"actions"`
	Transcripts         []dynaCUTranscript `json:"transcripts"`
	WireEvents          []dynaCUWireEvent  `json:"wire_events"`
	DeterministicResult json.RawMessage    `json:"deterministic_result"`
	CaptureErrors       []string           `json:"capture_errors"`
}

type dynaCUWireIndex struct {
	Format         string            `json:"format"`
	FormatVersion  int               `json:"format_version"`
	TimestampBasis string            `json:"timestamp_basis"`
	Events         []dynaCUWireEvent `json:"events"`
}

type dynaCUJournalRecord struct {
	Kind           string            `json:"kind"`
	AtUS           int64             `json:"at_us,omitempty"`
	TimestampBasis string            `json:"timestamp_basis,omitempty"`
	Event          *dynaCUWireEvent  `json:"event,omitempty"`
	Action         *dynaCUAction     `json:"action,omitempty"`
	Transcript     *dynaCUTranscript `json:"transcript,omitempty"`
}

type dynaCUJournal struct {
	Wire        []dynaCUWireEvent
	Actions     []dynaCUAction
	Transcripts []dynaCUTranscript
}

type dynaCUEvidenceEntry struct {
	directory  string
	attempt    dynaCURawAttempt
	manifest   *dynaCURawManifest
	trace      *dynaCURawTrace
	traceBytes []byte
	wireBytes  []byte
	mediaBytes []byte
	problem    error
}

type dynaCUEvidenceInventory struct {
	identity os.FileInfo
	entries  []dynaCUEvidenceEntry
}

func (config Config) validateAttemptEvidence() error {
	if err := candidate.RequirePlugin(config.Evidence); err != nil {
		return fmt.Errorf("DynaCU candidate evidence: %w", err)
	}
	if config.Resume {
		return errors.New("DynaCU candidate attempts are create-only; resume is recovery, not a new run")
	}
	if config.WithoutImages {
		return errors.New("DynaCU -no-images cannot produce the mandatory computer-use review video")
	}
	if err := config.EvidenceOrigin.Validate(); err != nil {
		return errors.New("DynaCU candidate evidence origin is invalid")
	}
	if err := validateDynaCUFreshPath("deterministic result", config.Output); err != nil {
		return err
	}
	if err := validateDynaCUFreshPath("raw evidence", config.EvidenceDirectory); err != nil {
		return err
	}
	if withinDynaCUPath(config.Output, config.EvidenceDirectory) ||
		withinDynaCUPath(config.EvidenceDirectory, config.Output) {
		return errors.New("DynaCU deterministic result and raw evidence paths must be independent")
	}
	return nil
}

func validateDynaCUFreshPath(label, path string) error {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path || path == filepath.Dir(path) {
		return errors.New("DynaCU " + label + " path must be a clean absolute non-root path")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("DynaCU " + label + " path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect DynaCU " + label + " path")
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("DynaCU " + label + " path ancestry is invalid")
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

func withinDynaCUPath(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (config Config) retainEvidence(
	ctx context.Context, lifecycle *candidate.Lifecycle, result *bench.Result,
) (resultErr error) {
	if ctx == nil || lifecycle == nil || result == nil {
		return errors.New("retain DynaCU evidence: nil context, lifecycle, or result")
	}
	inventory, snapshotErr := listDynaCUEvidence(ctx, config.EvidenceDirectory)
	resultErr = errors.Join(resultErr, snapshotErr)
	records, recordErr := readDynaCURecordMap(config.Output)
	resultErr = errors.Join(resultErr, recordErr)
	byTask := make(map[string]dynaCUEvidenceEntry, len(inventory.entries))
	for _, entry := range inventory.entries {
		if _, duplicate := byTask[entry.attempt.TaskID]; duplicate {
			resultErr = errors.Join(resultErr, errors.New("DynaCU raw attempt identity is duplicated"))
			continue
		}
		byTask[entry.attempt.TaskID] = entry
	}
	rows := make(map[string]bench.TaskOutcome, len(result.Tasks))
	order := make([]string, 0, len(result.Tasks)+len(inventory.entries))
	for _, outcome := range result.Tasks {
		if _, duplicate := rows[outcome.ID]; duplicate || strings.TrimSpace(outcome.ID) == "" {
			resultErr = errors.Join(resultErr, errors.New("DynaCU deterministic task identity is duplicated or empty"))
			continue
		}
		rows[outcome.ID] = outcome
		order = append(order, outcome.ID)
	}
	var extra []string
	for taskID := range byTask {
		if _, found := rows[taskID]; !found {
			extra = append(extra, taskID)
		}
	}
	sort.Strings(extra)
	for _, taskID := range extra {
		entry := byTask[taskID]
		rows[taskID] = bench.TaskOutcome{
			ID: taskID, Error: "external harness ended before publishing a deterministic task result",
			Notes: map[string]string{
				"category": entry.attempt.Category, "difficulty": entry.attempt.Difficulty,
			},
		}
		order = append(order, taskID)
	}
	result.Tasks = result.Tasks[:0]
	for _, taskID := range order {
		outcome := rows[taskID]
		entry, found := byTask[taskID]
		if found {
			loaded, loadErr := readDynaCUEvidenceEntryFromDirectory(
				config.EvidenceDirectory, entry.directory,
			)
			if errors.Is(loadErr, errDynaCURawManifestMissing) {
				recoveryErr := config.recoverDynaCUAttempt(ctx, entry.directory)
				resultErr = errors.Join(resultErr, recoveryErr)
				if recoveryErr == nil {
					loaded, loadErr = readDynaCUEvidenceEntryFromDirectory(
						config.EvidenceDirectory, entry.directory,
					)
				}
			}
			if loadErr != nil {
				loaded.attempt = entry.attempt
				loaded.directory = entry.directory
				loaded.problem = loadErr
				resultErr = errors.Join(resultErr, errors.New("one DynaCU raw attempt is incomplete or invalid"))
			}
			entry = loaded
			retained, hasRecord := records[taskID]
			if !hasRecord {
				if entry.trace == nil {
					entry.problem = errors.Join(entry.problem, errors.New("deterministic row is missing"))
				} else {
					var recovered record
					if err := decodeDynaCURawJSON(entry.trace.DeterministicResult, &recovered, 8<<20); err != nil ||
						recovered.TaskID != taskID || recovered.Category != entry.attempt.Category ||
						recovered.Difficulty != entry.attempt.Difficulty ||
						entry.manifest != nil && entry.manifest.ProcessInterrupted &&
							!strings.HasPrefix(recovered.Error, "INCOMPLETE:") {
						entry.problem = errors.Join(entry.problem, errors.New("sealed trace result is invalid"))
					} else {
						outcome = recovered.outcome()
					}
				}
			} else if entry.trace != nil {
				if err := validateDynaCUTraceResult(*entry.trace, retained); err != nil {
					entry.problem = errors.Join(entry.problem, err)
					resultErr = errors.Join(resultErr, errors.New("one DynaCU trace differs from its deterministic row"))
				}
			}
		}
		contextValue := map[string]any{
			"benchmark_revision":      PinnedRevision,
			"deterministic_authority": "pinned upstream DynaCU scorer is authoritative",
			"model":                   config.Model,
			"max_steps":               config.MaxSteps,
			"step_interval_ms":        config.StepInterval.Milliseconds(),
			"images_sent":             !config.WithoutImages,
			"page_elements_sent":      !config.WithoutPageElements,
			"recorder_format":         dynaCUEvidenceFormat,
			"recorder_format_version": dynaCUEvidenceFormatVersion,
		}
		if found {
			contextValue["category"] = entry.attempt.Category
			contextValue["difficulty"] = entry.attempt.Difficulty
			if entry.manifest != nil {
				contextValue["duration_us"] = entry.manifest.DurationUS
				contextValue["audio_chunks"] = entry.manifest.AudioChunkCount
				contextValue["video_frames"] = entry.manifest.FrameCount
				contextValue["actions"] = entry.manifest.ActionCount
				contextValue["capture_errors"] = entry.manifest.CaptureErrors
				contextValue["process_interrupted"] = entry.manifest.ProcessInterrupted
				contextValue["timestamp_basis"] = entry.manifest.TimestampBasis
			}
		} else {
			contextValue["capture_errors"] = []string{"raw_attempt_missing"}
		}
		attempt, beginErr := lifecycle.BeginExternal(taskID, 1, contextValue)
		if beginErr != nil {
			resultErr = errors.Join(resultErr, beginErr)
			result.Tasks = append(result.Tasks, outcome)
			continue
		}
		if !found {
			_ = attempt.RecordFailure("raw evidence", errors.New("raw attempt is missing"))
		} else {
			resultErr = errors.Join(resultErr, retainDynaCUAttemptEvidence(attempt, entry))
		}
		transcript := dynaCUAttemptTranscript(entry, outcome)
		resultErr = errors.Join(resultErr, attempt.Complete(outcome, transcript))
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	if config.Restricted() {
		result.Summary.Complete = false
		result.Summary.Incompleteness = "the run was restricted to a subset of the suite"
	}
	if err := verifyDynaCUEvidenceRoot(config.EvidenceDirectory, inventory.identity); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	return resultErr
}

func retainDynaCUAttemptEvidence(
	attempt *candidate.ActiveAttempt, entry dynaCUEvidenceEntry,
) error {
	if entry.problem != nil || entry.manifest == nil || entry.trace == nil {
		return attempt.RecordFailure("validate raw evidence", errors.New("raw task evidence is incomplete or invalid"))
	}
	var resultErr error
	if len(entry.mediaBytes) > 0 {
		resultErr = errors.Join(resultErr, attempt.CaptureMedia(candidate.CapturedMedia{
			Name: "review.mp4", Kind: "video", Role: "synchronized_screen_audio_and_actions",
			MediaType: "video/mp4", Bytes: entry.mediaBytes,
		}))
	}
	if !entry.manifest.Complete {
		resultErr = errors.Join(resultErr, attempt.RecordFailure(
			"validate raw evidence", errors.New("raw task evidence is not complete"),
		))
	}
	resultErr = errors.Join(resultErr, attempt.CaptureArtifact(candidate.CapturedArtifact{
		Name: "action-trace.json", Kind: "trace", Role: "exact_realtime_actions_and_timeline",
		ContentType: "application/json", Bytes: entry.traceBytes,
	}))
	resultErr = errors.Join(resultErr, attempt.CaptureArtifact(candidate.CapturedArtifact{
		Name: "wire-media.zip", Kind: "wire_media", Role: "exact_realtime_input_pcm_and_jpegs",
		ContentType: "application/zip", Bytes: entry.wireBytes,
	}))
	return resultErr
}

func dynaCUAttemptTranscript(entry dynaCUEvidenceEntry, outcome bench.TaskOutcome) bench.Transcript {
	transcript := bench.Transcript{Failure: outcome.Error}
	if entry.trace == nil {
		return transcript
	}
	transcript.PlaybackMS = float64(entry.trace.DurationUS) / 1_000
	for _, heard := range entry.trace.Transcripts {
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: float64(heard.AtUS) / 1_000, Kind: bench.MomentTranscript,
			Text: heard.Text, Source: "dynacu-page-audio", Observer: "pinned-realtime-baseline",
		})
	}
	for _, action := range entry.trace.Actions {
		arguments := strings.TrimSpace(string(action.Arguments))
		kind := bench.MomentToolCall
		if action.Error {
			kind = bench.MomentError
		}
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: float64(action.AtUS) / 1_000, Kind: kind, Name: action.Name,
			Arguments: arguments, Source: "dynacu-realtime-tool-call",
		})
	}
	sort.SliceStable(transcript.Moments, func(left, right int) bool {
		return transcript.Moments[left].AtMS < transcript.Moments[right].AtMS
	})
	return transcript
}

// listDynaCUEvidence snapshots only bounded task preambles. Large MP4 and
// exact-wire archives are reopened and imported one task at a time so a full
// 150-task population never scales resident memory with retained media size.
func listDynaCUEvidence(
	ctx context.Context, directory string,
) (dynaCUEvidenceInventory, error) {
	if ctx == nil {
		return dynaCUEvidenceInventory{}, errors.New("list DynaCU evidence: nil context")
	}
	if err := ctx.Err(); err != nil {
		return dynaCUEvidenceInventory{}, err
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return dynaCUEvidenceInventory{}, errors.New("DynaCU raw evidence root was not created as a stable directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return dynaCUEvidenceInventory{}, errors.New("open DynaCU raw evidence root")
	}
	defer root.Close()
	opened, openErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(directory)
	if openErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(visible, opened) || !os.SameFile(opened, afterOpen) {
		return dynaCUEvidenceInventory{}, errors.New("DynaCU raw evidence root changed while opening")
	}
	handle, err := root.Open(".")
	if err != nil {
		return dynaCUEvidenceInventory{}, errors.New("list DynaCU raw evidence root")
	}
	children, readErr := handle.ReadDir(maximumDynaCUAttempts + 1)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil || len(children) == 0 || len(children) > maximumDynaCUAttempts {
		return dynaCUEvidenceInventory{}, errors.New("DynaCU raw evidence population is empty, oversized, or unreadable")
	}
	sort.Slice(children, func(left, right int) bool { return children[left].Name() < children[right].Name() })
	inventory := dynaCUEvidenceInventory{identity: opened, entries: make([]dynaCUEvidenceEntry, 0, len(children))}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return inventory, err
		}
		if !child.IsDir() || len(child.Name()) != sha256.Size*2 {
			return inventory, errors.New("DynaCU raw evidence has an unexpected root entry")
		}
		if _, err := hex.DecodeString(child.Name()); err != nil {
			return inventory, errors.New("DynaCU raw evidence directory name is invalid")
		}
		attemptBytes, err := readDynaCURawFile(
			root, filepath.ToSlash(filepath.Join(child.Name(), "attempt.json")), 64<<10,
		)
		if err != nil {
			return inventory, errors.New("DynaCU raw attempt preamble is unreadable")
		}
		entry := dynaCUEvidenceEntry{directory: child.Name()}
		if err := decodeDynaCURawJSON(attemptBytes, &entry.attempt, 64<<10); err != nil ||
			entry.attempt.Format != dynaCUEvidenceFormat ||
			entry.attempt.FormatVersion != dynaCUEvidenceFormatVersion ||
			entry.attempt.TaskID == "" || strings.TrimSpace(entry.attempt.TaskID) != entry.attempt.TaskID ||
			entry.attempt.Category == "" || entry.attempt.Difficulty == "" ||
			entry.attempt.Terminal != "active" {
			return inventory, errors.New("DynaCU raw attempt preamble is invalid")
		}
		want := sha256.Sum256([]byte(entry.attempt.TaskID))
		if child.Name() != hex.EncodeToString(want[:]) {
			return inventory, errors.New("DynaCU raw attempt directory differs from its task identity")
		}
		inventory.entries = append(inventory.entries, entry)
	}
	if err := verifyDynaCUEvidenceRoot(directory, opened); err != nil {
		return inventory, err
	}
	return inventory, nil
}

func readDynaCUEvidenceEntryFromDirectory(
	directory, child string,
) (dynaCUEvidenceEntry, error) {
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return dynaCUEvidenceEntry{}, errors.New("DynaCU raw evidence root is invalid")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return dynaCUEvidenceEntry{}, errors.New("open DynaCU raw evidence root")
	}
	defer root.Close()
	opened, openErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(directory)
	if openErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(visible, opened) || !os.SameFile(opened, afterOpen) {
		return dynaCUEvidenceEntry{}, errors.New("DynaCU raw evidence root changed while opening")
	}
	entry, err := readDynaCUEvidenceEntry(root, child)
	if err != nil {
		return entry, err
	}
	if err := verifyDynaCUEvidenceRoot(directory, opened); err != nil {
		return entry, err
	}
	return entry, nil
}

func verifyDynaCUEvidenceRoot(directory string, expected os.FileInfo) error {
	if expected == nil {
		return errors.New("DynaCU raw evidence root identity is missing")
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		!os.SameFile(expected, visible) {
		return errors.New("DynaCU raw evidence root identity changed")
	}
	return nil
}

func (config Config) recoverDynaCUAttempt(ctx context.Context, child string) error {
	if ctx == nil {
		return errors.New("recover DynaCU evidence: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	visible, err := os.Lstat(config.EvidenceDirectory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return errors.New("recover DynaCU evidence: raw root is invalid")
	}
	root, err := os.OpenRoot(config.EvidenceDirectory)
	if err != nil {
		return errors.New("recover DynaCU evidence: open raw root")
	}
	opened, openErr := root.Stat(".")
	if openErr != nil || !os.SameFile(visible, opened) {
		root.Close()
		return errors.New("recover DynaCU evidence: raw root changed")
	}
	journalPath := filepath.ToSlash(filepath.Join(child, "capture.journal.jsonl"))
	journalBytes, journalErr := readDynaCURawFile(root, journalPath, maximumDynaCUTraceFile)
	if journalErr == nil && journalBytes[len(journalBytes)-1] != '\n' {
		if last := bytes.LastIndexByte(journalBytes, '\n'); last >= 0 {
			journalBytes = journalBytes[:last+1]
		} else {
			journalErr = errors.New("DynaCU partial capture journal has no durable record")
		}
	}
	journal, decodeErr := decodeDynaCUJournal(journalBytes)
	seen := make(map[string]struct{})
	if journalErr == nil && decodeErr == nil {
		for _, event := range journal.Wire {
			if event.Path == "" {
				continue
			}
			if _, duplicate := seen[event.Path]; duplicate {
				decodeErr = errors.New("DynaCU partial capture journal duplicates a media path")
				break
			}
			seen[event.Path] = struct{}{}
			payload, readErr := readDynaCURawFile(
				root, filepath.ToSlash(filepath.Join(child, event.Path)), maximumDynaCUEvidenceFile,
			)
			if readErr != nil || int64(len(payload)) != event.SizeBytes || dynaCUDigest(payload) != event.SHA256 {
				decodeErr = errors.New("DynaCU partial capture media differs from its durable journal")
				break
			}
		}
	}
	closeErr := root.Close()
	if journalErr != nil || decodeErr != nil || closeErr != nil {
		return errors.New("recover DynaCU evidence: partial journal is invalid")
	}
	directory := filepath.Join(config.EvidenceDirectory, filepath.FromSlash(child))
	driverPath := filepath.Join(config.AOIDir, driverName)
	output, err := runWith(ctx, config.AOIDir, config.Python, []string{
		driverPath, "--recover-partial", directory,
	}, []string{"PYTHONUNBUFFERED=1"})
	if err != nil {
		return errors.New("recover DynaCU synchronized evidence")
	}
	want := filepath.Join(directory, "manifest.json")
	if lastLine(output) != want {
		return errors.New("recover DynaCU evidence: recovery output identity differs")
	}
	return nil
}

func readDynaCUEvidence(ctx context.Context, directory string) ([]dynaCUEvidenceEntry, error) {
	if ctx == nil {
		return nil, errors.New("read DynaCU evidence: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	visible, err := os.Lstat(directory)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() {
		return nil, errors.New("DynaCU raw evidence root was not created as a stable directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("open DynaCU raw evidence root")
	}
	defer root.Close()
	opened, openErr := root.Stat(".")
	afterOpen, visibleErr := os.Lstat(directory)
	if openErr != nil || visibleErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(visible, opened) || !os.SameFile(opened, afterOpen) {
		return nil, errors.New("DynaCU raw evidence root changed while opening")
	}
	handle, err := root.Open(".")
	if err != nil {
		return nil, errors.New("list DynaCU raw evidence root")
	}
	children, readErr := handle.ReadDir(maximumDynaCUAttempts + 1)
	closeErr := handle.Close()
	if readErr != nil || closeErr != nil || len(children) == 0 || len(children) > maximumDynaCUAttempts {
		return nil, errors.New("DynaCU raw evidence population is empty, oversized, or unreadable")
	}
	sort.Slice(children, func(left, right int) bool { return children[left].Name() < children[right].Name() })
	entries := make([]dynaCUEvidenceEntry, 0, len(children))
	var resultErr error
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return entries, errors.Join(resultErr, err)
		}
		if !child.IsDir() || len(child.Name()) != sha256.Size*2 {
			return entries, errors.Join(resultErr, errors.New("DynaCU raw evidence has an unexpected root entry"))
		}
		if _, err := hex.DecodeString(child.Name()); err != nil {
			return entries, errors.Join(resultErr, errors.New("DynaCU raw evidence directory name is invalid"))
		}
		entry, entryErr := readDynaCUEvidenceEntry(root, child.Name())
		if entryErr != nil {
			entry.problem = entryErr
			resultErr = errors.Join(resultErr, errors.New("one DynaCU raw attempt is incomplete or invalid"))
		}
		if entry.attempt.TaskID == "" {
			continue
		}
		want := sha256.Sum256([]byte(entry.attempt.TaskID))
		if child.Name() != hex.EncodeToString(want[:]) {
			entry.problem = errors.New("raw attempt directory differs from its task identity")
			resultErr = errors.Join(resultErr, errors.New("one DynaCU raw attempt identity is invalid"))
		}
		entries = append(entries, entry)
	}
	openedAfter, statErr := root.Stat(".")
	afterRead, visibleErr := os.Lstat(directory)
	if statErr != nil || visibleErr != nil || afterRead.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, openedAfter) || !os.SameFile(openedAfter, afterRead) {
		resultErr = errors.Join(resultErr, errors.New("DynaCU raw evidence root changed while reading"))
	}
	return entries, resultErr
}

func readDynaCUEvidenceEntry(root *os.Root, directory string) (dynaCUEvidenceEntry, error) {
	entry := dynaCUEvidenceEntry{directory: directory}
	attemptBytes, err := readDynaCURawFile(root, filepath.ToSlash(filepath.Join(directory, "attempt.json")), 64<<10)
	if err != nil {
		return entry, err
	}
	if err := decodeDynaCURawJSON(attemptBytes, &entry.attempt, 64<<10); err != nil ||
		entry.attempt.Format != dynaCUEvidenceFormat ||
		entry.attempt.FormatVersion != dynaCUEvidenceFormatVersion ||
		entry.attempt.TaskID == "" || strings.TrimSpace(entry.attempt.TaskID) != entry.attempt.TaskID ||
		entry.attempt.Category == "" || entry.attempt.Difficulty == "" ||
		entry.attempt.Terminal != "active" {
		return entry, errors.New("DynaCU raw attempt preamble is invalid")
	}
	manifestPath := filepath.ToSlash(filepath.Join(directory, "manifest.json"))
	manifestBytes, err := readDynaCURawFile(root, manifestPath, 1<<20)
	if err != nil {
		return entry, errDynaCURawManifestMissing
	}
	var manifest dynaCURawManifest
	if err := decodeDynaCURawJSON(manifestBytes, &manifest, 1<<20); err != nil {
		return entry, err
	}
	entry.manifest = &manifest
	if manifest.Format != dynaCUEvidenceFormat || manifest.FormatVersion != dynaCUEvidenceFormatVersion ||
		manifest.TaskID != entry.attempt.TaskID || manifest.Category != entry.attempt.Category ||
		manifest.Difficulty != entry.attempt.Difficulty || manifest.Terminal != "completed" ||
		manifest.DurationUS <= 0 || manifest.DurationUS > 30*60*1_000_000 ||
		manifest.TimestampBasis != dynaCUTimestampBasis ||
		manifest.AudioChunkCount < 0 || manifest.FrameCount < 0 || manifest.ActionCount < 0 ||
		len(manifest.CaptureErrors) > 64 {
		return entry, errors.New("DynaCU raw evidence manifest identity is invalid")
	}
	for index, code := range manifest.CaptureErrors {
		if code == "" || strings.TrimSpace(code) != code ||
			(index > 0 && code <= manifest.CaptureErrors[index-1]) {
			return entry, errors.New("DynaCU raw capture-error set is invalid")
		}
	}
	wantKeys := []string{
		"capture_journal", "deterministic_result", "timeline_audio", "trace", "wire_archive",
	}
	_, hasReviewMedia := manifest.Files["review_media"]
	if manifest.Complete && !hasReviewMedia {
		return entry, errors.New("DynaCU raw evidence file set is incomplete")
	}
	wantCount := len(wantKeys)
	if hasReviewMedia {
		wantCount++
	}
	if len(manifest.Files) != wantCount {
		return entry, errors.New("DynaCU raw evidence file set is invalid")
	}
	for _, key := range wantKeys {
		if _, found := manifest.Files[key]; !found {
			return entry, errors.New("DynaCU raw evidence file set is incomplete")
		}
	}
	journalBytes, err := readDynaCURawIdentity(
		root, directory, manifest.Files["capture_journal"],
		[]string{"capture.journal.jsonl", "recovered-capture.journal.jsonl"}, maximumDynaCUTraceFile,
	)
	if err != nil {
		return entry, err
	}
	journal, err := decodeDynaCUJournal(journalBytes)
	if err != nil {
		return entry, err
	}
	audioCount, frameCount := 0, 0
	for _, event := range journal.Wire {
		switch event.Kind {
		case "input_audio_pcm16_24000_mono":
			audioCount++
		case "input_image_jpeg":
			frameCount++
		}
	}
	if audioCount != manifest.AudioChunkCount || frameCount != manifest.FrameCount ||
		len(journal.Actions) != manifest.ActionCount {
		return entry, errors.New("DynaCU capture journal population differs from its manifest")
	}
	deterministicBytes, err := readDynaCURawIdentity(
		root, directory, manifest.Files["deterministic_result"],
		[]string{"deterministic-result.json", "recovered-deterministic-result.json"}, 8<<20,
	)
	if err != nil {
		return entry, err
	}
	traceBytes, err := readDynaCURawIdentity(
		root, directory, manifest.Files["trace"],
		[]string{"trace.json", "recovered-trace.json"}, maximumDynaCUTraceFile,
	)
	if err != nil {
		return entry, err
	}
	var trace dynaCURawTrace
	if err := decodeDynaCURawJSON(traceBytes, &trace, int(maximumDynaCUTraceFile)); err != nil ||
		trace.Format != dynaCUEvidenceFormat+".action-trace" ||
		trace.FormatVersion != dynaCUEvidenceFormatVersion ||
		trace.TaskID != manifest.TaskID || trace.Category != manifest.Category ||
		trace.Difficulty != manifest.Difficulty || trace.DurationUS != manifest.DurationUS ||
		trace.TimestampBasis != dynaCUTimestampBasis ||
		trace.ProcessInterrupted != manifest.ProcessInterrupted ||
		!reflect.DeepEqual(trace.CaptureErrors, manifest.CaptureErrors) ||
		len(trace.Actions) != manifest.ActionCount ||
		!reflect.DeepEqual(trace.WireEvents, journal.Wire) ||
		!reflect.DeepEqual(trace.Actions, journal.Actions) ||
		!reflect.DeepEqual(trace.Transcripts, journal.Transcripts) ||
		!bytes.Equal(trace.DeterministicResult, bytes.TrimSpace(deterministicBytes)) {
		return entry, errors.New("DynaCU raw action trace differs from its manifest")
	}
	entry.trace, entry.traceBytes = &trace, traceBytes
	wireBytes, err := readDynaCURawIdentity(
		root, directory, manifest.Files["wire_archive"],
		[]string{"wire.zip", "recovered-wire.zip"}, 64<<20,
	)
	if err != nil || verifyDynaCUWireArchive(wireBytes, attemptBytes, trace.WireEvents) != nil {
		return entry, errors.New("DynaCU exact wire archive is invalid")
	}
	entry.wireBytes = wireBytes
	if _, err := readDynaCURawIdentity(
		root, directory, manifest.Files["timeline_audio"],
		[]string{"timeline.wav", "recovered-timeline.wav"}, maximumDynaCUEvidenceFile,
	); err != nil {
		return entry, err
	}
	if hasReviewMedia {
		media := manifest.Files["review_media"]
		if media.Kind != "video" || media.MediaType != "video/mp4" ||
			media.Role != "synchronized_screen_audio_and_actions" || manifest.FrameCount <= 0 {
			return entry, errors.New("DynaCU raw evidence has invalid review media")
		}
		entry.mediaBytes, err = readDynaCURawIdentity(
			root, directory, media,
			[]string{"review.mp4", "recovered-review.mp4"}, maximumDynaCUEvidenceFile,
		)
		if err != nil {
			return entry, err
		}
	}
	if manifest.Complete && len(manifest.CaptureErrors) != 0 {
		return entry, errors.New("DynaCU complete raw evidence retains capture failures")
	}
	if !manifest.Complete && len(manifest.CaptureErrors) == 0 {
		return entry, errors.New("DynaCU incomplete raw evidence has no capture failure")
	}
	return entry, nil
}

func readDynaCURawIdentity(
	root *os.Root, directory string, identity dynaCURawFile, names []string, maximum int64,
) ([]byte, error) {
	if !slices.Contains(names, identity.Path) || identity.SizeBytes <= 0 || identity.SizeBytes > maximum ||
		!validDynaCUDigest(identity.SHA256) {
		return nil, errors.New("DynaCU raw file identity is invalid")
	}
	payload, err := readDynaCURawFile(
		root, filepath.ToSlash(filepath.Join(directory, identity.Path)), maximum,
	)
	if err != nil || int64(len(payload)) != identity.SizeBytes || dynaCUDigest(payload) != identity.SHA256 {
		return nil, errors.New("DynaCU raw file differs from its manifest")
	}
	return payload, nil
}

func readDynaCURawFile(root *os.Root, path string, maximum int64) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("DynaCU raw evidence file is invalid")
	}
	handle, err := root.Open(path)
	if err != nil {
		return nil, errors.New("open DynaCU raw evidence file")
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(info, opened) || fileidentity.RequireSingleLink(handle) != nil {
		return nil, errors.New("DynaCU raw evidence file identity changed")
	}
	payload, err := io.ReadAll(io.LimitReader(handle, maximum+1))
	after, statErr := handle.Stat()
	visible, visibleErr := root.Lstat(path)
	if err != nil || len(payload) == 0 || int64(len(payload)) > maximum || statErr != nil ||
		visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, after) || !os.SameFile(after, visible) || int64(len(payload)) != after.Size() {
		return nil, errors.New("DynaCU raw evidence file changed while reading")
	}
	return payload, nil
}

func decodeDynaCUJournal(payload []byte) (dynaCUJournal, error) {
	if len(payload) == 0 || len(payload) > int(maximumDynaCUTraceFile) || payload[len(payload)-1] != '\n' {
		return dynaCUJournal{}, errors.New("DynaCU capture journal is empty, oversized, or unsealed")
	}
	result := dynaCUJournal{}
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	ordinal := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return dynaCUJournal{}, errors.New("DynaCU capture journal contains an empty record")
		}
		var item dynaCUJournalRecord
		if err := decodeDynaCURawJSON(line, &item, 8<<20); err != nil {
			return dynaCUJournal{}, errors.New("DynaCU capture journal record is invalid")
		}
		switch item.Kind {
		case "begin":
			if ordinal != 0 || item.AtUS != 0 || item.TimestampBasis != dynaCUTimestampBasis ||
				item.Event != nil || item.Action != nil || item.Transcript != nil {
				return dynaCUJournal{}, errors.New("DynaCU capture journal header is invalid")
			}
		case "wire":
			if ordinal == 0 || item.Event == nil || item.Action != nil || item.Transcript != nil ||
				item.AtUS != 0 || item.TimestampBasis != "" || validateDynaCUWireEvent(*item.Event) != nil {
				return dynaCUJournal{}, errors.New("DynaCU capture journal wire record is invalid")
			}
			result.Wire = append(result.Wire, *item.Event)
		case "action":
			if ordinal == 0 || item.Event != nil || item.Action == nil || item.Transcript != nil ||
				item.AtUS != 0 || item.TimestampBasis != "" || item.Action.AtUS < 0 ||
				item.Action.AtUS > 30*60*1_000_000 || item.Action.Name == "" ||
				strings.TrimSpace(item.Action.Name) != item.Action.Name || len(item.Action.Arguments) == 0 {
				return dynaCUJournal{}, errors.New("DynaCU capture journal action record is invalid")
			}
			result.Actions = append(result.Actions, *item.Action)
		case "transcript":
			if ordinal == 0 || item.Event != nil || item.Action != nil || item.Transcript == nil ||
				item.AtUS != 0 || item.TimestampBasis != "" || item.Transcript.AtUS < 0 ||
				item.Transcript.AtUS > 30*60*1_000_000 || item.Transcript.Text == "" ||
				strings.TrimSpace(item.Transcript.Text) == "" || len(item.Transcript.Text) > 16_384 {
				return dynaCUJournal{}, errors.New("DynaCU capture journal transcript record is invalid")
			}
			result.Transcripts = append(result.Transcripts, *item.Transcript)
		default:
			return dynaCUJournal{}, errors.New("DynaCU capture journal record kind is invalid")
		}
		ordinal++
		if ordinal > 200_000 {
			return dynaCUJournal{}, errors.New("DynaCU capture journal population is oversized")
		}
	}
	if err := scanner.Err(); err != nil || ordinal == 0 {
		return dynaCUJournal{}, errors.New("read DynaCU capture journal")
	}
	return result, nil
}

func validateDynaCUWireEvent(event dynaCUWireEvent) error {
	if event.AtUS < 0 || event.AtUS > 30*60*1_000_000 {
		return errors.New("DynaCU wire timestamp is invalid")
	}
	switch event.Kind {
	case "client_event":
		if event.EventType == "" || strings.TrimSpace(event.EventType) != event.EventType ||
			event.Path != "" || event.SizeBytes != 0 || event.SHA256 != "" || event.SampleCount != 0 {
			return errors.New("DynaCU client event is invalid")
		}
	case "input_audio_pcm16_24000_mono":
		if !strings.HasPrefix(event.Path, "audio/") || event.EventType != "" ||
			event.SizeBytes <= 0 || event.SizeBytes%2 != 0 || event.SampleCount != event.SizeBytes/2 ||
			!validDynaCUDigest(event.SHA256) {
			return errors.New("DynaCU audio wire event is invalid")
		}
	case "input_image_jpeg":
		if !strings.HasPrefix(event.Path, "frames/") || event.EventType != "" ||
			event.SizeBytes <= 0 || event.SampleCount != 0 || !validDynaCUDigest(event.SHA256) {
			return errors.New("DynaCU image wire event is invalid")
		}
	default:
		return errors.New("DynaCU wire event kind is invalid")
	}
	return nil
}

func decodeDynaCURawJSON(payload []byte, destination any, maximum int) error {
	if len(payload) == 0 || len(payload) > maximum {
		return errors.New("DynaCU raw JSON is outside its bound")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximum, MaxDepth: 128, MaxTokens: 2_000_000,
		MaxObjectMembers: 500_000, MaxArrayElements: 1_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: int64(maximum), MaxWorkBytes: int64(maximum) * 4,
	}); err != nil {
		return errors.New("DynaCU raw JSON is not strict or bounded")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("decode DynaCU raw JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("DynaCU raw JSON has trailing data")
	}
	return nil
}

func verifyDynaCUWireArchive(payload, attempt []byte, events []dynaCUWireEvent) error {
	archive, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil || len(archive.File) < 2 || len(archive.File) > 10_000 {
		return errors.New("DynaCU wire archive structure is invalid")
	}
	expected := map[string]struct{}{"attempt.json": {}, "wire-index.json": {}}
	for _, event := range events {
		if err := validateDynaCUWireEvent(event); err != nil {
			return err
		}
		if event.Path == "" {
			continue
		}
		if filepath.IsAbs(event.Path) || filepath.Clean(event.Path) != event.Path ||
			strings.Contains(event.Path, "\\") || strings.HasPrefix(event.Path, "../") ||
			event.SizeBytes <= 0 || event.SizeBytes > maximumDynaCUEvidenceFile ||
			!validDynaCUDigest(event.SHA256) {
			return errors.New("DynaCU wire event identity is invalid")
		}
		expected[event.Path] = struct{}{}
	}
	if len(expected) != len(archive.File) {
		return errors.New("DynaCU wire archive population differs from its index")
	}
	seen := make(map[string]struct{}, len(archive.File))
	var indexBytes []byte
	for _, file := range archive.File {
		if _, found := expected[file.Name]; !found || file.FileInfo().Mode()&os.ModeSymlink != 0 ||
			!file.FileInfo().Mode().IsRegular() || file.Method != zip.Store ||
			file.UncompressedSize64 == 0 || file.UncompressedSize64 > uint64(maximumDynaCUEvidenceFile) {
			return errors.New("DynaCU wire archive member is invalid")
		}
		if _, duplicate := seen[file.Name]; duplicate {
			return errors.New("DynaCU wire archive member is duplicated")
		}
		seen[file.Name] = struct{}{}
		handle, err := file.Open()
		if err != nil {
			return err
		}
		member, readErr := io.ReadAll(io.LimitReader(handle, maximumDynaCUEvidenceFile+1))
		closeErr := handle.Close()
		if readErr != nil || closeErr != nil || uint64(len(member)) != file.UncompressedSize64 {
			return errors.New("read DynaCU wire archive member")
		}
		switch file.Name {
		case "attempt.json":
			if !bytes.Equal(member, attempt) {
				return errors.New("DynaCU wire archive attempt changed")
			}
		case "wire-index.json":
			indexBytes = member
		default:
			var matching *dynaCUWireEvent
			for index := range events {
				if events[index].Path == file.Name {
					matching = &events[index]
					break
				}
			}
			if matching == nil || int64(len(member)) != matching.SizeBytes ||
				dynaCUDigest(member) != matching.SHA256 {
				return errors.New("DynaCU wire archive media differs from its index")
			}
		}
	}
	var index dynaCUWireIndex
	if err := decodeDynaCURawJSON(indexBytes, &index, int(maximumDynaCUTraceFile)); err != nil ||
		index.Format != dynaCUEvidenceFormat+".wire-index" ||
		index.FormatVersion != dynaCUEvidenceFormatVersion ||
		index.TimestampBasis != dynaCUTimestampBasis || !reflect.DeepEqual(index.Events, events) {
		return errors.New("DynaCU wire archive index differs from its action trace")
	}
	return nil
}

func validDynaCUDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func dynaCUDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func matchingDynaCURecord(left, right record) bool {
	return left.TaskID == right.TaskID && left.Category == right.Category &&
		left.Difficulty == right.Difficulty && left.ModelName == right.ModelName &&
		left.ObservationMode == right.ObservationMode && left.Success == right.Success &&
		left.ResultVal == right.ResultVal && left.Steps == right.Steps &&
		left.TotalTime == right.TotalTime && left.Error == right.Error &&
		left.FinalScore == right.FinalScore && left.Heard == right.Heard && left.WallS == right.WallS &&
		bytes.Equal(left.StepLog, right.StepLog)
}

func validateDynaCUTraceResult(trace dynaCURawTrace, expected record) error {
	var retained record
	if err := decodeDynaCURawJSON(trace.DeterministicResult, &retained, 8<<20); err != nil ||
		!matchingDynaCURecord(retained, expected) {
		return fmt.Errorf("DynaCU trace result differs from deterministic row")
	}
	return nil
}

func readDynaCURecordMap(path string) (map[string]record, error) {
	result := make(map[string]record)
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, errors.New("open DynaCU deterministic task records")
	}
	defer handle.Close()
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var resultErr error
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var decoded record
		if err := json.Unmarshal(line, &decoded); err != nil || decoded.TaskID == "" {
			resultErr = errors.Join(resultErr, errors.New("one DynaCU deterministic task record is invalid"))
			continue
		}
		result[decoded.TaskID] = decoded
	}
	if err := scanner.Err(); err != nil {
		resultErr = errors.Join(resultErr, errors.New("read DynaCU deterministic task records"))
	}
	return result, resultErr
}
