package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
)

type DurationHistogram struct {
	Count        uint64   `json:"count"`
	SumNS        uint64   `json:"sum_ns"`
	MinNS        uint64   `json:"min_ns"`
	MaxNS        uint64   `json:"max_ns"`
	BucketCounts []uint64 `json:"bucket_counts"`
}

type CounterAggregate struct {
	Value        uint64 `json:"value"`
	Observations uint64 `json:"observations"`
}

type GaugeAggregate struct {
	Value        int64  `json:"value"`
	Min          int64  `json:"min"`
	Max          int64  `json:"max"`
	Observations uint64 `json:"observations"`
}

type Series struct {
	Source          uint16             `json:"source"`
	Metric          Metric             `json:"metric"`
	Family          MetricFamily       `json:"family"`
	Dimensions      []Dimension        `json:"dimensions"`
	Coverage        Coverage           `json:"coverage"`
	CoverageReason  CoverageReason     `json:"coverage_reason"`
	Attempts        uint64             `json:"attempts"`
	SamplingDropped uint64             `json:"sampling_dropped"`
	Duration        *DurationHistogram `json:"duration,omitempty"`
	Counter         *CounterAggregate  `json:"counter,omitempty"`
	Gauge           *GaugeAggregate    `json:"gauge,omitempty"`
}

func (series Series) Clone() Series {
	result := series
	result.Dimensions = slices.Clone(series.Dimensions)
	if series.Duration != nil {
		copy := *series.Duration
		copy.BucketCounts = slices.Clone(series.Duration.BucketCounts)
		result.Duration = &copy
	}
	if series.Counter != nil {
		copy := *series.Counter
		result.Counter = &copy
	}
	if series.Gauge != nil {
		copy := *series.Gauge
		result.Gauge = &copy
	}
	return result
}

type RejectionCounter struct {
	Reason RejectionReason `json:"reason"`
	Count  uint64          `json:"count"`
}

type DropCounter struct {
	Reason DropReason `json:"reason"`
	Count  uint64     `json:"count"`
}

type Snapshot struct {
	FormatVersion     uint64             `json:"format_version"`
	Schema            string             `json:"schema"`
	Contract          plugin.Contract    `json:"contract"`
	Identity          RuntimeIdentity    `json:"identity"`
	Configuration     Configuration      `json:"configuration"`
	DurationBucketsNS []uint64           `json:"duration_buckets_ns"`
	Series            []Series           `json:"series"`
	Rejections        []RejectionCounter `json:"rejections"`
	Drops             []DropCounter      `json:"drops"`
	Fingerprint       string             `json:"fingerprint"`
}

func (snapshot Snapshot) Clone() Snapshot {
	result := snapshot
	result.Identity = snapshot.Identity.Clone()
	result.DurationBucketsNS = slices.Clone(snapshot.DurationBucketsNS)
	result.Series = make([]Series, len(snapshot.Series))
	for index := range snapshot.Series {
		result.Series[index] = snapshot.Series[index].Clone()
	}
	result.Rejections = slices.Clone(snapshot.Rejections)
	result.Drops = slices.Clone(snapshot.Drops)
	return result
}

// Snapshot returns a recursively isolated, immutable aggregate. It remains
// available after provider loss so a final generation artifact can be saved.
func (provider *Provider) Snapshot() (Snapshot, error) {
	if provider == nil {
		return Snapshot{}, ErrProviderUnavailable
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.snapshot()
}

var _ Service = (*Provider)(nil)

func (provider *Provider) snapshot() (Snapshot, error) {
	snapshot, err := provider.buildSnapshotLocked(true)
	if err != nil {
		return Snapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot.Clone(), nil
}

func (provider *Provider) buildSnapshotLocked(withFingerprint bool) (Snapshot, error) {
	snapshot := Snapshot{
		FormatVersion: SnapshotFormatVersion, Schema: SchemaIdentity,
		Contract: ServiceContract(), Identity: provider.identity.Clone(),
		Configuration:     provider.configuration,
		DurationBucketsNS: DurationBucketUpperBoundsNS(),
		Series:            make([]Series, 0, len(provider.series)),
		Rejections:        make([]RejectionCounter, 0, len(rejectionOrder)),
		Drops:             []DropCounter{{Reason: DropDeterministicSampling, Count: provider.drops[DropDeterministicSampling]}},
	}
	for _, state := range provider.series {
		duration := cloneDuration(state.Duration)
		counter := cloneCounter(state.Counter)
		gauge := cloneGauge(state.Gauge)
		if state.Coverage == CoverageUnsupported {
			duration, counter, gauge = nil, nil, nil
		}
		snapshot.Series = append(snapshot.Series, Series{
			Source: state.Source, Metric: state.Metric, Family: state.Family,
			Dimensions: slices.Clone(state.Dimensions), Coverage: state.Coverage,
			CoverageReason: state.CoverageReason, Attempts: state.Attempts,
			SamplingDropped: state.SamplingDropped,
			Duration:        duration, Counter: counter, Gauge: gauge,
		})
	}
	sort.Slice(snapshot.Series, func(left, right int) bool {
		return compareSeries(snapshot.Series[left], snapshot.Series[right]) < 0
	})
	for _, reason := range rejectionOrder {
		snapshot.Rejections = append(snapshot.Rejections, RejectionCounter{Reason: reason, Count: provider.rejections[reason]})
	}
	if withFingerprint {
		payload, err := canonicalSnapshotPayload(snapshot)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Fingerprint = digest(payload)
	}
	return snapshot, nil
}

func cloneDuration(source *DurationHistogram) *DurationHistogram {
	if source == nil {
		return nil
	}
	result := *source
	result.BucketCounts = slices.Clone(source.BucketCounts)
	return &result
}

func cloneCounter(source *CounterAggregate) *CounterAggregate {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}

func cloneGauge(source *GaugeAggregate) *GaugeAggregate {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}

func compareSeries(left, right Series) int {
	if left.Source != right.Source {
		if left.Source < right.Source {
			return -1
		}
		return 1
	}
	if left.Metric < right.Metric {
		return -1
	}
	if left.Metric > right.Metric {
		return 1
	}
	leftKey := makeSeriesKey(left.Source, left.Metric, left.Dimensions).dimensions
	rightKey := makeSeriesKey(right.Source, right.Metric, right.Dimensions).dimensions
	if leftKey < rightKey {
		return -1
	}
	if leftKey > rightKey {
		return 1
	}
	return 0
}

func (snapshot Snapshot) Validate() error {
	if snapshot.FormatVersion != SnapshotFormatVersion {
		return fmt.Errorf("unsupported performance snapshot format %d", snapshot.FormatVersion)
	}
	if snapshot.Schema != SchemaIdentity {
		return fmt.Errorf("unknown performance snapshot schema %q", snapshot.Schema)
	}
	if snapshot.Contract != ServiceContract() {
		return errors.New("performance snapshot service contract identity mismatch")
	}
	if err := snapshot.Configuration.Validate(); err != nil {
		return err
	}
	if err := validateRuntimeIdentity(snapshot.Identity, snapshot.Configuration); err != nil {
		return err
	}
	if !slices.Equal(snapshot.DurationBucketsNS, durationBucketUpperBoundsNS[:]) {
		return errors.New("performance snapshot duration buckets do not match schema v1")
	}
	if len(snapshot.Series) > int(snapshot.Configuration.Limits.MaxSeries) {
		return errors.New("performance snapshot exceeds its series bound")
	}
	for index := range snapshot.Series {
		if err := validateSeries(snapshot.Series[index], snapshot); err != nil {
			return fmt.Errorf("performance series %d: %w", index, err)
		}
		if index > 0 && compareSeries(snapshot.Series[index-1], snapshot.Series[index]) >= 0 {
			return errors.New("performance snapshot series are duplicate or not in canonical order")
		}
	}
	if len(snapshot.Rejections) != len(rejectionOrder) {
		return errors.New("performance snapshot rejection vocabulary is incomplete")
	}
	for index, reason := range rejectionOrder {
		if snapshot.Rejections[index].Reason != reason {
			return fmt.Errorf("performance snapshot rejection %d has unknown or non-canonical reason %q",
				index, snapshot.Rejections[index].Reason)
		}
	}
	if len(snapshot.Drops) != 1 || snapshot.Drops[0].Reason != DropDeterministicSampling {
		return errors.New("performance snapshot drop vocabulary is incomplete or non-canonical")
	}
	want := snapshot.Clone()
	want.Fingerprint = ""
	payload, err := canonicalSnapshotPayload(want)
	if err != nil {
		return err
	}
	wantFingerprint := digest(payload)
	if snapshot.Fingerprint != wantFingerprint {
		return fmt.Errorf("performance snapshot fingerprint is %q, want %q", snapshot.Fingerprint, wantFingerprint)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode performance snapshot: %w", err)
	}
	if len(encoded)+1 > int(snapshot.Configuration.Limits.MaxArtifactBytes) || len(encoded)+1 > MaxSnapshotBytes {
		return fmt.Errorf("performance snapshot has %d bytes, limit is %d", len(encoded)+1,
			snapshot.Configuration.Limits.MaxArtifactBytes)
	}
	return nil
}

func validateSeries(series Series, snapshot Snapshot) error {
	if int(series.Source) >= len(snapshot.Identity.Sources) {
		return errors.New("source index is outside runtime identity")
	}
	specification, found := metricSpecifications[series.Metric]
	if !found {
		return fmt.Errorf("unknown metric %q", series.Metric)
	}
	if series.Family != specification.family {
		return errors.New("metric family does not match metric")
	}
	dimensions, reject := normalizeDimensions(series.Dimensions, snapshot.Configuration.Limits.MaxDimensionsPerSeries)
	if reject != "" || !slices.Equal(dimensions, series.Dimensions) {
		return errors.New("dimensions are invalid or not canonical")
	}
	if !validCoverage(series.Coverage, series.CoverageReason, snapshot.Identity.EvidenceClass) {
		return errors.New("coverage is invalid")
	}
	if series.SamplingDropped > series.Attempts {
		return errors.New("sampling drops exceed attempts")
	}
	numeric := 0
	if series.Duration != nil {
		numeric++
	}
	if series.Counter != nil {
		numeric++
	}
	if series.Gauge != nil {
		numeric++
	}
	if series.Coverage == CoverageUnsupported {
		if numeric != 0 || series.Attempts != 0 || series.SamplingDropped != 0 {
			return errors.New("unsupported series must omit numeric evidence")
		}
		return nil
	}
	if numeric != 1 {
		return errors.New("supported series must contain exactly one metric aggregate")
	}
	var observations uint64
	switch series.Family {
	case FamilyDuration:
		if series.Counter != nil || series.Gauge != nil {
			return errors.New("duration series contains another family")
		}
		histogram := series.Duration
		if len(histogram.BucketCounts) != len(durationBucketUpperBoundsNS) {
			return errors.New("duration histogram has the wrong fixed bucket count")
		}
		var count uint64
		for _, value := range histogram.BucketCounts {
			if ^uint64(0)-count < value {
				return errors.New("duration bucket count overflow")
			}
			count += value
		}
		if count != histogram.Count {
			return errors.New("duration bucket counts do not equal count")
		}
		if histogram.Count == 0 {
			if histogram.SumNS != 0 || histogram.MinNS != 0 || histogram.MaxNS != 0 {
				return errors.New("empty duration histogram is non-zero")
			}
		} else if histogram.MinNS > histogram.MaxNS || histogram.MaxNS > MaxDurationNS {
			return errors.New("duration histogram range is invalid")
		}
		observations = histogram.Count
	case FamilyCounter:
		if series.Duration != nil || series.Gauge != nil {
			return errors.New("counter series contains another family")
		}
		observations = series.Counter.Observations
	case FamilyGauge:
		if series.Duration != nil || series.Counter != nil {
			return errors.New("gauge series contains another family")
		}
		if series.Gauge.Observations == 0 {
			if series.Gauge.Value != 0 || series.Gauge.Min != 0 || series.Gauge.Max != 0 {
				return errors.New("empty gauge aggregate is non-zero")
			}
		} else if series.Gauge.Min > series.Gauge.Value || series.Gauge.Value > series.Gauge.Max {
			return errors.New("gauge range does not contain current value")
		}
		observations = series.Gauge.Observations
	default:
		return fmt.Errorf("unknown metric family %q", series.Family)
	}
	if observations > uint64(snapshot.Configuration.Limits.MaxObservationsPerSeries) {
		return errors.New("series exceeds observation bound")
	}
	if observations+series.SamplingDropped > series.Attempts {
		return errors.New("series aggregates exceed attempts")
	}
	return nil
}

func canonicalSnapshotPayload(snapshot Snapshot) ([]byte, error) {
	return json.Marshal(snapshot)
}

func MarshalSnapshot(snapshot Snapshot) ([]byte, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode performance snapshot: %w", err)
	}
	return append(payload, '\n'), nil
}

// DecodeSnapshot accepts only the exact compact canonical representation
// produced by MarshalSnapshot. Duplicate keys, unknown fields, trailing data,
// enum drift, ordering drift, fingerprint drift, and oversized artifacts fail
// closed.
func DecodeSnapshot(reader io.Reader) (Snapshot, error) {
	if reader == nil {
		return Snapshot{}, errors.New("performance snapshot reader is nil")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, MaxSnapshotBytes+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read performance snapshot: %w", err)
	}
	if len(payload) == 0 {
		return Snapshot{}, errors.New("performance snapshot is empty")
	}
	if len(payload) > MaxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("performance snapshot exceeds %d bytes", MaxSnapshotBytes)
	}
	if err := strictjson.Validate(payload); err != nil {
		return Snapshot{}, fmt.Errorf("validate performance snapshot: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode performance snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Snapshot{}, errors.New("performance snapshot has a trailing JSON value")
		}
		return Snapshot{}, fmt.Errorf("decode performance snapshot trailing data: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	canonical, err := MarshalSnapshot(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return Snapshot{}, errors.New("performance snapshot JSON is not canonical")
	}
	return snapshot.Clone(), nil
}
