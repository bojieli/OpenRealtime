package releasevalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModePlan Mode = "plan"
	ModeRun  Mode = "run"
)

type Scope string

const (
	ScopeLocal Scope = "local"
	ScopeAll   Scope = "all"
)

type Options struct {
	Mode         Mode
	Scope        Scope
	GateIDs      []string
	IncludeOptIn bool
	Root         string
	ArtifactsDir string
	GoBinary     string
	Environment  []string
	Now          func() time.Time
	Progress     io.Writer
}

type Status string

const (
	StatusReady   Status = "ready"
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusBlocked Status = "blocked"
	StatusNotRun  Status = "not_run"
)

type Report struct {
	Version              int          `json:"version"`
	MatrixVersion        int          `json:"matrix_version"`
	MatrixSHA256         string       `json:"matrix_sha256"`
	Mode                 Mode         `json:"mode"`
	Scope                Scope        `json:"scope"`
	StartedAt            time.Time    `json:"started_at"`
	FinishedAt           time.Time    `json:"finished_at"`
	ArtifactsDirectory   string       `json:"artifacts_directory,omitempty"`
	SelectedOutcome      string       `json:"selected_outcome"`
	ReleaseComplete      bool         `json:"release_complete"`
	MissingRequiredGates []string     `json:"missing_required_gates,omitempty"`
	Gates                []GateResult `json:"gates"`
}

type GateResult struct {
	ID              string               `json:"id"`
	Description     string               `json:"description"`
	Availability    Availability         `json:"availability"`
	Required        bool                 `json:"required"`
	Selected        bool                 `json:"selected"`
	Status          Status               `json:"status"`
	Reason          string               `json:"reason,omitempty"`
	Command         []string             `json:"command"`
	Prerequisites   []PrerequisiteResult `json:"prerequisites,omitempty"`
	StartedAt       *time.Time           `json:"started_at,omitempty"`
	FinishedAt      *time.Time           `json:"finished_at,omitempty"`
	DurationMS      int64                `json:"duration_ms,omitempty"`
	ExitCode        *int                 `json:"exit_code,omitempty"`
	TimedOut        bool                 `json:"timed_out,omitempty"`
	ObservedSkips   int                  `json:"observed_skips,omitempty"`
	StdoutLog       string               `json:"stdout_log,omitempty"`
	StderrLog       string               `json:"stderr_log,omitempty"`
	FailedAssertion string               `json:"failed_assertion,omitempty"`
}

type PrerequisiteResult struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
}

const reportVersion = 1

// Execute evaluates prerequisites for every gate and runs the selected set.
// Unselected required gates remain not_run and therefore keep
// ReleaseComplete false. A missing prerequisite is blocked, never passed.
func Execute(ctx context.Context, matrix Matrix, options Options) (Report, error) {
	if err := matrix.Validate(); err != nil {
		return Report{}, err
	}
	if ctx == nil {
		return Report{}, errors.New("release validation context is nil")
	}
	if options.Mode == "" {
		options.Mode = ModeRun
	}
	if options.Mode != ModeRun && options.Mode != ModePlan {
		return Report{}, fmt.Errorf("unknown release validation mode %q", options.Mode)
	}
	if options.Scope == "" {
		options.Scope = ScopeLocal
	}
	if options.Scope != ScopeLocal && options.Scope != ScopeAll {
		return Report{}, fmt.Errorf("unknown release validation scope %q", options.Scope)
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return Report{}, fmt.Errorf("resolve repository root: %w", err)
	}
	if info, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr != nil || info.IsDir() {
		return Report{}, fmt.Errorf("repository root %s does not contain go.mod", root)
	}
	if options.Environment == nil {
		options.Environment = os.Environ()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Progress == nil {
		options.Progress = io.Discard
	}
	resolver, err := newResolver(root, options.GoBinary, options.Environment, options.ArtifactsDir)
	if err != nil {
		return Report{}, err
	}
	selected, err := selectGates(matrix, options)
	if err != nil {
		return Report{}, err
	}
	if options.Mode == ModeRun {
		if err := resolver.createArtifactsDirectory(); err != nil {
			return Report{}, err
		}
	}

	started := options.Now().UTC()
	matrixDigest, err := matrix.Digest()
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Version: reportVersion, MatrixVersion: matrix.Version, MatrixSHA256: matrixDigest,
		Mode: options.Mode, Scope: options.Scope, StartedAt: started,
		ArtifactsDirectory: resolver.artifacts,
	}
	resultsByID := make(map[string]Status, len(matrix.Gates))
	for _, gate := range matrix.Gates {
		_, isSelected := selected[gate.ID]
		result := GateResult{
			ID: gate.ID, Description: gate.Description, Availability: gate.Availability,
			Required: gate.Required, Selected: isSelected, Status: StatusNotRun,
			Command: append([]string(nil), gate.Command...),
		}
		prerequisites, available := resolver.checkPrerequisites(ctx, gate.Prerequisites)
		result.Prerequisites = prerequisites
		if !available {
			result.Status = StatusBlocked
			result.Reason = blockedReason(prerequisites)
		} else if isSelected && options.Mode == ModePlan {
			result.Status = StatusReady
		} else if isSelected {
			result = runGate(ctx, resolver, gate, result, options.Now, options.Progress)
		}
		resultsByID[gate.ID] = result.Status
		report.Gates = append(report.Gates, result)
	}
	report.FinishedAt = options.Now().UTC()
	report.SelectedOutcome = selectedOutcome(report.Gates, options.Mode)
	for _, gate := range matrix.Gates {
		if gate.Required && resultsByID[gate.ID] != StatusPassed {
			report.MissingRequiredGates = append(report.MissingRequiredGates, gate.ID)
		}
	}
	report.ReleaseComplete = len(report.MissingRequiredGates) == 0
	return report, nil
}

func selectGates(matrix Matrix, options Options) (map[string]struct{}, error) {
	selected := make(map[string]struct{})
	if len(options.GateIDs) > 0 {
		known := make(map[string]struct{}, len(matrix.Gates))
		for _, gate := range matrix.Gates {
			known[gate.ID] = struct{}{}
		}
		for _, id := range options.GateIDs {
			if _, exists := known[id]; !exists {
				return nil, fmt.Errorf("unknown release gate %q", id)
			}
			selected[id] = struct{}{}
		}
		return selected, nil
	}
	for _, gate := range matrix.Gates {
		if options.Scope == ScopeAll || (gate.Availability == AvailabilityLocal &&
			(gate.Selection == SelectionDefault || options.IncludeOptIn)) {
			selected[gate.ID] = struct{}{}
		}
	}
	return selected, nil
}

func selectedOutcome(results []GateResult, mode Mode) string {
	selected := 0
	for _, result := range results {
		if !result.Selected {
			continue
		}
		selected++
		switch result.Status {
		case StatusFailed:
			return "failed"
		case StatusBlocked:
			return "blocked"
		}
	}
	if selected == 0 {
		return "empty"
	}
	if mode == ModePlan {
		return "ready"
	}
	return "passed"
}

type resolver struct {
	root        string
	goBinary    string
	gofmt       string
	artifacts   string
	environment []string
	env         map[string]string
}

func newResolver(root, requestedGo string, environment []string, artifacts string) (*resolver, error) {
	env := environmentMap(environment)
	goBinary, err := resolveGoBinary(requestedGo, root, env)
	if err != nil {
		return nil, err
	}
	if artifacts == "" {
		artifacts = filepath.Join(root, ".runtime", "release-validation",
			time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+strconv.Itoa(os.Getpid()))
	} else if !filepath.IsAbs(artifacts) {
		artifacts = filepath.Join(root, artifacts)
	}
	return &resolver{
		root: root, goBinary: goBinary, gofmt: filepath.Join(filepath.Dir(goBinary), "gofmt"),
		artifacts: filepath.Clean(artifacts), environment: append([]string(nil), environment...), env: env,
	}, nil
}

func (resolver *resolver) createArtifactsDirectory() error {
	parent := filepath.Dir(resolver.artifacts)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create release evidence parent: %w", err)
	}
	if err := os.Mkdir(resolver.artifacts, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("release artifacts directory already exists: %s", resolver.artifacts)
		}
		return fmt.Errorf("create release artifacts directory: %w", err)
	}
	return nil
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, assignment := range environment {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			values[name] = value
		}
	}
	return values
}

func resolveGoBinary(requested, root string, env map[string]string) (string, error) {
	candidates := []string{requested, env["OPENREALTIME_GO_BIN"],
		filepath.Join(root, ".runtime", "toolchains", "go1.25.0", "bin", "go"),
		"/usr/local/go/bin/go", "go"}
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		resolved := candidate
		if !strings.ContainsRune(candidate, filepath.Separator) {
			path, err := exec.LookPath(candidate)
			if err != nil {
				continue
			}
			resolved = path
		}
		resolved, _ = filepath.Abs(resolved)
		if _, duplicate := seen[resolved]; duplicate {
			continue
		}
		seen[resolved] = struct{}{}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		output, err := exec.Command(resolved, "env", "GOVERSION").Output()
		if err != nil || !minimumGoVersion(strings.TrimSpace(string(output)), 1, 25) {
			continue
		}
		return resolved, nil
	}
	return "", errors.New("OpenRealtime release validation requires Go 1.25 or newer; set OPENREALTIME_GO_BIN")
}

func minimumGoVersion(version string, wantMajor, wantMinor int) bool {
	version = strings.TrimPrefix(version, "go")
	fields := strings.Split(version, ".")
	if len(fields) < 2 {
		return false
	}
	major, majorErr := strconv.Atoi(fields[0])
	minor, minorErr := strconv.Atoi(fields[1])
	return majorErr == nil && minorErr == nil && (major > wantMajor || major == wantMajor && minor >= wantMinor)
}

func (resolver *resolver) expand(value string) (string, error) {
	replacements := map[string]string{
		"{root}": resolver.root, "{go}": resolver.goBinary,
		"{gofmt}": resolver.gofmt, "{artifacts}": resolver.artifacts,
	}
	for token, replacement := range replacements {
		value = strings.ReplaceAll(value, token, replacement)
	}
	for {
		start := strings.Index(value, "{env:")
		if start < 0 {
			break
		}
		endRelative := strings.IndexByte(value[start:], '}')
		if endRelative < 0 {
			return "", fmt.Errorf("unterminated environment placeholder in %q", value)
		}
		end := start + endRelative
		name := strings.TrimSuffix(strings.TrimPrefix(value[start:end+1], "{env:"), "}")
		replacement, present := resolver.env[name]
		if !present || strings.TrimSpace(replacement) == "" {
			return "", fmt.Errorf("environment variable %s is unavailable", name)
		}
		value = value[:start] + replacement + value[end+1:]
	}
	return value, nil
}

func (resolver *resolver) checkPrerequisites(ctx context.Context, prerequisites []Prerequisite) ([]PrerequisiteResult, bool) {
	results := make([]PrerequisiteResult, 0, len(prerequisites))
	available := true
	for _, prerequisite := range prerequisites {
		result := PrerequisiteResult{Kind: prerequisite.Kind, Description: prerequisite.Description}
		result.Available, result.Reason = resolver.checkPrerequisite(ctx, prerequisite)
		if !result.Available {
			available = false
		}
		results = append(results, result)
	}
	return results, available
}

func (resolver *resolver) checkPrerequisite(ctx context.Context, prerequisite Prerequisite) (bool, string) {
	switch prerequisite.Kind {
	case "command":
		if _, err := exec.LookPath(prerequisite.Value); err != nil {
			return false, fmt.Sprintf("command %s is unavailable", prerequisite.Value)
		}
	case "any_command":
		for _, command := range prerequisite.Alternatives {
			if _, err := exec.LookPath(command); err == nil {
				return true, ""
			}
		}
		return false, "none of the declared commands are available: " + strings.Join(prerequisite.Alternatives, ", ")
	case "any_env":
		for _, name := range prerequisite.Alternatives {
			if strings.TrimSpace(resolver.env[name]) != "" {
				return true, ""
			}
		}
		return false, "none of the declared environment variables are set: " +
			strings.Join(prerequisite.Alternatives, ", ")
	case "browser":
		if configured := strings.TrimSpace(resolver.env["CHROMIUM"]); configured != "" {
			if info, err := os.Stat(configured); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return true, ""
			}
			return false, "CHROMIUM is set but does not name an executable file"
		}
		for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if _, err := exec.LookPath(candidate); err == nil {
				return true, ""
			}
		}
		return false, "CHROMIUM is unset and no Chromium executable is on PATH"
	case "docker_image":
		command := exec.CommandContext(ctx, "docker", "image", "inspect", prerequisite.Value)
		if err := command.Run(); err != nil {
			return false, fmt.Sprintf("preloaded Docker image %s is unavailable (the gate never pulls)", prerequisite.Value)
		}
	case "pkg_config":
		// A cgo build tag is only as available as the C library behind it,
		// and a missing library is a compile error rather than a skip. Ask
		// pkg-config, which is what the cgo directive itself consults, so a
		// host without the library reports blocked instead of failing to build.
		if _, err := exec.LookPath("pkg-config"); err != nil {
			return false, fmt.Sprintf("pkg-config is unavailable, so library %s cannot be located", prerequisite.Value)
		}
		command := exec.CommandContext(ctx, "pkg-config", "--exists", prerequisite.Value)
		if err := command.Run(); err != nil {
			return false, fmt.Sprintf("pkg-config does not know library %s", prerequisite.Value)
		}
	case "python_module":
		parts := strings.SplitN(prerequisite.Value, ":", 2)
		if len(parts) != 2 {
			return false, "python_module must be interpreter:module"
		}
		command := exec.CommandContext(ctx, parts[0], "-c", "import "+parts[1])
		if err := command.Run(); err != nil {
			return false, fmt.Sprintf("Python module %s is unavailable to %s", parts[1], parts[0])
		}
	case "env", "env_url", "env_file", "env_directory", "env_executable":
		value := strings.TrimSpace(resolver.env[prerequisite.Value])
		if value == "" {
			return false, fmt.Sprintf("environment variable %s is unset", prerequisite.Value)
		}
		switch prerequisite.Kind {
		case "env_url":
			parsed, err := url.Parse(value)
			allowedScheme := parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https" ||
				parsed.Scheme == "ws" || parsed.Scheme == "wss")
			if err != nil || !allowedScheme || parsed.Host == "" || parsed.User != nil ||
				parsed.RawQuery != "" || parsed.Fragment != "" {
				return false, fmt.Sprintf("environment variable %s is not a credential-free absolute URL", prerequisite.Value)
			}
		case "env_file":
			if !filepath.IsAbs(value) {
				return false, fmt.Sprintf("file named by %s must be absolute", prerequisite.Value)
			}
			if info, err := os.Stat(value); err != nil || info.IsDir() {
				return false, fmt.Sprintf("file named by %s is unavailable", prerequisite.Value)
			}
		case "env_directory":
			if !filepath.IsAbs(value) {
				return false, fmt.Sprintf("directory named by %s must be absolute", prerequisite.Value)
			}
			if info, err := os.Stat(value); err != nil || !info.IsDir() {
				return false, fmt.Sprintf("directory named by %s is unavailable", prerequisite.Value)
			}
		case "env_executable":
			if !filepath.IsAbs(value) {
				return false, fmt.Sprintf("executable named by %s must be absolute", prerequisite.Value)
			}
			if info, err := os.Stat(value); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				return false, fmt.Sprintf("executable named by %s is unavailable", prerequisite.Value)
			}
		}
	case "file", "directory", "executable":
		value, err := resolver.expand(prerequisite.Value)
		if err != nil {
			return false, err.Error()
		}
		info, err := os.Stat(value)
		if err != nil {
			return false, fmt.Sprintf("%s is unavailable", prerequisite.Value)
		}
		if prerequisite.Kind == "directory" && !info.IsDir() {
			return false, fmt.Sprintf("%s is not a directory", prerequisite.Value)
		}
		if prerequisite.Kind == "file" && info.IsDir() {
			return false, fmt.Sprintf("%s is not a file", prerequisite.Value)
		}
		if prerequisite.Kind == "executable" && (info.IsDir() || info.Mode()&0o111 == 0) {
			return false, fmt.Sprintf("%s is not executable", prerequisite.Value)
		}
	case "os":
		if runtime.GOOS != prerequisite.Value {
			return false, fmt.Sprintf("host OS is %s, requires %s", runtime.GOOS, prerequisite.Value)
		}
	default:
		return false, "unknown prerequisite kind"
	}
	return true, ""
}

func blockedReason(results []PrerequisiteResult) string {
	var reasons []string
	for _, result := range results {
		if !result.Available {
			reasons = append(reasons, result.Reason)
		}
	}
	if len(reasons) == 0 {
		return "one or more prerequisites are unavailable"
	}
	return strings.Join(reasons, "; ")
}

func runGate(
	ctx context.Context,
	resolver *resolver,
	gate Gate,
	result GateResult,
	now func() time.Time,
	progress io.Writer,
) GateResult {
	started := now().UTC()
	result.StartedAt = &started
	fmt.Fprintf(progress, "[%s] running %s\n", gate.ID, gate.Description)
	arguments := make([]string, len(gate.Command))
	for index, template := range gate.Command {
		value, err := resolver.expand(template)
		if err != nil {
			result.Status, result.Reason = StatusBlocked, err.Error()
			return finishGate(result, now)
		}
		arguments[index] = value
	}
	workingDirectory, err := resolver.expand(gate.WorkingDir)
	if err != nil {
		result.Status, result.Reason = StatusBlocked, err.Error()
		return finishGate(result, now)
	}
	if !filepath.IsAbs(workingDirectory) {
		workingDirectory = filepath.Join(resolver.root, workingDirectory)
	}
	stdoutPath := filepath.Join(resolver.artifacts, gate.ID+".stdout.log")
	stderrPath := filepath.Join(resolver.artifacts, gate.ID+".stderr.log")
	stdout, err := os.OpenFile(stdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		result.Status, result.Reason = StatusFailed, fmt.Sprintf("create stdout log: %v", err)
		return finishGate(result, now)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = stdout.Close()
		result.Status, result.Reason = StatusFailed, fmt.Sprintf("create stderr log: %v", err)
		return finishGate(result, now)
	}
	result.StdoutLog, result.StderrLog = stdoutPath, stderrPath

	timeout, _ := time.ParseDuration(gate.Timeout)
	gateContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(gateContext, arguments[0], arguments[1:]...)
	configureCommandCancellation(command)
	command.Dir = workingDirectory
	command.Env = append([]string(nil), resolver.environment...)
	command.Env = setEnvironment(command.Env, "OPENREALTIME_GO_BIN", resolver.goBinary)
	for name, template := range gate.Environment {
		value, expandErr := resolver.expand(template)
		if expandErr != nil {
			_ = stdout.Close()
			_ = stderr.Close()
			result.Status, result.Reason = StatusBlocked, expandErr.Error()
			return finishGate(result, now)
		}
		command.Env = setEnvironment(command.Env, name, value)
	}
	command.Stdout, command.Stderr = stdout, stderr
	runErr := command.Run()
	closeErr := errors.Join(stdout.Close(), stderr.Close())
	if gateContext.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.Status = StatusFailed
		result.Reason = "gate exceeded its checked timeout " + gate.Timeout
	} else if runErr != nil {
		result.Status = StatusFailed
		result.Reason = runErr.Error()
	} else if closeErr != nil {
		result.Status = StatusFailed
		result.Reason = fmt.Sprintf("close gate logs: %v", closeErr)
	} else {
		result.Status = StatusPassed
	}
	if command.ProcessState != nil {
		exitCode := command.ProcessState.ExitCode()
		result.ExitCode = &exitCode
	}
	stdoutSkips, stdoutScanErr := countGoTestSkips(stdoutPath)
	stderrSkips, stderrScanErr := countGoTestSkips(stderrPath)
	result.ObservedSkips = stdoutSkips + stderrSkips
	if result.Status == StatusPassed && (stdoutScanErr != nil || stderrScanErr != nil) {
		result.Status = StatusFailed
		result.Reason = fmt.Sprintf("inspect gate logs for skips: %v", errors.Join(stdoutScanErr, stderrScanErr))
	}
	if result.Status == StatusPassed && gate.SkipPolicy == SkipForbid && result.ObservedSkips > 0 {
		result.Status = StatusFailed
		result.Reason = fmt.Sprintf("gate observed %d skipped Go tests under a forbid policy", result.ObservedSkips)
	}
	if result.Status == StatusPassed {
		if failed := resolver.checkAssertions(gate.Assertions, stdoutPath, stderrPath); failed != "" {
			result.Status = StatusFailed
			result.Reason = "release postcondition failed"
			result.FailedAssertion = failed
		}
	}
	result = finishGate(result, now)
	fmt.Fprintf(progress, "[%s] %s\n", gate.ID, result.Status)
	return result
}

func finishGate(result GateResult, now func() time.Time) GateResult {
	finished := now().UTC()
	result.FinishedAt = &finished
	if result.StartedAt != nil {
		result.DurationMS = finished.Sub(*result.StartedAt).Milliseconds()
	}
	return result
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	filtered := environment[:0]
	for _, assignment := range environment {
		if !strings.HasPrefix(assignment, prefix) {
			filtered = append(filtered, assignment)
		}
	}
	return append(filtered, prefix+value)
}

var goTestSkipPattern = regexp.MustCompile(
	`(?m)^[ \t]*--- SKIP:|"Action"[ \t]*:[ \t]*"skip"`,
)

func countGoTestSkips(path string) (int, error) {
	payload, err := readBounded(path, 64<<20)
	if err != nil {
		return 0, err
	}
	return len(goTestSkipPattern.FindAll(payload, -1)), nil
}

func (resolver *resolver) checkAssertions(assertions []Assertion, stdoutPath, stderrPath string) string {
	for _, assertion := range assertions {
		switch assertion.Kind {
		case "stdout_regex", "stderr_regex":
			path := stdoutPath
			if assertion.Kind == "stderr_regex" {
				path = stderrPath
			}
			payload, err := readBounded(path, 64<<20)
			if err != nil {
				return fmt.Sprintf("%s %q: %v", assertion.Kind, assertion.Value, err)
			}
			if !regexp.MustCompile(assertion.Value).Match(payload) {
				return fmt.Sprintf("%s %q did not match", assertion.Kind, assertion.Value)
			}
		case "file_nonempty":
			path, err := resolver.expand(assertion.Value)
			if err != nil {
				return fmt.Sprintf("file_nonempty %q: %v", assertion.Value, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || info.Size() == 0 {
				return fmt.Sprintf("file_nonempty %q was not a non-empty file", assertion.Value)
			}
		}
	}
	return ""
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.LimitReader(file, maximum+1)
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("log exceeds %d bytes", maximum)
	}
	return payload, nil
}

// MarshalReport emits deterministic, indented JSON. Gate and prerequisite
// order is matrix order; maps never enter the evidence shape.
func MarshalReport(report Report) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return nil, fmt.Errorf("encode release report: %w", err)
	}
	return buffer.Bytes(), nil
}

func RequiredGateIDs(matrix Matrix) []string {
	var ids []string
	for _, gate := range matrix.Gates {
		if gate.Required {
			ids = append(ids, gate.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
