// Package tauvoice runs the tau-Voice suite against an OpenRealtime endpoint.
//
// The environment stays where it belongs. tau2-bench owns the domains, the
// databases, the user simulator, and the reward function, and reimplementing
// any of that here would produce a benchmark that agreed with itself rather
// than with the published one. What OpenRealtime ships is a runner: it pins
// the environment to a revision, points it at a local endpoint, and turns what
// comes back into the same report shape every other suite produces.
//
// Pointing it at OpenRealtime needs no bridge. tau2's audio-native path speaks
// the OpenAI Realtime protocol over a configurable base URL, and OpenRealtime
// is a strict superset of that protocol - so the endpoint is a flag, and the
// wire is unmodified in both directions. That is the whole argument for the
// protocol being a superset rather than a dialect, tested against a benchmark
// that has never heard of this project.
//
// The suite is 278 tasks in each of two speech conditions. Control is clean
// speech; regular carries the disfluencies, backchannels, and non-directed
// audio that a deployed system actually receives. The gap between them is the
// measurement - a system that scores well on control and badly on regular has
// been measured on a recording studio.
package tauvoice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// PinnedRevision is the tau2-bench commit the suite is measured against.
//
// It is pinned rather than tracked because a benchmark that moves underneath a
// result makes the result unreproducible without anybody noticing. Moving it
// is a deliberate change with its own comparison, not a `git pull`.
const PinnedRevision = "c3398666e6559e3a063da3fc04b5acf7f941464e"

// TaskCount is how many tasks the published suite declares.
//
// A cell that runs fewer is incomplete, and the report says so rather than
// dividing by whatever ran.
const TaskCount = 278

// Condition is one speech condition.
type Condition string

const (
	// Control is clean synthesised speech: the ceiling, not the operating point.
	Control Condition = "control"
	// Regular carries disfluency, backchannel, and non-directed audio. This is
	// the condition a deployed system is actually in.
	Regular Condition = "regular"
)

// Conditions are every level tau2 accepts, headline conditions first.
//
// Between control and regular sit ablations that hold one group of effects
// constant. They answer which part of realistic speech a system is losing to -
// the audio, the accents, or the behaviour - which a two-level comparison can
// only say it lost to something.
var Conditions = []Condition{
	Control, Regular,
	"control_audio", "control_accents", "control_behavior",
	"control_audio_accents", "control_audio_behavior", "control_accents_behavior",
}

// ParseCondition validates a configured speech condition.
func ParseCondition(value string) (Condition, error) {
	candidate := Condition(strings.ToLower(strings.TrimSpace(value)))
	if candidate == "" {
		return Control, nil
	}
	for _, condition := range Conditions {
		if candidate == condition {
			return candidate, nil
		}
	}
	names := make([]string, len(Conditions))
	for index, condition := range Conditions {
		names[index] = string(condition)
	}
	return "", fmt.Errorf("speech condition must be one of %s, got %q", strings.Join(names, ", "), value)
}

// Domains are the task domains the suite spans.
var Domains = []string{"airline", "retail", "telecom"}

// Config configures one cell.
type Config struct {
	// Tau2Dir is the prepared tau2-bench checkout. scripts/prepare-tau-voice.sh
	// creates it at the pinned revision with the local-endpoint patch applied.
	Tau2Dir string
	// Endpoint is the OpenRealtime WebSocket base URL the agent connects to.
	Endpoint string
	// Model names the session the endpoint should open. It reaches the server
	// as the ordinary Realtime model parameter.
	Model string
	// Domain restricts the run. Empty runs every domain, which is what a
	// reportable cell requires.
	Domain string
	// Condition selects clean or realistic speech.
	Condition Condition
	// Trials repeats each task. The published metric is pass^1, but a single
	// trial cannot separate a flaky system from a bad one.
	Trials int
	// TaskIDs restricts the run to named tasks, for reproducing one row. A cell
	// that uses it is never complete.
	TaskIDs []string
	// Limit caps the task count, for a smoke run. Same consequence.
	Limit int
	// UserModel is the simulator behind the person. It is part of the
	// measurement, not an implementation detail, so it is recorded.
	UserModel string
	// UserModelURL points the simulator at an OpenAI-compatible endpoint.
	// Setting it is what makes a run fully local: the caller, the agent, the
	// recogniser, and the voice all become models on this machine, and the
	// result stops depending on somebody else's API being up, priced, and
	// unchanged.
	UserModelURL string
	// UserModelThinking leaves a reasoning model's thinking mode on. It is off
	// by default because a user simulator that emits its deliberation into the
	// conversation is playing a different part than the benchmark intends.
	UserModelThinking bool
	// HallucinationRetries is tau2's re-roll when it judges the simulator to
	// have hallucinated. The check itself calls a model, so a run that is
	// meant to be local sets it to zero.
	HallucinationRetries int
	// RunPrefix names the tau2 runs this cell produces. tau2 writes them under
	// data/simulations/ in its own checkout, keyed by name, and resumes a run
	// whose name already exists - which is how an interrupted 278-task cell
	// continues rather than restarting.
	RunPrefix string
	// Cadence is the trigger cadence in seconds, tau2's tick duration. It is
	// factor F4 and the two must agree, so the runner passes it through rather
	// than letting each side keep its own default.
	Cadence float64
	// Timeout bounds one simulation.
	Timeout time.Duration
	// Cell is the measured configuration this run belongs to.
	Cell bench.Cell
	// Python is the interpreter that has tau2 installed.
	Python string
	// TokenEnv names the environment variable holding the bearer token the
	// agent presents to the endpoint. tau2's adapter requires one to be set
	// even when the endpoint accepts anything.
	TokenEnv string
	// SynthesisProvider gives the simulated caller a voice. tau2 defaults to a
	// hosted service; the pinned patch adds a local one, which is what makes a
	// measurement run without sending every caller utterance to a third party
	// and without a per-run bill that discourages repeating it.
	SynthesisProvider string
	// SynthesisEndpoint is the local speech endpoint, when one is in use.
	SynthesisEndpoint string
	// SynthesisModel and SynthesisVoice identify the caller's voice. They are
	// part of the measurement - a different voice is a different recognition
	// problem - so they are configured rather than defaulted silently.
	SynthesisModel string
	SynthesisVoice string
	// Logf receives progress. tau2 runs for hours, and a runner that says
	// nothing until it finishes is indistinguishable from one that has hung.
	Logf func(format string, args ...any)
}

func (config *Config) applyDefaults() {
	// Every path derived from the checkout - the interpreter, the results
	// directory - is used with the subprocess's working directory set to the
	// checkout itself, where a relative path would resolve somewhere else
	// entirely.
	if directory := strings.TrimSpace(config.Tau2Dir); directory != "" {
		if absolute, err := filepath.Abs(directory); err == nil {
			config.Tau2Dir = absolute
		}
	}
	if config.Condition == "" {
		config.Condition = Control
	}
	if config.Trials <= 0 {
		config.Trials = 1
	}
	if config.Cadence <= 0 {
		config.Cadence = 0.2
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Minute
	}
	if strings.TrimSpace(config.Python) == "" {
		// tau2 manages its own virtual environment, and its dependency set is
		// not one a system interpreter happens to have - the audio path alone
		// pulls PortAudio bindings. Preferring the checkout's own interpreter
		// is the difference between a run and 278 infrastructure errors.
		venv := filepath.Join(config.Tau2Dir, ".venv", "bin", "python")
		if info, err := os.Stat(venv); err == nil && !info.IsDir() {
			config.Python = venv
		} else {
			config.Python = "python3"
		}
	}
	if strings.TrimSpace(config.UserModel) == "" {
		config.UserModel = "gpt-4.1"
	}
	if strings.TrimSpace(config.RunPrefix) == "" {
		config.RunPrefix = "openrealtime"
	}
	if strings.TrimSpace(config.TokenEnv) == "" {
		config.TokenEnv = "OPENREALTIME_TOKEN"
	}
	if strings.TrimSpace(config.SynthesisProvider) == "" {
		config.SynthesisProvider = "fish_audio"
	}
	if config.SynthesisProvider == "fish_audio" {
		if strings.TrimSpace(config.SynthesisEndpoint) == "" {
			config.SynthesisEndpoint = "http://127.0.0.1:8081/v1/audio/speech"
		}
		if strings.TrimSpace(config.SynthesisModel) == "" {
			config.SynthesisModel = "fishaudio/s2-pro"
		}
		if strings.TrimSpace(config.SynthesisVoice) == "" {
			config.SynthesisVoice = "default"
		}
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
}

// loopback reports whether the endpoint is on this machine.
//
// It decides whether a missing credential is a misconfiguration or simply the
// truth. A local server with authentication disabled has no key, and demanding
// the operator invent one would be friction that protects nothing; a remote
// endpoint with no credential is a run that will fail 278 times.
func (config *Config) loopback() bool {
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
func (config *Config) agentToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(config.TokenEnv)); token != "" {
		return token, nil
	}
	if config.loopback() {
		return "local-development", nil
	}
	return "", fmt.Errorf(
		"%s is empty and %s is not a local endpoint; tau2's adapter requires a bearer token",
		config.TokenEnv, config.Endpoint)
}

// Verify checks that the environment is the one the result will claim.
//
// It runs before anything expensive and fails closed. Every check here is a
// way a run can produce numbers that look fine and mean nothing: a checkout at
// the wrong revision measures a different benchmark, an unapplied patch
// silently sends the agent to a hosted endpoint, and a missing interpreter
// wastes an hour before saying so.
func (config *Config) Verify(ctx context.Context) error {
	config.applyDefaults()
	if strings.TrimSpace(config.Tau2Dir) == "" {
		return errors.New("a prepared tau2-bench checkout is required; run scripts/prepare-tau-voice.sh")
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return errors.New("an OpenRealtime endpoint is required")
	}
	if info, err := os.Stat(filepath.Join(config.Tau2Dir, ".git")); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a Git checkout; run scripts/prepare-tau-voice.sh", config.Tau2Dir)
	}
	revision, err := run(ctx, config.Tau2Dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read tau2-bench revision: %w", err)
	}
	if revision != PinnedRevision {
		return fmt.Errorf(
			"tau2-bench is at %s but the suite is pinned to %s; a result from another revision "+
				"is not a tau-Voice result", revision, PinnedRevision)
	}
	// The patch is what makes the agent connect to a local endpoint. Without
	// it the run would still complete - against somebody else's model.
	if _, err := os.Stat(filepath.Join(
		config.Tau2Dir, "src", "tau2", "voice", "audio_native", "openai", "discrete_time_adapter.py",
	)); err != nil {
		return fmt.Errorf(
			"the tau-Voice local-endpoint patch is not applied to %s; run scripts/prepare-tau-voice.sh",
			config.Tau2Dir)
	}
	if _, err := config.agentToken(); err != nil {
		return err
	}
	// Importing the adapter, not the package. `import tau2` succeeds against a
	// bare interpreter and tells you nothing: the dependencies a voice run
	// needs are pulled by the audio-native path, and the first thing that
	// notices they are missing is otherwise the 278 simulations that all end
	// in an infrastructure error.
	if _, err := run(ctx, config.Tau2Dir, config.Python,
		"-c", "import tau2.voice.audio_native.openai.discrete_time_adapter",
	); err != nil {
		return fmt.Errorf(
			"%s cannot import tau2's audio-native adapter; prepare the environment with "+
				"scripts/prepare-tau-voice.sh (uv sync in %s): %w",
			config.Python, config.Tau2Dir, err)
	}
	return nil
}

// Run executes one cell and returns its report.
//
// It runs each domain separately. tau2 evaluates a run against one domain's
// database, and a single invocation spanning three would either need the
// domains merged - which changes the benchmark - or hide which one a failure
// came from.
func Run(ctx context.Context, config Config) (bench.Result, error) {
	config.applyDefaults()
	if err := config.Verify(ctx); err != nil {
		return bench.Result{}, err
	}
	provenance := bench.Capture()

	domains := Domains
	if strings.TrimSpace(config.Domain) != "" {
		domains = []string{config.Domain}
	}
	result := bench.Result{
		Suite: "tau-voice", Cell: config.Cell, Provenance: provenance, Expected: TaskCount * config.Trials,
	}
	if config.Limit > 0 || len(config.TaskIDs) > 0 || strings.TrimSpace(config.Domain) != "" {
		// A restricted run is not the declared cell, and the report must not be
		// able to claim it is. Expected stays at the declared count so Finish
		// marks it incomplete.
		config.Logf("running a restricted subset: this cell will be reported incomplete")
	}

	for _, domain := range domains {
		config.Logf("tau-Voice: %s domain, %s speech", domain, config.Condition)
		// --save-to names a run rather than giving a path: tau2 writes it
		// under data/simulations/ inside its own checkout. The runner follows
		// that convention instead of fighting it, and records where the
		// artifacts actually landed.
		runName := fmt.Sprintf("%s-%s-%s", config.RunPrefix, domain, config.Condition)
		outcomes, err := config.runDomain(ctx, domain, runName)
		if err != nil {
			// A domain that failed to run is one incomplete row per task it
			// would have covered, not a dropped domain. A cell that quietly
			// covered two domains out of three is the failure this prevents.
			result.Tasks = append(result.Tasks, bench.TaskOutcome{
				ID: domain, Completed: false, Error: err.Error(),
				Notes: map[string]string{"artifacts": config.simulationDir(runName)},
			})
			config.Logf("tau-Voice: %s domain failed: %v", domain, err)
			continue
		}
		// Where the audio, ticks, and transcripts landed. A surprising row is
		// only investigable if the reader can find the recording behind it.
		for index := range outcomes {
			if outcomes[index].Notes == nil {
				outcomes[index].Notes = map[string]string{}
			}
			outcomes[index].Notes["artifacts"] = config.simulationDir(runName)
		}
		result.Tasks = append(result.Tasks, outcomes...)
	}

	result.Provenance = result.Provenance.Complete()
	result.Finish()
	return result, nil
}

// runDomain invokes tau2 for one domain and reads what it wrote.
func (config *Config) runDomain(ctx context.Context, domain, runName string) ([]bench.TaskOutcome, error) {
	arguments := []string{
		"-m", "tau2.cli", "run",
		"--domain", domain,
		"--num-trials", fmt.Sprint(config.Trials),
		"--save-to", runName,
		"--audio-native",
		"--audio-native-provider", "openai",
		"--audio-native-base-url", config.Endpoint,
		"--speech-complexity", string(config.Condition),
		"--tick-duration", fmt.Sprintf("%g", config.Cadence),
		"--user-llm", config.UserModel,
		"--user-llm-args", config.userModelArgs(),
		"--hallucination-retries", fmt.Sprint(max(0, config.HallucinationRetries)),
		"--voice-synthesis-provider", config.SynthesisProvider,
	}
	if config.SynthesisProvider == "fish_audio" {
		arguments = append(arguments,
			"--fish-audio-endpoint", config.SynthesisEndpoint,
			"--fish-audio-model", config.SynthesisModel,
			"--fish-audio-voice", config.SynthesisVoice,
		)
	}
	if strings.TrimSpace(config.Model) != "" {
		arguments = append(arguments, "--audio-native-model", config.Model)
	}
	if config.Limit > 0 {
		arguments = append(arguments, "--num-tasks", fmt.Sprint(config.Limit))
	}
	if len(config.TaskIDs) > 0 {
		arguments = append(arguments, "--task-ids")
		arguments = append(arguments, config.TaskIDs...)
	}
	if config.Timeout > 0 {
		arguments = append(arguments, "--timeout", fmt.Sprint(int(config.Timeout.Seconds())))
	}

	token, err := config.agentToken()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, config.Python, arguments...)
	command.Dir = config.Tau2Dir
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Join(config.Tau2Dir, "src"),
		// tau2's adapter insists on a credential even for an endpoint that
		// accepts anything, so the runner supplies the one the endpoint
		// actually wants rather than leaving it to the operator's shell.
		"OPENAI_REALTIME_API_KEY="+token,
		"OPENAI_API_KEY="+os.Getenv("OPENAI_API_KEY"),
	)
	// tau2 writes its progress and its summary to stdout and its logging to
	// stderr, so both are followed. Forwarding them is the difference between
	// a runner that appears to have hung for two hours and one that is visibly
	// on task 140 of 278.
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture tau2 output: %w", err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start tau2: %w", err)
	}
	// The tail is read to completion before Wait, because Wait closes the pipe
	// out from under a reader that has not finished - which would truncate the
	// traceback in exactly the case where it is needed.
	tail := forward(stdout, config.Logf)()
	waitErr := command.Wait()

	// Results first, exit status second. tau2 runs optional post-processing
	// after it has written its results - a conversation review, a hallucination
	// check - and any of those can fail on a credential that has nothing to do
	// with the benchmark. Discarding hours of completed simulations because an
	// optional step at the end exited non-zero would be losing the measurement
	// to a footnote.
	outcomes, readErr := readOutcomes(config.simulationDir(runName), domain)
	if readErr != nil {
		if waitErr != nil {
			return nil, fmt.Errorf("tau2 run failed: %w: %s", waitErr, tail)
		}
		return nil, readErr
	}
	if waitErr != nil {
		config.Logf("tau2 exited %v after writing results; the simulations below stand, "+
			"but something after them failed:\n%s", waitErr, tail)
	}
	return outcomes, nil
}

// userModelArgs is the JSON tau2 passes through to the simulator's provider.
//
// It carries the endpoint when one is configured, which is how a local model
// stands in for the hosted default, and it turns thinking off unless asked
// otherwise - a caller who narrates their reasoning aloud is not the caller
// the benchmark describes.
func (config *Config) userModelArgs() string {
	arguments := map[string]any{"temperature": 0.0}
	if url := strings.TrimSpace(config.UserModelURL); url != "" {
		arguments["api_base"] = url
		// LiteLLM insists on a credential for an OpenAI-compatible provider
		// even when the endpoint ignores it.
		arguments["api_key"] = "local"
	}
	if !config.UserModelThinking {
		arguments["extra_body"] = map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
		}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return `{"temperature":0.0}`
	}
	return string(encoded)
}

// simulationDir is where tau2 writes a named run.
func (config *Config) simulationDir(runName string) string {
	return filepath.Join(config.Tau2Dir, "data", "simulations", runName)
}

// simulationIndexEntry is the part of tau2's results file this runner reads.
//
// Only the summary is decoded. The full simulation carries the audio, every
// tick, and the whole message history, and a runner that parsed all of it
// would be reimplementing tau2's own analysis with a second opinion about what
// the numbers mean.
type simulationIndexEntry struct {
	ID                string   `json:"id"`
	TaskID            any      `json:"task_id"`
	Trial             int      `json:"trial"`
	Reward            *float64 `json:"reward"`
	TerminationReason string   `json:"termination_reason"`
	Duration          *float64 `json:"duration"`
	AgentCost         *float64 `json:"agent_cost"`
}

type resultsFile struct {
	Index       []simulationIndexEntry `json:"simulation_index"`
	Simulations []simulationIndexEntry `json:"simulations"`
}

// readOutcomes converts one domain's tau2 results into task rows.
func readOutcomes(saveTo, domain string) ([]bench.TaskOutcome, error) {
	path := filepath.Join(saveTo, "results.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		// tau2 writes a single JSON file for some run shapes and a directory
		// for others; try the file before giving up.
		payload, err = os.ReadFile(saveTo + ".json")
		if err != nil {
			return nil, fmt.Errorf("tau2 wrote no readable results under %s: %w", saveTo, err)
		}
	}
	var file resultsFile
	if err := json.Unmarshal(payload, &file); err != nil {
		return nil, fmt.Errorf("decode tau2 results: %w", err)
	}
	entries := file.Index
	if len(entries) == 0 {
		entries = file.Simulations
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("tau2 results under %s contain no simulations", saveTo)
	}

	outcomes := make([]bench.TaskOutcome, 0, len(entries))
	for _, entry := range entries {
		outcome := bench.TaskOutcome{
			ID: fmt.Sprintf("%s/%v/trial-%d", domain, entry.TaskID, entry.Trial),
			// A simulation that produced no reward did not reach evaluation,
			// whatever else it did. Scoring it zero would count a crashed run
			// as a failed task, and those are different numbers.
			Completed: entry.Reward != nil,
			Notes:     map[string]string{"domain": domain},
		}
		if entry.TerminationReason != "" {
			outcome.Notes["termination"] = entry.TerminationReason
		}
		if entry.Reward != nil {
			// tau2's reward is the database check and the communicated
			// information together; the published metric thresholds it at 1.
			outcome.Passed = *entry.Reward >= 1
		} else {
			outcome.Error = "tau2 produced no reward for this simulation"
			if entry.TerminationReason != "" {
				outcome.Error += ": " + entry.TerminationReason
			}
		}
		outcome.Metrics = map[string]float64{}
		if entry.Duration != nil {
			outcome.Metrics["simulation_duration_ms"] = *entry.Duration * 1000
		}
		if entry.AgentCost != nil {
			outcome.Metrics["agent_cost_usd"] = *entry.AgentCost
		}
		if len(outcome.Metrics) == 0 {
			outcome.Metrics = nil
		}
		outcomes = append(outcomes, outcome)
	}
	sort.Slice(outcomes, func(left, right int) bool { return outcomes[left].ID < outcomes[right].ID })
	return outcomes, nil
}

// InteractionMetrics runs tau2's own interaction-metrics computation.
//
// Response and yield latency, response and yield rate, and the three
// selectivity measures come from tau2 rather than from a second implementation
// here. They are what makes a tau-Voice run say something about turn-taking
// rather than only about task success, and computing them independently would
// invite two numbers that disagree with no way to tell which is right.
func InteractionMetrics(ctx context.Context, config Config, runName string) (map[string]float64, error) {
	config.applyDefaults()
	saveTo := config.simulationDir(runName)
	output := filepath.Join(saveTo, "interaction-metrics.json")
	command := exec.CommandContext(ctx, config.Python,
		"-m", "tau2.cli", "submit", "interaction-metrics",
		"--input-paths", saveTo, "--output", output)
	command.Dir = config.Tau2Dir
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(config.Tau2Dir, "src"))
	if combined, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("compute interaction metrics: %w: %s", err, truncate(string(combined)))
	}
	payload, err := os.ReadFile(output)
	if err != nil {
		return nil, fmt.Errorf("read interaction metrics: %w", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode interaction metrics: %w", err)
	}
	return flatten(decoded, ""), nil
}

// flatten turns tau2's nested metric block into flat named numbers, which is
// what a bench report carries.
func flatten(value map[string]any, prefix string) map[string]float64 {
	flat := map[string]float64{}
	for name, entry := range value {
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		switch typed := entry.(type) {
		case float64:
			flat[key] = typed
		case map[string]any:
			for nested, number := range flatten(typed, key) {
				flat[nested] = number
			}
		}
	}
	return flat
}

// forward streams a subprocess's output to the log and keeps the tail.
//
// The tail is what goes into the error. A Python traceback's useful part is
// its last twenty lines, and an error that says "exit status 1" has thrown
// them away.
func forward(reader interface{ Read([]byte) (int, error) }, logf func(string, ...any)) func() string {
	const keep = 20
	lines := make([]string, 0, keep)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			logf("tau2: %s", line)
			if len(lines) == keep {
				lines = lines[1:]
			}
			lines = append(lines, line)
		}
	}()
	return func() string {
		<-done
		return strings.Join(lines, "\n")
	}
}

func run(ctx context.Context, directory, name string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(directory, "src"))
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func truncate(value string) string {
	const limit = 2000
	if len(value) <= limit {
		return value
	}
	return "…" + value[len(value)-limit:]
}

// RunDomainForTest exposes one domain's subprocess run to the package's tests.
//
// The behaviour worth pinning there is what happens when tau2 writes good
// results and then exits non-zero, which needs a real process to produce.
func RunDomainForTest(
	ctx context.Context, config Config, domain, runName string,
) ([]bench.TaskOutcome, error) {
	config.applyDefaults()
	return config.runDomain(ctx, domain, runName)
}

// ReadOutcomesForTest exposes result parsing to the package's tests.
//
// The parsing is where a tau2 result becomes a bench row, and the distinction
// it draws - a simulation that failed against one that never ran - is the one
// worth testing without a two-hour benchmark behind it.
func ReadOutcomesForTest(saveTo, domain string) ([]bench.TaskOutcome, error) {
	return readOutcomes(saveTo, domain)
}
