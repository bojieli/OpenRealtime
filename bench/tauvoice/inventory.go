package tauvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// TaskInventoryVersion is the on-disk contract used by migration census
	// registration. Changing the task identity contract requires a new version;
	// silently reinterpreting an old inventory would change the experiment.
	TaskInventoryVersion = 1

	AirlineTaskCount = 50
	RetailTaskCount  = 114
	TelecomTaskCount = 114

	maxTaskInventoryBytes = 512 << 10
	maxTaskIdentityBytes  = 1024
)

// TaskIdentity is one exact upstream tau2 base-split task. IDs are scoped by
// domain because airline and retail intentionally reuse numeric identifiers.
type TaskIdentity struct {
	Domain string `json:"domain"`
	ID     string `json:"id"`
}

// TaskInventory is the canonical compatibility boundary between the pinned
// tau2 harness and OpenRealtime's preregistered migration census.
type TaskInventory struct {
	Version  int            `json:"version"`
	Revision string         `json:"revision"`
	Tasks    []TaskIdentity `json:"tasks"`
}

// Validate rejects an inventory that does not describe the exact published
// base split. The total alone is insufficient: 278 tasks divided among the
// wrong domains is a different benchmark with a plausible-looking row count.
func (inventory TaskInventory) Validate() error {
	if inventory.Version != TaskInventoryVersion {
		return fmt.Errorf("tau task inventory version must be %d, got %d", TaskInventoryVersion, inventory.Version)
	}
	if inventory.Revision != PinnedRevision {
		return fmt.Errorf("tau task inventory revision must be %s, got %q", PinnedRevision, inventory.Revision)
	}
	if len(inventory.Tasks) != TaskCount {
		return fmt.Errorf("tau task inventory must contain %d tasks, got %d", TaskCount, len(inventory.Tasks))
	}

	counts := map[string]int{}
	seen := make(map[string]struct{}, len(inventory.Tasks))
	for index, task := range inventory.Tasks {
		expected, ok := taskCountForDomain(task.Domain)
		if !ok {
			return fmt.Errorf("tau task inventory task %d has unknown domain %q", index, task.Domain)
		}
		if err := validateTaskID(task.ID); err != nil {
			return fmt.Errorf("tau task inventory task %d (%s): %w", index, task.Domain, err)
		}
		identity := task.Domain + "\x00" + task.ID
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("tau task inventory repeats %s/%s", task.Domain, task.ID)
		}
		seen[identity] = struct{}{}
		counts[task.Domain]++
		if counts[task.Domain] > expected {
			return fmt.Errorf("tau task inventory has more than %d %s tasks", expected, task.Domain)
		}
	}
	for _, domain := range []string{"airline", "retail", "telecom"} {
		expected, _ := taskCountForDomain(domain)
		if counts[domain] != expected {
			return fmt.Errorf("tau task inventory must contain %d %s tasks, got %d", expected, domain, counts[domain])
		}
	}
	return nil
}

func taskCountForDomain(domain string) (int, bool) {
	switch domain {
	case "airline":
		return AirlineTaskCount, true
	case "retail":
		return RetailTaskCount, true
	case "telecom":
		return TelecomTaskCount, true
	default:
		return 0, false
	}
}

func validateTaskID(id string) error {
	if id == "" {
		return errors.New("task ID is empty")
	}
	if len(id) > maxTaskIdentityBytes {
		return fmt.Errorf("task ID exceeds %d bytes", maxTaskIdentityBytes)
	}
	if !utf8.ValidString(id) {
		return errors.New("task ID is not valid UTF-8")
	}
	if strings.TrimSpace(id) != id {
		return errors.New("task ID has leading or trailing whitespace")
	}
	for _, value := range id {
		if unicode.IsControl(value) {
			return errors.New("task ID contains a control character")
		}
	}
	return nil
}

// FreezeTaskInventory clones and deterministically orders a valid inventory.
func FreezeTaskInventory(inventory TaskInventory) (TaskInventory, error) {
	if err := inventory.Validate(); err != nil {
		return TaskInventory{}, err
	}
	frozen := TaskInventory{
		Version:  inventory.Version,
		Revision: inventory.Revision,
		Tasks:    append([]TaskIdentity(nil), inventory.Tasks...),
	}
	sort.Slice(frozen.Tasks, func(left, right int) bool {
		if frozen.Tasks[left].Domain != frozen.Tasks[right].Domain {
			return frozen.Tasks[left].Domain < frozen.Tasks[right].Domain
		}
		return frozen.Tasks[left].ID < frozen.Tasks[right].ID
	})
	return frozen, nil
}

// MarshalTaskInventory emits the only accepted representation of an
// inventory. Stable bytes make the census source digest meaningful.
func MarshalTaskInventory(inventory TaskInventory) ([]byte, error) {
	frozen, err := FreezeTaskInventory(inventory)
	if err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(frozen, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tau task inventory: %w", err)
	}
	return append(payload, '\n'), nil
}

// DecodeTaskInventory accepts strict, bounded, canonical inventory evidence.
func DecodeTaskInventory(reader io.Reader) (TaskInventory, error) {
	if reader == nil {
		return TaskInventory{}, errors.New("decode tau task inventory: reader is nil")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maxTaskInventoryBytes+1))
	if err != nil {
		return TaskInventory{}, fmt.Errorf("read tau task inventory: %w", err)
	}
	if len(payload) == 0 {
		return TaskInventory{}, errors.New("decode tau task inventory: document is empty")
	}
	if len(payload) > maxTaskInventoryBytes {
		return TaskInventory{}, fmt.Errorf("decode tau task inventory: document exceeds %d bytes", maxTaskInventoryBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var inventory TaskInventory
	if err := decoder.Decode(&inventory); err != nil {
		return TaskInventory{}, fmt.Errorf("decode tau task inventory: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return TaskInventory{}, errors.New("decode tau task inventory: trailing JSON value")
		}
		return TaskInventory{}, fmt.Errorf("decode tau task inventory trailing content: %w", err)
	}
	frozen, err := FreezeTaskInventory(inventory)
	if err != nil {
		return TaskInventory{}, err
	}
	canonical, err := MarshalTaskInventory(frozen)
	if err != nil {
		return TaskInventory{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return TaskInventory{}, errors.New("decode tau task inventory: artifact bytes are not canonical JSON")
	}
	return frozen, nil
}

var taskInventoryGitPaths = []string{
	"data/tau2/domains/airline/tasks.json",
	"data/tau2/domains/airline/split_tasks.json",
	"data/tau2/domains/retail/tasks.json",
	"data/tau2/domains/retail/split_tasks.json",
	"data/tau2/domains/telecom/tasks.json",
	"data/tau2/domains/telecom/split_tasks.json",
	"src/tau2/data_model/tasks.py",
	"src/tau2/domains/airline",
	"src/tau2/domains/retail",
	"src/tau2/domains/telecom",
	"src/tau2/registry.py",
	"src/tau2/runner/helpers.py",
	"src/tau2/utils",
}

const taskInventoryPython = `import json
from tau2.runner.helpers import load_tasks

rows = []
for domain in ("airline", "retail", "telecom"):
    for task in load_tasks(domain, "base"):
        rows.append({"domain": domain, "id": str(task.id)})
print(json.dumps(rows, ensure_ascii=False, separators=(",", ":"), sort_keys=True))
`

// LoadTaskInventory asks the pinned upstream harness for its task identities.
// It neither reimplements tau2's split logic nor writes into the checkout.
func LoadTaskInventory(ctx context.Context, tau2Dir, python string) (TaskInventory, error) {
	root, err := verifyTaskInventoryCheckout(ctx, tau2Dir, PinnedRevision)
	if err != nil {
		return TaskInventory{}, err
	}
	tasks, err := exportTaskIdentities(ctx, root, python)
	if err != nil {
		return TaskInventory{}, err
	}
	return FreezeTaskInventory(TaskInventory{
		Version: TaskInventoryVersion, Revision: PinnedRevision, Tasks: tasks,
	})
}

func verifyTaskInventoryCheckout(
	ctx context.Context,
	tau2Dir string,
	expectedRevision string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("load tau task inventory: context is nil")
	}
	if strings.TrimSpace(tau2Dir) == "" {
		return "", errors.New("load tau task inventory: a prepared tau2-bench checkout is required")
	}
	root, err := filepath.Abs(tau2Dir)
	if err != nil {
		return "", fmt.Errorf("resolve tau2-bench checkout: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("stat tau2-bench checkout: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("tau2-bench checkout %s is not a directory", root)
	}

	top, err := inventoryCommand(ctx, root, nil, "git", "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("read tau2-bench repository root: %w", err)
	}
	topInfo, err := os.Stat(top)
	if err != nil || !os.SameFile(info, topInfo) {
		return "", fmt.Errorf("%s is not the tau2-bench repository root", root)
	}
	revision, err := inventoryCommand(ctx, root, nil, "git", "-c", "core.fsmonitor=false", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read tau2-bench revision: %w", err)
	}
	if revision != expectedRevision {
		return "", fmt.Errorf("tau2-bench is at %s but task inventory is pinned to %s", revision, expectedRevision)
	}

	statusArguments := []string{"-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--untracked-files=all", "--"}
	statusArguments = append(statusArguments, taskInventoryGitPaths...)
	status, err := inventoryCommand(ctx, root, nil, "git", statusArguments...)
	if err != nil {
		return "", fmt.Errorf("inspect tau2 task-universe inputs: %w", err)
	}
	if status != "" {
		return "", fmt.Errorf("tau2 task-universe inputs differ from pinned revision %s:\n%s", expectedRevision, status)
	}
	return root, nil
}

func exportTaskIdentities(ctx context.Context, root, python string) ([]TaskIdentity, error) {
	python = strings.TrimSpace(python)
	if python == "" {
		candidate := filepath.Join(root, ".venv", "bin", "python")
		if candidateInfo, statErr := os.Stat(candidate); statErr == nil && !candidateInfo.IsDir() {
			python = candidate
		} else {
			python = "python3"
		}
	}
	environment := replaceEnvironment(os.Environ(), map[string]string{
		"GIT_OPTIONAL_LOCKS":      "0",
		"PYTHONDONTWRITEBYTECODE": "1",
		"PYTHONHASHSEED":          "0",
		"PYTHONNOUSERSITE":        "1",
		"PYTHONPATH":              filepath.Join(root, "src"),
		"TAU2_DATA_DIR":           filepath.Join(root, "data"),
	})
	output, err := inventoryCommand(ctx, root, environment, python, "-s", "-c", taskInventoryPython)
	if err != nil {
		return nil, fmt.Errorf("export tau2 base-split tasks: %w", err)
	}
	if len(output) > maxTaskInventoryBytes {
		return nil, fmt.Errorf("export tau2 base-split tasks: output exceeds %d bytes", maxTaskInventoryBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	var tasks []TaskIdentity
	if err := decoder.Decode(&tasks); err != nil {
		return nil, fmt.Errorf("decode tau2 task export: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("decode tau2 task export: trailing JSON value")
	}
	return tasks, nil
}

func inventoryCommand(
	ctx context.Context,
	directory string,
	environment []string,
	name string,
	arguments ...string,
) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	} else {
		command.Env = replaceEnvironment(os.Environ(), map[string]string{"GIT_OPTIONAL_LOCKS": "0"})
	}
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(exitError.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, truncate(string(exitError.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if _, replace := replacements[name]; !replace {
			result = append(result, entry)
		}
	}
	names := make([]string, 0, len(replacements))
	for name := range replacements {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+replacements[name])
	}
	return result
}
