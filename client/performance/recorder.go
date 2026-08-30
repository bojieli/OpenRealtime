package performance

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
)

var ErrProviderUnavailable = errors.New("performance evidence provider unavailable")

type RejectedError struct {
	Reason RejectionReason
}

func (rejection RejectedError) Error() string {
	return "performance observation rejected: " + string(rejection.Reason)
}

type seriesKey struct {
	source     uint16
	metric     Metric
	dimensions string
}

type seriesState struct {
	Source          uint16
	Metric          Metric
	Family          MetricFamily
	Dimensions      []Dimension
	Coverage        Coverage
	CoverageReason  CoverageReason
	coverageSet     bool
	Attempts        uint64
	SamplingDropped uint64
	Duration        *DurationHistogram
	Counter         *CounterAggregate
	Gauge           *GaugeAggregate
}

type spanState struct {
	provider   *Provider
	generation uint64
	source     uint16
	key        seriesKey
	startNS    uint64
	token      uint64
	completed  bool
}

// Span is an opaque process-local correlation handle. It has no JSON fields,
// exposes no token, and is never retained by a snapshot.
type Span struct {
	state *spanState
}

type Provider struct {
	mu            sync.Mutex
	active        bool
	identity      RuntimeIdentity
	configuration Configuration
	series        map[seriesKey]*seriesState
	pending       map[uint64]*spanState
	nextSpan      uint64
	rejections    map[RejectionReason]uint64
	drops         map[DropReason]uint64
}

// SnapshotReader is the narrow read-only face intended for exporters and
// observability views. It cannot obtain a recorder or mutate collector state.
type SnapshotReader interface {
	Snapshot() (Snapshot, error)
}

// Service is the provider face mounted under ServiceContract. Client runtimes
// inject individual Recorder values into probes and may separately export the
// narrower SnapshotReader face.
type Service interface {
	SnapshotReader
	Recorder(source uint16) (Recorder, error)
}

func newProvider(identity RuntimeIdentity, configuration Configuration) *Provider {
	return &Provider{
		active: true, identity: identity.Clone(), configuration: configuration,
		series: make(map[seriesKey]*seriesState), pending: make(map[uint64]*spanState),
		rejections: make(map[RejectionReason]uint64), drops: make(map[DropReason]uint64),
	}
}

func (provider *Provider) isActive() bool {
	if provider == nil {
		return false
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.active
}

func (provider *Provider) lose() {
	provider.mu.Lock()
	provider.active = false
	for _, span := range provider.pending {
		span.completed = true
	}
	clear(provider.pending)
	provider.mu.Unlock()
}

// Identity returns a recursively independent copy of the runtime-injected
// envelope. Mutating it cannot affect the provider or later snapshots.
func (provider *Provider) Identity() RuntimeIdentity {
	if provider == nil {
		return RuntimeIdentity{}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.identity.Clone()
}

// Recorder returns the scoped recorder selected by the runtime's canonical
// source index. The runtime injects this value into the corresponding plugin;
// the plugin itself never supplies an identity.
func (provider *Provider) Recorder(source uint16) (Recorder, error) {
	if provider == nil {
		return Recorder{}, ErrProviderUnavailable
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.active {
		return Recorder{}, ErrProviderUnavailable
	}
	if int(source) >= len(provider.identity.Sources) {
		return Recorder{}, provider.rejectLocked(RejectSourceBound)
	}
	return Recorder{provider: provider, source: source, generation: provider.identity.Generation}, nil
}

// Recorder is a scoped, identity-free recording face. Its fields are private
// so it cannot be retargeted to another source or generation.
type Recorder struct {
	provider   *Provider
	source     uint16
	generation uint64
}

func (recorder Recorder) RecordDuration(metric Metric, nanoseconds uint64, dimensions ...Dimension) error {
	return recorder.record(metric, FamilyDuration, nanoseconds, dimensions)
}

func (recorder Recorder) AddCounter(metric Metric, delta uint64, dimensions ...Dimension) error {
	return recorder.record(metric, FamilyCounter, delta, dimensions)
}

func (recorder Recorder) SetGauge(metric Metric, value int64, dimensions ...Dimension) error {
	provider, err := recorder.lockActive()
	if err != nil {
		return err
	}
	defer provider.mu.Unlock()
	specification, found := metricSpecifications[metric]
	if !found {
		return provider.rejectLocked(RejectUnknownEnum)
	}
	if specification.family != FamilyGauge {
		return provider.rejectLocked(RejectWrongMetricFamily)
	}
	normalized, reject := normalizeDimensions(dimensions, provider.configuration.Limits.MaxDimensionsPerSeries)
	if reject != "" {
		return provider.rejectLocked(reject)
	}
	state, _, err := provider.ensureSeriesLocked(recorder.source, metric, FamilyGauge, normalized)
	if err != nil {
		return err
	}
	return provider.observeGaugeLocked(makeSeriesKey(recorder.source, metric, normalized), state, value)
}

func (recorder Recorder) SetCoverage(metric Metric, coverage Coverage, reason CoverageReason, dimensions ...Dimension) error {
	provider, err := recorder.lockActive()
	if err != nil {
		return err
	}
	defer provider.mu.Unlock()
	specification, found := metricSpecifications[metric]
	if !found {
		return provider.rejectLocked(RejectUnknownEnum)
	}
	normalized, reject := normalizeDimensions(dimensions, provider.configuration.Limits.MaxDimensionsPerSeries)
	if reject != "" {
		return provider.rejectLocked(reject)
	}
	if !validCoverage(coverage, reason, provider.identity.EvidenceClass) {
		return provider.rejectLocked(RejectUnknownEnum)
	}
	state, _, err := provider.ensureSeriesLocked(recorder.source, metric, specification.family, normalized)
	if err != nil {
		return err
	}
	if (state.coverageSet || state.observations() > 0) &&
		(coverage != state.Coverage || reason != state.CoverageReason) {
		return provider.rejectLocked(RejectCoverageConflict)
	}
	state.Coverage, state.CoverageReason = coverage, reason
	state.coverageSet = true
	return nil
}

func (recorder Recorder) BeginDuration(metric Metric, startNS uint64, dimensions ...Dimension) (Span, error) {
	provider, err := recorder.lockActive()
	if err != nil {
		return Span{}, err
	}
	defer provider.mu.Unlock()
	specification, found := metricSpecifications[metric]
	if !found {
		return Span{}, provider.rejectLocked(RejectUnknownEnum)
	}
	if specification.family != FamilyDuration {
		return Span{}, provider.rejectLocked(RejectWrongMetricFamily)
	}
	if specification.crossHost && !provider.identity.Clock.CrossHostSynchronized {
		return Span{}, provider.rejectLocked(RejectIdentityMismatch)
	}
	normalized, reject := normalizeDimensions(dimensions, provider.configuration.Limits.MaxDimensionsPerSeries)
	if reject != "" {
		return Span{}, provider.rejectLocked(reject)
	}
	if len(provider.pending) >= int(provider.configuration.Limits.MaxPendingSpans) {
		return Span{}, provider.rejectLocked(RejectPendingSpanBound)
	}
	state, created, err := provider.ensureSeriesLocked(recorder.source, metric, FamilyDuration, normalized)
	if err != nil {
		return Span{}, err
	}
	_ = state
	if provider.nextSpan == math.MaxUint64 {
		if created {
			delete(provider.series, makeSeriesKey(recorder.source, metric, normalized))
		}
		return Span{}, provider.rejectLocked(RejectTimingOverflow)
	}
	provider.nextSpan++
	span := &spanState{
		provider: provider, generation: recorder.generation, source: recorder.source,
		key: makeSeriesKey(recorder.source, metric, normalized), startNS: startNS, token: provider.nextSpan,
	}
	provider.pending[span.token] = span
	return Span{state: span}, nil
}

func (recorder Recorder) CompleteDuration(span Span, endNS uint64) error {
	provider, err := recorder.lockActive()
	if err != nil {
		return err
	}
	defer provider.mu.Unlock()
	state := span.state
	if state == nil {
		return provider.rejectLocked(RejectUnmatchedSpan)
	}
	if state.provider != provider || state.generation != recorder.generation || state.source != recorder.source {
		return provider.rejectLocked(RejectIdentityMismatch)
	}
	if state.completed {
		return provider.rejectLocked(RejectDuplicateSpan)
	}
	pending, found := provider.pending[state.token]
	if !found || pending != state {
		return provider.rejectLocked(RejectUnmatchedSpan)
	}
	delete(provider.pending, state.token)
	state.completed = true
	if endNS < state.startNS {
		return provider.rejectLocked(RejectBackwardTime)
	}
	duration := endNS - state.startNS
	if duration > MaxDurationNS {
		return provider.rejectLocked(RejectTimingOverflow)
	}
	series := provider.series[state.key]
	return provider.observeLocked(state.key, series, duration)
}

func (recorder Recorder) record(metric Metric, wanted MetricFamily, value uint64, dimensions []Dimension) error {
	provider, err := recorder.lockActive()
	if err != nil {
		return err
	}
	defer provider.mu.Unlock()
	specification, found := metricSpecifications[metric]
	if !found {
		return provider.rejectLocked(RejectUnknownEnum)
	}
	if specification.family != wanted {
		return provider.rejectLocked(RejectWrongMetricFamily)
	}
	if wanted == FamilyDuration && value > MaxDurationNS {
		return provider.rejectLocked(RejectTimingOverflow)
	}
	if specification.crossHost && !provider.identity.Clock.CrossHostSynchronized {
		return provider.rejectLocked(RejectIdentityMismatch)
	}
	normalized, reject := normalizeDimensions(dimensions, provider.configuration.Limits.MaxDimensionsPerSeries)
	if reject != "" {
		return provider.rejectLocked(reject)
	}
	state, created, err := provider.ensureSeriesLocked(recorder.source, metric, wanted, normalized)
	if err != nil {
		return err
	}
	key := makeSeriesKey(recorder.source, metric, normalized)
	if err := provider.observeLocked(key, state, value); err != nil {
		if created && state.observations() == 0 && state.Attempts == 0 {
			delete(provider.series, key)
		}
		return err
	}
	return nil
}

func (recorder Recorder) lockActive() (*Provider, error) {
	provider := recorder.provider
	if provider == nil {
		return nil, ErrProviderUnavailable
	}
	provider.mu.Lock()
	if !provider.active || recorder.generation != provider.identity.Generation ||
		int(recorder.source) >= len(provider.identity.Sources) {
		provider.mu.Unlock()
		return nil, ErrProviderUnavailable
	}
	return provider, nil
}

func (provider *Provider) ensureSeriesLocked(source uint16, metric Metric, family MetricFamily, dimensions []Dimension) (*seriesState, bool, error) {
	key := makeSeriesKey(source, metric, dimensions)
	if state, found := provider.series[key]; found {
		return state, false, nil
	}
	if len(provider.series) >= int(provider.configuration.Limits.MaxSeries) {
		return nil, false, provider.rejectLocked(RejectSeriesBound)
	}
	coverage := CoverageMeasured
	if provider.identity.EvidenceClass == EvidenceDeterministicFixture {
		coverage = CoverageSimulated
	}
	state := &seriesState{
		Source: source, Metric: metric, Family: family, Dimensions: slices.Clone(dimensions),
		Coverage: coverage, CoverageReason: CoverageReasonNone,
	}
	switch family {
	case FamilyDuration:
		state.Duration = &DurationHistogram{BucketCounts: make([]uint64, len(durationBucketUpperBoundsNS))}
	case FamilyCounter:
		state.Counter = &CounterAggregate{}
	case FamilyGauge:
		state.Gauge = &GaugeAggregate{}
	default:
		return nil, false, provider.rejectLocked(RejectUnknownEnum)
	}
	provider.series[key] = state
	if err := provider.enforceArtifactBoundLocked(); err != nil {
		delete(provider.series, key)
		return nil, false, provider.rejectLocked(RejectArtifactBound)
	}
	return state, true, nil
}

func (provider *Provider) observeLocked(key seriesKey, state *seriesState, value uint64) error {
	if state == nil {
		return provider.rejectLocked(RejectUnmatchedSpan)
	}
	if state.Coverage == CoverageUnsupported {
		return provider.rejectLocked(RejectCoverageConflict)
	}
	if state.Attempts == math.MaxUint64 {
		return provider.rejectLocked(RejectObservationBound)
	}
	ordinal := state.Attempts
	if !provider.sampleLocked(key, state.Dimensions, ordinal) {
		state.Attempts++
		state.SamplingDropped = saturatingAdd(state.SamplingDropped, 1)
		provider.drops[DropDeterministicSampling] = saturatingAdd(provider.drops[DropDeterministicSampling], 1)
		return nil
	}
	if state.observations() >= uint64(provider.configuration.Limits.MaxObservationsPerSeries) {
		return provider.rejectLocked(RejectObservationBound)
	}
	switch state.Family {
	case FamilyDuration:
		histogram := state.Duration
		if math.MaxUint64-histogram.SumNS < value {
			return provider.rejectLocked(RejectTimingOverflow)
		}
		bucket := sort.Search(len(durationBucketUpperBoundsNS), func(index int) bool {
			return durationBucketUpperBoundsNS[index] >= value
		})
		if bucket == len(durationBucketUpperBoundsNS) {
			return provider.rejectLocked(RejectTimingOverflow)
		}
		if histogram.Count == 0 {
			histogram.MinNS, histogram.MaxNS = value, value
		} else {
			histogram.MinNS = min(histogram.MinNS, value)
			histogram.MaxNS = max(histogram.MaxNS, value)
		}
		histogram.Count++
		histogram.SumNS += value
		histogram.BucketCounts[bucket]++
	case FamilyCounter:
		if math.MaxUint64-state.Counter.Value < value {
			return provider.rejectLocked(RejectCounterOverflow)
		}
		state.Counter.Value += value
		state.Counter.Observations++
	}
	state.Attempts++
	return nil
}

func (provider *Provider) observeGaugeLocked(key seriesKey, state *seriesState, value int64) error {
	if state == nil {
		return provider.rejectLocked(RejectUnmatchedSpan)
	}
	if state.Coverage == CoverageUnsupported {
		return provider.rejectLocked(RejectCoverageConflict)
	}
	if state.Attempts == math.MaxUint64 {
		return provider.rejectLocked(RejectObservationBound)
	}
	ordinal := state.Attempts
	if !provider.sampleLocked(key, state.Dimensions, ordinal) {
		state.Attempts++
		state.SamplingDropped = saturatingAdd(state.SamplingDropped, 1)
		provider.drops[DropDeterministicSampling] = saturatingAdd(provider.drops[DropDeterministicSampling], 1)
		return nil
	}
	if state.Gauge.Observations >= uint64(provider.configuration.Limits.MaxObservationsPerSeries) {
		return provider.rejectLocked(RejectObservationBound)
	}
	gauge := state.Gauge
	if gauge.Observations == 0 {
		gauge.Min, gauge.Max = value, value
	} else {
		gauge.Min = min(gauge.Min, value)
		gauge.Max = max(gauge.Max, value)
	}
	gauge.Value = value
	gauge.Observations++
	state.Attempts++
	return nil
}

func (provider *Provider) sampleLocked(key seriesKey, dimensions []Dimension, ordinal uint64) bool {
	sampling := provider.configuration.Sampling
	if sampling.Numerator == sampling.Denominator {
		return true
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(SchemaIdentity))
	var integer [8]byte
	binary.BigEndian.PutUint64(integer[:], sampling.Seed)
	_, _ = hash.Write(integer[:])
	binary.BigEndian.PutUint64(integer[:], provider.identity.Generation)
	_, _ = hash.Write(integer[:])
	binary.BigEndian.PutUint64(integer[:], uint64(key.source))
	_, _ = hash.Write(integer[:])
	_, _ = hash.Write([]byte(key.metric))
	for _, dimension := range dimensions {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(dimension.Name))
		_, _ = hash.Write([]byte{'='})
		_, _ = hash.Write([]byte(dimension.Value))
	}
	binary.BigEndian.PutUint64(integer[:], ordinal)
	_, _ = hash.Write(integer[:])
	sum := hash.Sum(nil)
	value := binary.BigEndian.Uint64(sum[:8])
	return value%uint64(sampling.Denominator) < uint64(sampling.Numerator)
}

func normalizeDimensions(source []Dimension, limit uint8) ([]Dimension, RejectionReason) {
	if len(source) > int(limit) {
		return nil, RejectDimensionBound
	}
	result := slices.Clone(source)
	for _, dimension := range result {
		values, nameFound := dimensionPairs[dimension.Name]
		_, valueFound := values[dimension.Value]
		if !nameFound || !valueFound {
			return nil, RejectUnknownEnum
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	for index := 1; index < len(result); index++ {
		if result[index-1].Name == result[index].Name {
			return nil, RejectDimensionBound
		}
	}
	return result, ""
}

func makeSeriesKey(source uint16, metric Metric, dimensions []Dimension) seriesKey {
	var builder strings.Builder
	for _, dimension := range dimensions {
		builder.WriteString(string(dimension.Name))
		builder.WriteByte('=')
		builder.WriteString(string(dimension.Value))
		builder.WriteByte(0)
	}
	return seriesKey{source: source, metric: metric, dimensions: builder.String()}
}

func validCoverage(coverage Coverage, reason CoverageReason, class EvidenceClass) bool {
	switch reason {
	case CoverageReasonNone, CoverageReasonPlatformUnavailable, CoverageReasonCapabilityUnavailable,
		CoverageReasonSensorUnavailable, CoverageReasonPermissionUnavailable, CoverageReasonClockUnsynchronized:
	default:
		return false
	}
	switch coverage {
	case CoverageMeasured:
		return reason == CoverageReasonNone && class != EvidenceDeterministicFixture
	case CoverageSimulated:
		return reason == CoverageReasonNone
	case CoverageUnsupported:
		return reason != CoverageReasonNone
	default:
		return false
	}
}

func (state *seriesState) observations() uint64 {
	switch state.Family {
	case FamilyDuration:
		return state.Duration.Count
	case FamilyCounter:
		return state.Counter.Observations
	case FamilyGauge:
		return state.Gauge.Observations
	default:
		return 0
	}
}

func (provider *Provider) rejectLocked(reason RejectionReason) error {
	provider.rejections[reason] = saturatingAdd(provider.rejections[reason], 1)
	return RejectedError{Reason: reason}
}

func saturatingAdd(value, delta uint64) uint64 {
	if math.MaxUint64-value < delta {
		return math.MaxUint64
	}
	return value + delta
}

func (provider *Provider) enforceArtifactBoundLocked() error {
	snapshot, err := provider.buildSnapshotLocked(false)
	if err != nil {
		return err
	}
	// Reserve the largest decimal representation for every mutable integer and
	// the longest closed coverage strings. This check runs only when a series is
	// created; subsequent observations are therefore O(1) while remaining
	// unable to grow the canonical artifact beyond the selected ceiling.
	for index := range snapshot.Series {
		series := &snapshot.Series[index]
		series.Coverage = CoverageUnsupported
		series.CoverageReason = CoverageReasonCapabilityUnavailable
		series.Attempts = math.MaxUint64
		series.SamplingDropped = math.MaxUint64
		if series.Duration != nil {
			series.Duration.Count = math.MaxUint64
			series.Duration.SumNS = math.MaxUint64
			series.Duration.MinNS = math.MaxUint64
			series.Duration.MaxNS = math.MaxUint64
			for bucket := range series.Duration.BucketCounts {
				series.Duration.BucketCounts[bucket] = math.MaxUint64
			}
		}
		if series.Counter != nil {
			series.Counter.Value = math.MaxUint64
			series.Counter.Observations = math.MaxUint64
		}
		if series.Gauge != nil {
			series.Gauge.Value = math.MinInt64
			series.Gauge.Min = math.MinInt64
			series.Gauge.Max = math.MaxInt64
			series.Gauge.Observations = math.MaxUint64
		}
	}
	for index := range snapshot.Rejections {
		snapshot.Rejections[index].Count = math.MaxUint64
	}
	for index := range snapshot.Drops {
		snapshot.Drops[index].Count = math.MaxUint64
	}
	snapshot.Fingerprint = "sha256:" + strings.Repeat("f", sha256.Size*2)
	payload, err := canonicalSnapshotPayload(snapshot)
	if err != nil {
		return err
	}
	if len(payload)+1 > int(provider.configuration.Limits.MaxArtifactBytes) {
		return fmt.Errorf("performance snapshot would exceed %d bytes", provider.configuration.Limits.MaxArtifactBytes)
	}
	return nil
}

func (rejection RejectedError) Is(target error) bool {
	other, ok := target.(RejectedError)
	return ok && rejection.Reason == other.Reason
}

// RejectionReasonOf extracts the closed reason from a rejected recorder call.
func RejectionReasonOf(err error) (RejectionReason, bool) {
	var rejection RejectedError
	if errors.As(err, &rejection) {
		return rejection.Reason, true
	}
	return "", false
}
