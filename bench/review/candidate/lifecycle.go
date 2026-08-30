package candidate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	defaultAttemptTimeout  = 2 * time.Minute
	defaultSuiteTimeout    = 5 * time.Minute
	maximumEvidenceTimeout = 30 * time.Minute
)

// LifecycleConfig supplies the exact run identity and an already-constructed
// evidence plug-in. The lifecycle owns no provider, credential, filesystem,
// encoder, server, or presentation client.
type LifecycleConfig struct {
	Context        context.Context
	Plugin         Plugin
	Suite          string
	Cell           bench.Cell
	Provenance     bench.Provenance
	Origin         RunOrigin
	AttemptTimeout time.Duration
	SuiteTimeout   time.Duration
}

// Lifecycle coordinates one candidate cell and prevents duplicate,
// uncommitted, or post-finish attempts from being presented as complete.
type Lifecycle struct {
	mu             sync.Mutex
	ctx            context.Context
	plugin         Plugin
	suite          string
	cell           bench.Cell
	provenance     bench.Provenance
	origin         RunOrigin
	attemptTimeout time.Duration
	suiteTimeout   time.Duration
	started        map[string]struct{}
	cases          map[string]int
	committed      map[string]struct{}
	active         int
	finishing      bool
	finished       bool
}

// NewLifecycle validates the candidate-run contract before any attempt can
// acquire an external session or create a retention directory.
func NewLifecycle(config LifecycleConfig) (*Lifecycle, error) {
	if config.Context == nil {
		return nil, errors.New("candidate evidence lifecycle requires a context")
	}
	if nilPlugin(config.Plugin) {
		return nil, errors.New("candidate evidence lifecycle requires a plug-in")
	}
	if err := config.Context.Err(); err != nil {
		return nil, err
	}
	if err := validateIdentity("candidate suite", config.Suite); err != nil {
		return nil, err
	}
	if err := validateCell(config.Cell); err != nil {
		return nil, err
	}
	if err := config.Origin.Validate(); err != nil {
		return nil, err
	}
	attemptTimeout, err := evidenceTimeout(
		"candidate attempt evidence", config.AttemptTimeout, defaultAttemptTimeout,
	)
	if err != nil {
		return nil, err
	}
	suiteTimeout, err := evidenceTimeout(
		"candidate suite evidence", config.SuiteTimeout, defaultSuiteTimeout,
	)
	if err != nil {
		return nil, err
	}
	return &Lifecycle{
		ctx: config.Context, plugin: config.Plugin, suite: config.Suite,
		cell: cloneCell(config.Cell), provenance: config.Provenance, origin: config.Origin,
		attemptTimeout: attemptTimeout, suiteTimeout: suiteTimeout,
		started: make(map[string]struct{}), cases: make(map[string]int),
		committed: make(map[string]struct{}),
	}, nil
}

func nilPlugin(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func evidenceTimeout(label string, configured, fallback time.Duration) (time.Duration, error) {
	if configured == 0 {
		configured = fallback
	}
	if configured < 0 || configured > maximumEvidenceTimeout {
		return 0, fmt.Errorf("%s timeout is outside the supported range", label)
	}
	return configured, nil
}

// Begin snapshots one suite-owned deterministic context and calls the plug-in
// before playback. Attempt identities are create-once within a lifecycle.
func (lifecycle *Lifecycle) Begin(
	caseID string, trial int, contextValue any,
) (*ActiveAttempt, error) {
	if lifecycle == nil {
		return nil, errors.New("candidate evidence lifecycle is nil")
	}
	specification, err := NewAttempt(
		lifecycle.suite, caseID, trial, lifecycle.cell, lifecycle.provenance,
		lifecycle.origin, contextValue,
	)
	if err != nil {
		return nil, StageError(caseID, "validate attempt", err)
	}
	providerSpecification, err := CloneAttempt(specification)
	if err != nil {
		return nil, StageError(caseID, "snapshot attempt", err)
	}
	identity := specification.ID()
	lifecycle.mu.Lock()
	if lifecycle.finishing || lifecycle.finished {
		lifecycle.mu.Unlock()
		return nil, StageError(caseID, "begin attempt", errors.New("candidate evidence lifecycle is finishing"))
	}
	if _, duplicate := lifecycle.started[identity]; duplicate {
		lifecycle.mu.Unlock()
		return nil, StageError(caseID, "begin attempt", errors.New("candidate attempt identity is duplicated"))
	}
	lifecycle.started[identity] = struct{}{}
	lifecycle.cases[caseID]++
	lifecycle.active++
	lifecycle.mu.Unlock()

	sink, err := lifecycle.plugin.BeginAttempt(lifecycle.ctx, providerSpecification)
	if err != nil || nilPlugin(sink) {
		lifecycle.release(identity, false)
		if err == nil {
			err = errors.New("candidate evidence plug-in returned a nil attempt")
		}
		return nil, StageError(caseID, "begin attempt", err)
	}
	return &ActiveAttempt{
		lifecycle: lifecycle, identity: identity, caseID: caseID,
		specification: specification, sink: sink,
	}, nil
}

func (lifecycle *Lifecycle) release(identity string, committed bool) {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.active > 0 {
		lifecycle.active--
	}
	if committed {
		lifecycle.committed[identity] = struct{}{}
	}
}

// Finish calls the plug-in exactly after all attempts have terminated. It
// still calls FinishSuite for an incomplete result so a review document can
// show missing evidence, but returns an integrity error for every uncommitted
// attempt even if a faulty plug-in tries to report success.
func (lifecycle *Lifecycle) Finish(result bench.Result) error {
	if lifecycle == nil {
		return errors.New("candidate evidence lifecycle is nil")
	}
	lifecycle.mu.Lock()
	if lifecycle.finished || lifecycle.finishing {
		lifecycle.mu.Unlock()
		return StageError("", "finish suite", errors.New("candidate evidence lifecycle already finished"))
	}
	if lifecycle.active != 0 {
		lifecycle.mu.Unlock()
		return StageError("", "finish suite", errors.New("candidate evidence lifecycle has active attempts"))
	}
	lifecycle.finishing = true
	started, committed := len(lifecycle.started), len(lifecycle.committed)
	caseCounts := make(map[string]int, len(lifecycle.cases))
	for caseID, count := range lifecycle.cases {
		caseCounts[caseID] = count
	}
	lifecycle.mu.Unlock()

	finishState := func() {
		lifecycle.mu.Lock()
		lifecycle.finishing = false
		lifecycle.finished = true
		lifecycle.mu.Unlock()
	}
	defer finishState()

	var integrityErr error
	if result.Suite != lifecycle.suite || !reflect.DeepEqual(result.Cell, lifecycle.cell) {
		integrityErr = errors.Join(integrityErr,
			StageError("", "finish suite", errors.New("candidate final result identity differs from its lifecycle")))
	}
	wantProvenance, gotProvenance := lifecycle.provenance, result.Provenance
	wantProvenance.FinishedAt, gotProvenance.FinishedAt = "", ""
	if !reflect.DeepEqual(gotProvenance, wantProvenance) {
		integrityErr = errors.Join(integrityErr,
			StageError("", "finish suite", errors.New("candidate final result provenance differs from its lifecycle")))
	}
	if started != len(result.Tasks) {
		integrityErr = errors.Join(integrityErr, StageError("", "finish suite", fmt.Errorf(
			"candidate lifecycle started %d attempts for %d deterministic rows", started, len(result.Tasks),
		)))
	}
	resultCases := make(map[string]int, len(result.Tasks))
	for _, outcome := range result.Tasks {
		resultCases[outcome.ID]++
	}
	if !reflect.DeepEqual(resultCases, caseCounts) {
		integrityErr = errors.Join(integrityErr,
			StageError("", "finish suite", errors.New("candidate final rows differ from started attempt cases")))
	}
	if committed != started {
		integrityErr = errors.Join(integrityErr, StageError("", "finish suite", fmt.Errorf(
			"candidate lifecycle committed %d of %d started attempts", committed, started,
		)))
	}
	frozen, err := CloneResult(result)
	if err != nil {
		return errors.Join(integrityErr, StageError("", "snapshot final result", err))
	}
	evidenceContext, cancel := context.WithTimeout(
		context.WithoutCancel(lifecycle.ctx), lifecycle.suiteTimeout,
	)
	err = lifecycle.plugin.FinishSuite(evidenceContext, frozen)
	cancel()
	return errors.Join(integrityErr, StageError("", "finish suite", err))
}

// ActiveAttempt serializes callbacks for one plug-in-owned attempt.
type ActiveAttempt struct {
	mu            sync.Mutex
	lifecycle     *Lifecycle
	identity      string
	caseID        string
	specification Attempt
	sink          AttemptEvidence
	failures      []error
	terminal      bool
}

// Specification returns a deep copy for diagnostics and tests.
func (attempt *ActiveAttempt) Specification() (Attempt, error) {
	if attempt == nil {
		return Attempt{}, errors.New("candidate active attempt is nil")
	}
	return CloneAttempt(attempt.specification)
}

// CaptureAudio forwards exact owned session audio and records a stage-typed
// failure for the final lifecycle gate.
func (attempt *ActiveAttempt) CaptureAudio(capture bench.SessionAudioCapture) error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate active attempt is terminal")
	}
	err := attempt.sink.CaptureAudio(capture)
	if err != nil {
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture audio", err))
	}
	return err
}

// CaptureVideo forwards exact owned frames for suites that declare video.
func (attempt *ActiveAttempt) CaptureVideo(capture bench.SessionVideoCapture) error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate active attempt is terminal")
	}
	err := attempt.sink.CaptureVideo(capture)
	if err != nil {
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture video", err))
	}
	return err
}

// Complete snapshots the authoritative outcome/transcript, gives the plug-in
// a bounded cancellation-independent commit window, and aborts on failure.
func (attempt *ActiveAttempt) Complete(
	outcome bench.TaskOutcome, transcript bench.Transcript,
) error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return StageError(attempt.caseID, "complete attempt", errors.New("candidate attempt is already terminal"))
	}
	attempt.terminal = true
	prior := errors.Join(attempt.failures...)
	attempt.mu.Unlock()

	completion, err := CloneCompletion(Completion{
		Attempt: attempt.specification, Outcome: outcome, Transcript: transcript,
	})
	if err == nil {
		evidenceContext, cancel := context.WithTimeout(
			context.WithoutCancel(attempt.lifecycle.ctx), attempt.lifecycle.attemptTimeout,
		)
		err = attempt.sink.Complete(evidenceContext, completion)
		cancel()
	}
	committed := err == nil && prior == nil
	if err != nil {
		err = errors.Join(
			prior,
			StageError(attempt.caseID, "complete attempt", err),
			StageError(attempt.caseID, "abort attempt", attempt.sink.Abort()),
		)
	} else if prior != nil {
		err = prior
	}
	attempt.lifecycle.release(attempt.identity, committed)
	return err
}

// Abort terminates an attempt that cannot produce even an incomplete
// deterministic row. Normal runner paths should call Complete instead.
func (attempt *ActiveAttempt) Abort() error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	attempt.mu.Lock()
	if attempt.terminal {
		attempt.mu.Unlock()
		return StageError(attempt.caseID, "abort attempt", errors.New("candidate attempt is already terminal"))
	}
	attempt.terminal = true
	prior := errors.Join(attempt.failures...)
	attempt.mu.Unlock()
	err := errors.Join(prior, StageError(attempt.caseID, "abort attempt", attempt.sink.Abort()))
	attempt.lifecycle.release(attempt.identity, false)
	return err
}
