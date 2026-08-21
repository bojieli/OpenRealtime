// Package dynacu runs DynaCU-Bench against an OpenRealtime endpoint.
//
// The benchmark stays where it belongs. The AOI repository owns the 150 task
// pages, the browser environment that serves them, the audio injected into
// them, and the evaluator that decides whether a task passed - and
// reimplementing any of that here would produce a benchmark that agreed with
// this project rather than with the published one. What OpenRealtime ships is
// a runner: it pins the environment to a revision, points it at a local
// endpoint, and turns what comes back into the same report shape every other
// suite produces.
//
// Pointing it at OpenRealtime needs no bridge. The AOI harness carries a
// GA Realtime baseline that is already provider-agnostic - its websocket base,
// credential, and image support are constructor arguments, because OpenAI and
// xAI both speak that protocol - and OpenRealtime is a strict superset of it,
// so it is a third value for the same argument. Nothing in the benchmark is
// patched and nothing in it knows this project exists, which is what makes the
// result a test of the protocol claim rather than of our own adapter.
//
// The suite is 150 tasks: 100 dynamic ones across ten categories that a
// screenshot-only agent cannot solve, and a static 50 that any agent should,
// which is the control that says whether perception cost anything on pages
// where there was nothing to perceive.
package dynacu

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// PinnedRevision is the AOI commit the suite is measured against.
//
// It is pinned rather than tracked because a benchmark that moves underneath a
// result makes the result unreproducible without anybody noticing. Moving it
// is a deliberate change with its own comparison, not a `git pull`.
const PinnedRevision = "3c9f452aca697ee61de87a4907f0937d6c486a99"

// Upstream is where the environment comes from.
const Upstream = "https://github.com/19PINE-AI/aoi.git"

// TaskCount is how many tasks the published suite declares: 100 dynamic and a
// static 50. A cell that runs fewer is incomplete, and the report says so
// rather than dividing by whatever ran.
const TaskCount = 150

// DynamicTasks and StaticTasks are the two halves, which answer different
// questions and are never averaged: the first is what a screenshot-only agent
// cannot do, and the second is whether perception cost anything where there
// was nothing to perceive.
const (
	DynamicTasks = 100
	StaticTasks  = 50
)

// Categories are the ten dynamic domains plus the static control.
var Categories = []string{
	"A_podcast", "B_meeting", "C_video", "D_carousel", "E_dashboard",
	"F_transient", "G_phone", "H_interview", "I_collab", "J_game", "S_static",
}

// Config configures one cell.
type Config struct {
	// AOIDir is the prepared AOI checkout. scripts/prepare-dynacu.sh creates
	// it at the pinned revision with its dependencies verified.
	AOIDir string
	// Endpoint is the OpenRealtime WebSocket base URL the agent connects to.
	Endpoint string
	// Model names the session the endpoint should open. It reaches the server
	// as the ordinary Realtime model parameter.
	Model string
	// Category restricts the run to one domain. A cell that uses it is never
	// complete.
	Category string
	// Difficulty restricts the run to easy, medium, or hard. Same consequence.
	Difficulty string
	// TaskIDs restricts the run to named tasks, for reproducing one row. Same
	// consequence.
	TaskIDs []string
	// Limit caps the task count, for a smoke run. Same consequence.
	Limit int
	// MaxSteps bounds one task's agent loop. It is part of the measurement:
	// an agent given three steps and one given thirty are being asked
	// different questions.
	MaxSteps int
	// StepInterval is how long the agent observes between actions. The suite's
	// own baseline uses two seconds, which is the interval the published
	// numbers were taken at.
	StepInterval time.Duration
	// WithoutImages withholds screenshots, leaving audio and the element list.
	// It isolates audio perception, which is the axis half these categories
	// are about.
	WithoutImages bool
	// WithoutPageElements withholds the interactive-element list, leaving the
	// screenshot alone. The suite's own baselines are given the list so a
	// failure reflects perception rather than selector guessing; withholding
	// it asks the harder question.
	WithoutPageElements bool
	// Resume continues a run that was interrupted rather than starting again.
	// A 150-task cell is hours, and losing all of it to one crash is how a
	// measurement program stops being run.
	Resume bool
	// Output is where per-task records are written. Empty selects a file under
	// the checkout's results directory.
	Output string
	// Timeout bounds the whole run. Zero selects six hours.
	Timeout time.Duration
	// Cell is the measured configuration this run belongs to.
	Cell bench.Cell
	// Python is the interpreter that has the AOI dependencies installed.
	Python string
	// TokenEnv names the environment variable holding the bearer token the
	// agent presents to the endpoint.
	TokenEnv string
	// Logf receives progress. A 150-task run is hours, and a runner that says
	// nothing until it finishes is indistinguishable from one that has hung.
	Logf func(format string, args ...any)
}

func (config *Config) applyDefaults() {
	if directory := strings.TrimSpace(config.AOIDir); directory != "" {
		if absolute, err := filepath.Abs(directory); err == nil {
			config.AOIDir = absolute
		}
	}
	if config.MaxSteps <= 0 {
		config.MaxSteps = 15
	}
	if config.StepInterval <= 0 {
		config.StepInterval = 2 * time.Second
	}
	if config.Timeout <= 0 {
		config.Timeout = 6 * time.Hour
	}
	if strings.TrimSpace(config.Model) == "" {
		config.Model = "openrealtime"
	}
	if strings.TrimSpace(config.TokenEnv) == "" {
		config.TokenEnv = "OPENREALTIME_TOKEN"
	}
	if strings.TrimSpace(config.Python) == "" {
		// The AOI dependency set is not one a system interpreter happens to
		// have - Playwright and a browser alone are most of it - so the
		// checkout's own environment is preferred where it exists.
		venv := filepath.Join(config.AOIDir, ".venv", "bin", "python")
		if info, err := os.Stat(venv); err == nil && !info.IsDir() {
			config.Python = venv
		} else {
			config.Python = "python3"
		}
	}
	if strings.TrimSpace(config.Output) == "" {
		config.Output = filepath.Join(config.AOIDir, "results", "openrealtime-dynacu.jsonl")
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
}

// Restricted reports whether this run covers less than the declared suite.
//
// A restricted run is reported incomplete however well it scores, because it
// is not the suite: five tasks from one category answer a question the
// published number does not ask.
func (config Config) Restricted() bool {
	return strings.TrimSpace(config.Category) != "" ||
		strings.TrimSpace(config.Difficulty) != "" ||
		len(config.TaskIDs) > 0 || config.Limit > 0
}

// Verify checks that the environment is the one the result will claim.
//
// It runs before anything expensive and fails closed. Every check here is a
// way a run can produce numbers that look fine and mean nothing: a checkout at
// the wrong revision measures a different benchmark, a missing browser turns
// 150 tasks into 150 identical infrastructure errors, and a task registry that
// does not declare 150 tasks is not the suite the report will name.
func (config *Config) Verify(ctx context.Context) error {
	config.applyDefaults()
	if strings.TrimSpace(config.AOIDir) == "" {
		return errors.New("a prepared AOI checkout is required; run scripts/prepare-dynacu.sh")
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return errors.New("an OpenRealtime endpoint is required")
	}
	if info, err := os.Stat(filepath.Join(config.AOIDir, ".git")); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a Git checkout; run scripts/prepare-dynacu.sh", config.AOIDir)
	}
	revision, err := run(ctx, config.AOIDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read the checkout revision: %w", err)
	}
	if revision != PinnedRevision {
		return fmt.Errorf(
			"the checkout is at %s but this runner measures %s; run scripts/prepare-dynacu.sh",
			revision, PinnedRevision)
	}
	if err := config.verifyUnmodified(ctx); err != nil {
		return err
	}
	for _, required := range []string{
		filepath.Join("benchmark_env", "html_tasks"),
		filepath.Join("dynacubench", "tasks_v3.py"),
		filepath.Join("aoi", "realtime_baselines.py"),
	} {
		if _, err := os.Stat(filepath.Join(config.AOIDir, required)); err != nil {
			return fmt.Errorf("the checkout is missing %s: %w", required, err)
		}
	}
	if _, err := exec.LookPath(config.Python); err != nil {
		return fmt.Errorf("interpreter %q is not runnable: %w", config.Python, err)
	}
	if _, err := config.agentToken(); err != nil {
		return err
	}
	if err := config.installDriver(); err != nil {
		return err
	}
	if err := config.verifyImports(ctx); err != nil {
		return err
	}
	return config.verifySelection(ctx)
}

// verifyUnmodified refuses a checkout that is not the benchmark it claims.
//
// A result from a modified benchmark cannot be reproduced, so it is refused
// rather than labelled. Two things are ignored, and both are ours rather than
// the benchmark's: the runner this package installs, and the results
// directory a run writes into.
func (config Config) verifyUnmodified(ctx context.Context) error {
	status, err := run(ctx, config.AOIDir, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read the checkout status: %w", err)
	}
	var modified []string
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		path := fields[len(fields)-1]
		if path == driverName || strings.HasPrefix(path, "results/") {
			continue
		}
		modified = append(modified, path)
	}
	if len(modified) > 0 {
		return fmt.Errorf(
			"the AOI checkout has local modifications (%s); a result from a modified benchmark "+
				"cannot be reproduced, so it is refused rather than labelled",
			strings.Join(modified, ", "))
	}
	return nil
}

// driver is OpenRealtime's runner for a benchmark that lives elsewhere.
//
//go:embed driver.py
var driver []byte

// installDriver writes the runner into the checkout.
//
// It is written rather than committed there, and rewritten on every run, so a
// checkout can never drift from the runner that is measuring with it. The
// checkout is otherwise untouched - Verify refuses one with local
// modifications - and this file is the single exception, which is why the
// working-tree check runs before it.
func (config Config) installDriver() error {
	target := filepath.Join(config.AOIDir, driverName)
	existing, err := os.ReadFile(target)
	if err == nil && string(existing) == string(driver) {
		return nil
	}
	if err := os.WriteFile(target, driver, 0o644); err != nil {
		return fmt.Errorf("install the runner into the checkout: %w", err)
	}
	return nil
}

// verifyImports checks the dependencies a run needs before it spends hours
// discovering them one task at a time.
func (config *Config) verifyImports(ctx context.Context) error {
	const probe = `
import json, shutil, sys
missing = []
for module in ("playwright.sync_api", "websocket", "numpy", "PIL"):
    try:
        __import__(module)
    except Exception as failure:
        missing.append(f"{module}: {failure}")
print(json.dumps({"missing": missing}))
`
	output, err := runWith(ctx, config.AOIDir, config.Python, []string{"-c", probe}, nil)
	if err != nil {
		return fmt.Errorf("probe the checkout's Python environment: %w", err)
	}
	var probed struct {
		Missing []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(lastLine(output)), &probed); err != nil {
		return fmt.Errorf("read the environment probe: %w", err)
	}
	if len(probed.Missing) > 0 {
		return fmt.Errorf(
			"the checkout's Python environment cannot run the suite: %s. "+
				"Install the AOI requirements and `playwright install chromium`",
			strings.Join(probed.Missing, "; "))
	}
	return nil
}

// verifySelection loads the task registry and checks it declares the suite
// this runner names.
func (config *Config) verifySelection(ctx context.Context) error {
	arguments := append(config.driverArguments(), "--count-only")
	output, err := runWith(ctx, config.AOIDir, config.Python, arguments, config.environment())
	if err != nil {
		return fmt.Errorf("load the task registry: %w", err)
	}
	var counted struct {
		Declared int `json:"declared"`
		Selected int `json:"selected"`
	}
	if err := json.Unmarshal([]byte(lastLine(output)), &counted); err != nil {
		return fmt.Errorf("read the task count: %w", err)
	}
	if counted.Declared != TaskCount {
		return fmt.Errorf(
			"the checkout declares %d tasks but this runner measures a %d-task suite",
			counted.Declared, TaskCount)
	}
	if counted.Selected == 0 {
		return errors.New("the selection covers no tasks")
	}
	config.Logf("dynacu: %d of %d tasks selected", counted.Selected, counted.Declared)
	return nil
}

// Run executes the cell and reports what came back.
func Run(ctx context.Context, config Config) (bench.Result, error) {
	if err := config.Verify(ctx); err != nil {
		return bench.Result{}, err
	}
	if !config.Resume {
		if err := os.Remove(config.Output); err != nil && !os.IsNotExist(err) {
			return bench.Result{}, fmt.Errorf("clear the previous run: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(config.Output), 0o755); err != nil {
		return bench.Result{}, err
	}

	timed, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	command := exec.CommandContext(timed, config.Python, config.driverArguments()...)
	command.Dir = config.AOIDir
	command.Env = append(os.Environ(), config.environment()...)
	command.Stdout = progress(config.Logf)
	command.Stderr = progress(config.Logf)
	runErr := command.Run()

	result, readErr := config.report()
	if readErr != nil {
		return bench.Result{}, errors.Join(runErr, readErr)
	}
	// A run that died is reported with whatever it completed rather than
	// discarded: the tasks that ran are evidence, and the cell is incomplete
	// either way, which the summary already says.
	return result, runErr
}

// driverArguments is the command line the driver is invoked with.
func (config Config) driverArguments() []string {
	arguments := []string{
		filepath.Join(config.AOIDir, driverName),
		"--endpoint", config.Endpoint,
		"--model", config.Model,
		"--token-env", config.TokenEnv,
		"--out", config.Output,
		"--max-steps", strconv.Itoa(config.MaxSteps),
		"--step-interval", strconv.FormatFloat(config.StepInterval.Seconds(), 'f', 2, 64),
	}
	if config.Category != "" {
		arguments = append(arguments, "--category", config.Category)
	}
	if config.Difficulty != "" {
		arguments = append(arguments, "--difficulty", config.Difficulty)
	}
	if len(config.TaskIDs) > 0 {
		arguments = append(arguments, "--task-ids", strings.Join(config.TaskIDs, ","))
	}
	if config.Limit > 0 {
		arguments = append(arguments, "--limit", strconv.Itoa(config.Limit))
	}
	if config.WithoutImages {
		arguments = append(arguments, "--no-images")
	}
	if config.WithoutPageElements {
		arguments = append(arguments, "--no-page-elements")
	}
	return arguments
}

func (config Config) environment() []string {
	environment := []string{"PYTHONPATH=" + config.AOIDir, "PYTHONUNBUFFERED=1"}
	if token, err := config.agentToken(); err == nil {
		environment = append(environment, config.TokenEnv+"="+token)
	}
	return environment
}

// loopback reports whether the endpoint is on this machine.
func (config Config) loopback() bool {
	endpoint := config.Endpoint
	for _, scheme := range []string{"ws://", "wss://", "http://", "https://"} {
		endpoint = strings.TrimPrefix(endpoint, scheme)
	}
	host := endpoint
	if index := strings.IndexAny(host, "/:"); index >= 0 {
		host = host[:index]
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// agentToken is the credential the subprocess presents to the endpoint.
//
// The suite's baseline requires one to exist whether or not the endpoint wants
// it, so a local server with authentication disabled would otherwise fail 150
// times on a missing environment variable. Inventing one for a loopback
// endpoint protects nothing and costs nothing; inventing one for a remote
// endpoint would hide a misconfiguration until every task had failed.
func (config Config) agentToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(config.TokenEnv)); token != "" {
		return token, nil
	}
	if config.loopback() {
		return "local-development", nil
	}
	return "", fmt.Errorf(
		"%s is empty and %s is not a local endpoint; the suite's client requires a bearer token",
		config.TokenEnv, config.Endpoint)
}

// record is one task as the suite reported it.
type record struct {
	TaskID     string  `json:"task_id"`
	Category   string  `json:"category"`
	Difficulty string  `json:"difficulty"`
	Success    bool    `json:"success"`
	ResultVal  string  `json:"result_val"`
	Steps      int     `json:"steps_taken"`
	TotalTime  float64 `json:"total_time_s"`
	Error      string  `json:"error"`
	FinalScore float64 `json:"final_score"`
	Heard      string  `json:"heard_audio"`
	WallS      float64 `json:"wall_s"`
}

// Report turns the driver's per-task records into a cell.
//
// It is exported because it is the half of a run that can be checked without a
// browser: whether an interrupted run is reported with what it completed,
// whether a retried task appears once, and whether a task the suite marked
// invalid is kept out of the pass rate.
func Report(config Config) (bench.Result, error) {
	config.applyDefaults()
	return config.report()
}

func (config Config) report() (bench.Result, error) {
	result := bench.Result{
		Suite: "dynacu-bench", Cell: config.Cell, Provenance: bench.Capture(),
		Expected: TaskCount,
	}
	handle, err := os.Open(config.Output)
	if err != nil {
		if os.IsNotExist(err) {
			result.Finish()
			return result, nil
		}
		return bench.Result{}, err
	}
	defer handle.Close()

	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var decoded record
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			return bench.Result{}, fmt.Errorf("read a task record: %w", err)
		}
		if _, duplicate := seen[decoded.TaskID]; duplicate {
			// A resumed run rewrites a task it retried. The last word wins.
			for index := range result.Tasks {
				if result.Tasks[index].ID == decoded.TaskID {
					result.Tasks[index] = decoded.outcome()
					break
				}
			}
			continue
		}
		seen[decoded.TaskID] = struct{}{}
		result.Tasks = append(result.Tasks, decoded.outcome())
	}
	if err := scanner.Err(); err != nil {
		return bench.Result{}, err
	}
	result.Finish()
	if config.Restricted() {
		// However well it scored, it is not the suite.
		result.Summary.Complete = false
		result.Summary.Incompleteness = "the run was restricted to a subset of the suite"
	}
	return result, nil
}

func (decoded record) outcome() bench.TaskOutcome {
	outcome := bench.TaskOutcome{
		ID: decoded.TaskID,
		Metrics: map[string]float64{
			"steps": float64(decoded.Steps), "score": decoded.FinalScore,
			"task_seconds": decoded.TotalTime,
		},
		Notes: map[string]string{
			"category": decoded.Category, "difficulty": decoded.Difficulty,
			"result": decoded.ResultVal,
		},
	}
	if decoded.Heard != "" {
		outcome.Notes["heard"] = decoded.Heard
	}
	// An invalid run is not a failed one. The suite marks a task INVALID when
	// every model call failed, and scoring that zero is how infrastructure
	// trouble becomes a published capability claim.
	if strings.TrimSpace(decoded.Error) != "" {
		outcome.Error = decoded.Error
		return outcome
	}
	outcome.Completed = true
	outcome.Passed = decoded.Success
	return outcome
}

// Breakdown summarises a cell by category, because the dynamic ten and the
// static control answer different questions and one number hides that.
func Breakdown(result bench.Result) map[string]CategorySummary {
	summaries := map[string]CategorySummary{}
	for _, task := range result.Tasks {
		category := task.Notes["category"]
		summary := summaries[category]
		summary.Total++
		switch {
		case !task.Completed:
			summary.Invalid++
		case task.Passed:
			summary.Passed++
			summary.Completed++
		default:
			summary.Completed++
		}
		summaries[category] = summary
	}
	for category, summary := range summaries {
		if summary.Completed > 0 {
			summary.Rate = float64(summary.Passed) / float64(summary.Completed)
		}
		summaries[category] = summary
	}
	return summaries
}

// CategorySummary is one domain's reading.
//
// Invalid is reported separately rather than folded into the rate: a task
// where every model call failed says something about the endpoint and nothing
// about the agent.
type CategorySummary struct {
	Total     int     `json:"total"`
	Invalid   int     `json:"invalid"`
	Completed int     `json:"completed"`
	Passed    int     `json:"passed"`
	Rate      float64 `json:"rate"`
}

const driverName = "openrealtime_dynacu_driver.py"

func run(ctx context.Context, directory, name string, arguments ...string) (string, error) {
	return runWith(ctx, directory, name, arguments, nil)
}

func runWith(ctx context.Context, directory, name string, arguments, environment []string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	if len(environment) > 0 {
		command.Env = append(os.Environ(), environment...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// progress forwards subprocess output line by line.
type progressWriter struct {
	logf      func(string, ...any)
	remainder []byte
}

func progress(logf func(string, ...any)) *progressWriter {
	return &progressWriter{logf: logf}
}

func (writer *progressWriter) Write(payload []byte) (int, error) {
	writer.remainder = append(writer.remainder, payload...)
	for {
		index := strings.IndexByte(string(writer.remainder), '\n')
		if index < 0 {
			return len(payload), nil
		}
		line := strings.TrimSpace(string(writer.remainder[:index]))
		writer.remainder = writer.remainder[index+1:]
		if line != "" {
			writer.logf("%s", line)
		}
	}
}
