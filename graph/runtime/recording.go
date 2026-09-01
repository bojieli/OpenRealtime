package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const (
	traceSessionKeyBytes        = 32
	defaultTraceRetainedBytes   = 2 << 20
	maximumTraceRetainedBytes   = 4 << 20
	defaultTraceCaptureInterval = 10 * time.Millisecond
)

var (
	ErrTraceRecordingDisabled = errors.New("graph trace recording is disabled")
	ErrTraceRecordingNotReady = errors.New("graph trace recording has no complete attested snapshot")
	ErrTraceRecordingClosed   = errors.New("graph trace recording is closed")
)

// TraceRecordingConfig opts one mounted graph into bounded, payload-free live
// trace retention. SessionCorrelationKey must be a fresh cryptographically
// random 32-byte value for this session. The runtime copies and erases its copy
// during shutdown; it never creates an ambient process-wide correlation key.
//
// MaxRetainedBytes bounds the compact JSON size of retained snapshot/event
// records, in addition to Limits. CaptureInterval coalesces media-path signals;
// zero selects a conservative default and a negative duration is invalid.
type TraceRecordingConfig struct {
	Limits                inspect.TraceLimits
	SessionCorrelationKey []byte
	MaxRetainedBytes      int
	CaptureInterval       time.Duration
}

type traceCorrelation struct {
	token     string
	edges     []string
	firstNS   uint64
	lastNS    uint64
	truncated bool
}

type traceRecorder struct {
	graph         ir.Graph
	configuration inspect.ArtifactIdentity
	limits        inspect.TraceLimits
	maxBytes      int
	interval      time.Duration

	mu             sync.Mutex
	key            []byte
	generation     uint64
	active         map[string]traceCorrelation
	snapshots      []inspect.TraceSnapshot
	events         []inspect.TraceEvent
	current        inspect.TraceSnapshot
	initialized    bool
	nextSequence   uint64
	lastAtNS       uint64
	dropped        uint64
	retainedBytes  int
	lastCaptureErr error
	sealed         bool

	wake       chan struct{}
	stop       chan struct{}
	done       chan struct{}
	finished   chan struct{}
	started    atomic.Bool
	stopping   atomic.Bool
	finishOnce sync.Once
	finishErr  error
}

func newTraceRecorder(
	graph ir.Graph,
	configuration *inspect.ArtifactIdentity,
	inspection InspectionConfig,
	config *TraceRecordingConfig,
) (*traceRecorder, error) {
	if config == nil {
		return nil, nil
	}
	if configuration == nil {
		return nil, errors.New("trace recording requires an exact configuration artifact identity")
	}
	if err := configuration.Validate(); err != nil {
		return nil, fmt.Errorf("trace recording configuration identity: %w", err)
	}
	if !canonicalRuntimeTraceName(configuration.ID) ||
		(configuration.Revision != "" && !canonicalRuntimeTraceName(configuration.Revision)) {
		return nil, errors.New("trace recording configuration contains a non-canonical identifier")
	}
	if !canonicalRuntimeTraceDigest(configuration.Digest) {
		return nil, errors.New("trace recording configuration requires a canonical SHA-256 digest")
	}
	if !canonicalRuntimeTraceName(graph.ID) || !canonicalRuntimeTraceDigest(graph.Fingerprint) {
		return nil, errors.New("trace recording requires a canonical graph identity")
	}
	for _, node := range graph.Nodes {
		if !canonicalRuntimeTraceName(node.ID) {
			return nil, fmt.Errorf("trace recording graph contains non-canonical node %q", node.ID)
		}
	}
	for _, edge := range graph.Edges {
		if !canonicalRuntimeTraceName(edge.ID) {
			return nil, fmt.Errorf("trace recording graph contains non-canonical edge %q", edge.ID)
		}
	}
	for _, boundary := range graph.Boundaries {
		if !canonicalRuntimeTraceName(boundary.Name) {
			return nil, fmt.Errorf("trace recording graph contains non-canonical boundary %q", boundary.Name)
		}
	}
	if len(config.SessionCorrelationKey) != traceSessionKeyBytes {
		return nil, fmt.Errorf("trace recording session correlation key has %d bytes, want %d",
			len(config.SessionCorrelationKey), traceSessionKeyBytes)
	}
	limits := config.Limits
	if limits == (inspect.TraceLimits{}) {
		limits = inspect.DefaultTraceLimits()
	}
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("trace recording limits: %w", err)
	}
	queueCount := len(graph.Edges) + len(graph.Boundaries)
	if uint64(len(graph.Nodes)) > uint64(limits.MaxNodes) ||
		uint64(queueCount) > uint64(limits.MaxEdges) {
		return nil, fmt.Errorf("trace recording limits cannot cover graph with %d nodes and %d queues",
			len(graph.Nodes), queueCount)
	}
	if uint64(inspection.MaxFlows) > uint64(limits.MaxFlows) ||
		uint64(inspection.MaxEdgesPerFlow) > uint64(limits.MaxEdgesPerFlow) {
		return nil, errors.New("trace recording flow limits are smaller than live inspection retention")
	}
	maxBytes := config.MaxRetainedBytes
	if maxBytes == 0 {
		maxBytes = defaultTraceRetainedBytes
	}
	if maxBytes < 4096 || maxBytes > maximumTraceRetainedBytes {
		return nil, fmt.Errorf("trace recording max retained bytes must be in 4096..%d",
			maximumTraceRetainedBytes)
	}
	interval := config.CaptureInterval
	if interval == 0 {
		interval = defaultTraceCaptureInterval
	}
	if interval < 0 || interval > time.Minute {
		return nil, errors.New("trace recording capture interval must be in 0..1m")
	}
	configurationCopy := *configuration
	return &traceRecorder{
		graph: graph, configuration: configurationCopy, limits: limits,
		maxBytes: maxBytes, interval: interval,
		key: slices.Clone(config.SessionCorrelationKey), active: make(map[string]traceCorrelation),
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		finished: make(chan struct{}),
	}, nil
}

func canonicalRuntimeTraceDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func canonicalRuntimeTraceName(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024 &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func (recorder *traceRecorder) start(mounted *Mounted) {
	if recorder == nil || mounted == nil || recorder.stopping.Load() {
		return
	}
	if !recorder.started.CompareAndSwap(false, true) {
		return
	}
	go recorder.run(mounted)
	recorder.signal()
}

func (recorder *traceRecorder) run(mounted *Mounted) {
	defer close(recorder.done)
	for {
		select {
		case <-recorder.stop:
			return
		case <-recorder.wake:
		}

		timer := time.NewTimer(recorder.interval)
	wait:
		for {
			select {
			case <-recorder.stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
				break wait
			}
		}
		_ = recorder.capture(mounted)
	}
}

func (recorder *traceRecorder) signal() {
	if recorder == nil || recorder.stopping.Load() {
		return
	}
	select {
	case recorder.wake <- struct{}{}:
	default:
	}
}

func (recorder *traceRecorder) capture(mounted *Mounted) error {
	if recorder == nil || mounted == nil {
		return ErrTraceRecordingDisabled
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.sealed {
		return ErrTraceRecordingClosed
	}

	live := mounted.Live()
	atNS := mounted.now()
	minimumAtNS := recorder.lastAtNS
	for _, node := range live.Nodes {
		minimumAtNS = max(minimumAtNS, node.FirstOutputNS, node.CompletionNS, node.CancellationNS)
	}
	for _, flow := range live.Flows {
		minimumAtNS = max(minimumAtNS, flow.FirstNS, flow.LastNS)
	}
	if atNS < minimumAtNS {
		atNS = minimumAtNS
		recorder.dropped = saturatingAdd(recorder.dropped, 1)
	}
	live.Sequence = 1 // Trace record sequencing is private to this recorder.
	live.Flows = recorder.pseudonymizeFlowsLocked(live.Flows)
	live.TraceDropped = saturatingAdd(live.TraceDropped, recorder.dropped)
	var snapshot inspect.TraceSnapshot
	var err error
	if live.Deployment == nil {
		snapshot, err = inspect.TraceSnapshotFromLive(
			recorder.graph, recorder.configuration, live, atNS,
		)
	} else {
		snapshot, err = inspect.TraceSnapshotFromLiveWithDeployment(
			recorder.graph, recorder.configuration, live.Deployment, live, atNS,
		)
	}
	if err != nil {
		recorder.dropped = saturatingAdd(recorder.dropped, 1)
		recorder.lastCaptureErr = err
		return err
	}
	recorder.lastCaptureErr = nil
	recorder.lastAtNS = atNS

	if !recorder.initialized {
		if err := recorder.replaceWithSnapshotLocked(snapshot, false); err != nil {
			return recorder.failCaptureLocked(err)
		}
		return nil
	}

	prototypes := diffTraceSnapshots(recorder.current, snapshot)
	if len(prototypes) == 0 {
		return nil
	}
	candidate, next, err := recorder.sequenceEventsLocked(prototypes)
	if err != nil || uint64(len(recorder.events)+len(candidate)) > uint64(recorder.limits.MaxEvents) {
		if replaceErr := recorder.replaceWithSnapshotLocked(snapshot, true); replaceErr != nil {
			return recorder.failCaptureLocked(replaceErr)
		}
		return nil
	}
	addedBytes := 0
	for _, event := range candidate {
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return recorder.failCaptureLocked(fmt.Errorf("measure trace event: %w", marshalErr))
		}
		addedBytes += len(payload)
	}
	if addedBytes > recorder.maxBytes-recorder.retainedBytes {
		if err := recorder.replaceWithSnapshotLocked(snapshot, true); err != nil {
			return recorder.failCaptureLocked(err)
		}
		return nil
	}
	recorder.events = append(recorder.events, candidate...)
	recorder.nextSequence = next
	recorder.retainedBytes += addedBytes
	recorder.current = snapshot.Clone()
	return nil
}

func (recorder *traceRecorder) failCaptureLocked(err error) error {
	recorder.dropped = saturatingAdd(recorder.dropped, 1)
	recorder.lastCaptureErr = err
	return err
}

func (recorder *traceRecorder) replaceWithSnapshotLocked(
	snapshot inspect.TraceSnapshot, compact bool,
) error {
	if compact {
		discarded := uint64(len(recorder.snapshots) + len(recorder.events))
		recorder.dropped = saturatingAdd(recorder.dropped, discarded)
		snapshot.Graph.TraceDropped = saturatingAdd(snapshot.Graph.TraceDropped, discarded)
	}
	if recorder.nextSequence == math.MaxUint64 {
		// Every retained record is replaced below, so restarting the private
		// sequence domain cannot collide with an observable retained record.
		recorder.nextSequence = 0
	}
	recorder.nextSequence++
	snapshot.Sequence = recorder.nextSequence
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("measure trace snapshot: %w", err)
	}
	if len(payload) > recorder.maxBytes {
		return fmt.Errorf("trace snapshot requires %d bytes, retention bound is %d",
			len(payload), recorder.maxBytes)
	}
	recorder.snapshots = []inspect.TraceSnapshot{snapshot.Clone()}
	recorder.events = nil
	recorder.current = snapshot.Clone()
	recorder.initialized = true
	recorder.retainedBytes = len(payload)
	return nil
}

func (recorder *traceRecorder) sequenceEventsLocked(
	prototypes []inspect.TraceEvent,
) ([]inspect.TraceEvent, uint64, error) {
	if uint64(len(prototypes)) > uint64(math.MaxUint64-recorder.nextSequence) {
		return nil, recorder.nextSequence, errors.New("trace record sequence exhausted")
	}
	next := recorder.nextSequence
	result := make([]inspect.TraceEvent, len(prototypes))
	for index, event := range prototypes {
		next++
		event.Sequence = next
		result[index] = event.Clone()
	}
	return result, next, nil
}

func (recorder *traceRecorder) pseudonymizeFlowsLocked(
	flows map[string]inspect.FlowLive,
) map[string]inspect.FlowLive {
	keys := make([]string, 0, len(flows))
	for key := range flows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	nextActive := make(map[string]traceCorrelation, len(keys))
	result := make(map[string]inspect.FlowLive, len(keys))
	for _, raw := range keys {
		flow := flows[raw]
		if raw == "" || flow.Correlation != raw {
			recorder.dropped = saturatingAdd(recorder.dropped, 1)
			continue
		}
		base := recorder.hmacLocked("lookup", []byte(raw))
		correlation, found := recorder.active[base]
		if !found || !monotonicRawFlow(correlation, flow) {
			if found {
				recorder.dropped = saturatingAdd(recorder.dropped, 1)
			}
			recorder.generation++
			var generation [8]byte
			binary.BigEndian.PutUint64(generation[:], recorder.generation)
			correlation = traceCorrelation{
				token: recorder.hmacLocked("artifact", []byte(base), generation[:]),
			}
		}
		correlation.edges = slices.Clone(flow.Edges)
		correlation.firstNS = flow.FirstNS
		correlation.lastNS = flow.LastNS
		correlation.truncated = flow.Truncated
		nextActive[base] = correlation
		result[correlation.token] = inspect.FlowLive{
			Correlation: correlation.token, Edges: slices.Clone(flow.Edges),
			FirstNS: flow.FirstNS, LastNS: flow.LastNS, Truncated: flow.Truncated,
		}
	}
	recorder.active = nextActive
	return result
}

func monotonicRawFlow(before traceCorrelation, after inspect.FlowLive) bool {
	return before.firstNS == after.FirstNS && before.lastNS <= after.LastNS &&
		len(before.edges) <= len(after.Edges) &&
		slices.Equal(before.edges, after.Edges[:len(before.edges)]) &&
		(!before.truncated || after.Truncated)
}

func (recorder *traceRecorder) hmacLocked(domain string, values ...[]byte) string {
	digest := hmac.New(sha256.New, recorder.key)
	_, _ = digest.Write([]byte("openrealtime/live-trace/v1\x00" + domain + "\x00"))
	for _, value := range values {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write(value)
	}
	return "hmac-sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func diffTraceSnapshots(before, after inspect.TraceSnapshot) []inspect.TraceEvent {
	result := make([]inspect.TraceEvent, 0)
	if before.Graph != after.Graph {
		value := after.Graph
		result = append(result, inspect.TraceEvent{
			AtNS: after.AtNS, Kind: inspect.TraceEventGraph, Graph: &value,
		})
	}
	beforeNodes := make(map[string]inspect.TraceNodeLive, len(before.Nodes))
	for _, node := range before.Nodes {
		beforeNodes[node.Node] = node
	}
	for _, node := range after.Nodes {
		if previous, found := beforeNodes[node.Node]; found && reflect.DeepEqual(previous, node) {
			continue
		}
		value := cloneRecordedNode(node)
		result = append(result, inspect.TraceEvent{
			AtNS: after.AtNS, Kind: inspect.TraceEventNode, Node: &value,
		})
	}
	beforeEdges := make(map[string]inspect.TraceEdgeLive, len(before.Edges))
	for _, edge := range before.Edges {
		beforeEdges[edge.Edge] = edge
	}
	for _, edge := range after.Edges {
		if previous, found := beforeEdges[edge.Edge]; found && previous == edge {
			continue
		}
		value := edge
		result = append(result, inspect.TraceEvent{
			AtNS: after.AtNS, Kind: inspect.TraceEventEdge, Edge: &value,
		})
	}
	beforeFlows := make(map[string]inspect.TraceFlowLive, len(before.Flows))
	afterFlows := make(map[string]inspect.TraceFlowLive, len(after.Flows))
	for _, flow := range before.Flows {
		beforeFlows[flow.Correlation] = flow
	}
	for _, flow := range after.Flows {
		afterFlows[flow.Correlation] = flow
	}
	removed := make([]string, 0)
	for correlation := range beforeFlows {
		if _, found := afterFlows[correlation]; !found {
			removed = append(removed, correlation)
		}
	}
	sort.Strings(removed)
	for _, correlation := range removed {
		result = append(result, inspect.TraceEvent{
			AtNS: after.AtNS, Kind: inspect.TraceEventFlowRemove, FlowID: correlation,
		})
	}
	correlations := make([]string, 0, len(afterFlows))
	for correlation := range afterFlows {
		correlations = append(correlations, correlation)
	}
	sort.Strings(correlations)
	for _, correlation := range correlations {
		flow := afterFlows[correlation]
		if previous, found := beforeFlows[correlation]; found && reflect.DeepEqual(previous, flow) {
			continue
		}
		value := flow.Clone()
		result = append(result, inspect.TraceEvent{
			AtNS: after.AtNS, Kind: inspect.TraceEventFlow, Flow: &value,
		})
	}
	return result
}

func cloneRecordedNode(node inspect.TraceNodeLive) inspect.TraceNodeLive {
	result := node
	result.Resolution = node.Resolution.Clone()
	return result
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func (recorder *traceRecorder) export(mounted *Mounted) (inspect.LiveTrace, error) {
	if recorder == nil {
		return inspect.LiveTrace{}, ErrTraceRecordingDisabled
	}
	if recorder.stopping.Load() {
		<-recorder.finished
	} else {
		if err := recorder.capture(mounted); err != nil {
			return inspect.LiveTrace{}, fmt.Errorf("checkpoint graph trace: %w", err)
		}
	}
	live := mounted.Live()
	recorder.mu.Lock()
	if !recorder.initialized {
		err := recorder.lastCaptureErr
		recorder.mu.Unlock()
		if err == nil {
			err = ErrTraceRecordingNotReady
		}
		return inspect.LiveTrace{}, err
	}
	if recorder.lastCaptureErr != nil {
		err := recorder.lastCaptureErr
		recorder.mu.Unlock()
		return inspect.LiveTrace{}, err
	}
	candidate := inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: recorder.graph.FormatVersion, ID: recorder.graph.ID,
			Revision: recorder.graph.Revision, Fingerprint: recorder.graph.Fingerprint,
		},
		Configuration: recorder.configuration, Limits: recorder.limits,
		Snapshots: make([]inspect.TraceSnapshot, len(recorder.snapshots)),
		Events:    make([]inspect.TraceEvent, len(recorder.events)),
	}
	if live.Deployment != nil {
		copy := live.Deployment.Clone()
		candidate.Deployment = &copy
	}
	for index, snapshot := range recorder.snapshots {
		candidate.Snapshots[index] = snapshot.Clone()
	}
	for index, event := range recorder.events {
		candidate.Events[index] = event.Clone()
	}
	graph := recorder.graph
	recorder.mu.Unlock()

	frozen, err := inspect.FreezeLiveTrace(candidate)
	if err != nil {
		return inspect.LiveTrace{}, fmt.Errorf("freeze recorded graph trace: %w", err)
	}
	if _, err := inspect.NewTraceReplayer(graph, frozen); err != nil {
		return inspect.LiveTrace{}, fmt.Errorf("verify recorded graph trace: %w", err)
	}
	if _, err := inspect.MarshalLiveTrace(frozen); err != nil {
		return inspect.LiveTrace{}, fmt.Errorf("verify recorded graph trace encoding: %w", err)
	}
	return frozen, nil
}

func (recorder *traceRecorder) finish(mounted *Mounted) error {
	if recorder == nil {
		return nil
	}
	recorder.finishOnce.Do(func() {
		defer close(recorder.finished)
		recorder.stopping.Store(true)
		if recorder.started.Load() {
			close(recorder.stop)
			<-recorder.done
		}
		recorder.finishErr = recorder.capture(mounted)
		recorder.mu.Lock()
		for index := range recorder.key {
			recorder.key[index] = 0
		}
		recorder.key = nil
		recorder.active = nil
		recorder.sealed = true
		recorder.mu.Unlock()
	})
	return recorder.finishErr
}

func (recorder *traceRecorder) discard() {
	if recorder == nil {
		return
	}
	recorder.stopping.Store(true)
	recorder.mu.Lock()
	for index := range recorder.key {
		recorder.key[index] = 0
	}
	recorder.key = nil
	recorder.active = nil
	recorder.snapshots = nil
	recorder.events = nil
	recorder.current = inspect.TraceSnapshot{}
	recorder.sealed = true
	recorder.mu.Unlock()
}

// CheckpointTrace synchronously captures the latest best-effort live view. It
// is normally unnecessary because runtime changes signal the coalescing
// recorder, but it provides an explicit management-plane consistency point.
func (mounted *Mounted) CheckpointTrace() error {
	if mounted == nil || mounted.recorder == nil {
		return ErrTraceRecordingDisabled
	}
	if mounted.recorder.stopping.Load() {
		return ErrTraceRecordingClosed
	}
	return mounted.recorder.capture(mounted)
}

// RecordedTrace returns a recursively independent, fingerprinted artifact and
// verifies deterministic replay against the exact mounted Graph IR.
func (mounted *Mounted) RecordedTrace() (inspect.LiveTrace, error) {
	if mounted == nil || mounted.recorder == nil {
		return inspect.LiveTrace{}, ErrTraceRecordingDisabled
	}
	return mounted.recorder.export(mounted)
}

// MarshalRecordedTrace returns deterministic, newline-terminated strict JSON.
func (mounted *Mounted) MarshalRecordedTrace() ([]byte, error) {
	trace, err := mounted.RecordedTrace()
	if err != nil {
		return nil, err
	}
	return inspect.MarshalLiveTrace(trace)
}
