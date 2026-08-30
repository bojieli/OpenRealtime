package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const alternateDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestPluginContractAndDescriptorHaveStableExactIdentity(t *testing.T) {
	contract := ServiceContract()
	if contract.Name != "presentation.client.performance_evidence" || contract.Revision != 1 {
		t.Fatalf("service contract = %+v", contract)
	}
	if err := plugin.ValidateContract(contract); err != nil {
		t.Fatalf("service contract: %v", err)
	}
	descriptor := CollectorDescriptor()
	configurationContract := ConfigurationContract()
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("collector descriptor: %v", err)
	}
	if descriptor.Realm != plugin.ClientRealm || !slices.Equal(descriptor.Platforms, []string{"portable"}) ||
		!slices.Equal(descriptor.Provides, []plugin.Contract{contract}) || descriptor.ConfigSchema == nil ||
		*descriptor.ConfigSchema != configurationContract || len(descriptor.Permissions) != 0 {
		t.Fatalf("collector descriptor boundary = %+v", descriptor)
	}
	identity, err := CollectorDescriptorIdentity()
	if err != nil {
		t.Fatal(err)
	}
	descriptor.Platforms[0] = "mutated"
	second, err := CollectorDescriptorIdentity()
	if err != nil || second != identity {
		t.Fatalf("descriptor identity changed through caller mutation: %+v, %v", second, err)
	}
}

func TestRecorderCoversEveryMetricAndAllMetricFamilies(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 2), configuration)
	recorder, err := provider.Recorder(0)
	if err != nil {
		t.Fatal(err)
	}
	dimensions := []Dimension{{Name: DimensionTransport, Value: ValueWebRTC}}
	for metric, specification := range metricSpecifications {
		switch specification.family {
		case FamilyDuration:
			err = recorder.RecordDuration(metric, 2_000_000, dimensions...)
		case FamilyCounter:
			err = recorder.AddCounter(metric, 3, dimensions...)
		case FamilyGauge:
			err = recorder.SetGauge(metric, 5, dimensions...)
		default:
			t.Fatalf("metric %q has unexpected family %q", metric, specification.family)
		}
		if err != nil {
			t.Fatalf("record %s: %v", metric, err)
		}
	}
	snapshot := mustSnapshot(t, provider)
	if len(snapshot.Series) != len(metricSpecifications) {
		t.Fatalf("series = %d, want %d", len(snapshot.Series), len(metricSpecifications))
	}
	seen := map[Metric]bool{}
	families := map[MetricFamily]bool{}
	for _, series := range snapshot.Series {
		seen[series.Metric] = true
		families[series.Family] = true
		switch series.Family {
		case FamilyDuration:
			if series.Duration == nil || series.Duration.Count != 1 || series.Duration.SumNS != 2_000_000 ||
				series.Counter != nil || series.Gauge != nil {
				t.Fatalf("duration aggregate = %+v", series)
			}
		case FamilyCounter:
			if series.Counter == nil || series.Counter.Value != 3 || series.Counter.Observations != 1 ||
				series.Duration != nil || series.Gauge != nil {
				t.Fatalf("counter aggregate = %+v", series)
			}
		case FamilyGauge:
			if series.Gauge == nil || series.Gauge.Value != 5 || series.Gauge.Min != 5 || series.Gauge.Max != 5 ||
				series.Gauge.Observations != 1 || series.Duration != nil || series.Counter != nil {
				t.Fatalf("gauge aggregate = %+v", series)
			}
		}
	}
	if len(seen) != len(metricSpecifications) || len(families) != 3 {
		t.Fatalf("metric coverage = %d metrics, families %v", len(seen), families)
	}
}

func TestDurationHistogramCounterGaugeAndCoverageSemantics(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	for _, value := range []uint64{100_000, 100_001, MaxDurationNS} {
		if err := recorder.RecordDuration(MetricReconnectRecovery, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.AddCounter(MetricAudioGap, math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	expectReject(t, recorder.AddCounter(MetricAudioGap, 1), RejectCounterOverflow)
	if err := recorder.SetGauge(MetricMemoryCurrent, 10); err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetGauge(MetricMemoryCurrent, 4); err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetGauge(MetricMemoryGrowth, -6); err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetCoverage(MetricVideoCaptureDrop, CoverageUnsupported, CoverageReasonSensorUnavailable); err != nil {
		t.Fatal(err)
	}
	expectReject(t, recorder.AddCounter(MetricVideoCaptureDrop, 1), RejectCoverageConflict)
	expectReject(t, recorder.SetCoverage(MetricVideoCaptureDrop, Coverage("invented"), CoverageReasonNone), RejectUnknownEnum)
	expectReject(t, recorder.SetCoverage(MetricVideoCaptureDrop, CoverageUnsupported, CoverageReason("details")), RejectUnknownEnum)

	snapshot := mustSnapshot(t, provider)
	duration := findSeries(t, snapshot, MetricReconnectRecovery)
	if duration.Duration.Count != 3 || duration.Duration.SumNS != MaxDurationNS+200_001 ||
		duration.Duration.MinNS != 100_000 || duration.Duration.MaxNS != MaxDurationNS {
		t.Fatalf("duration histogram = %+v", duration.Duration)
	}
	var bucketTotal uint64
	for _, count := range duration.Duration.BucketCounts {
		bucketTotal += count
	}
	if bucketTotal != 3 || duration.Duration.BucketCounts[0] != 1 || duration.Duration.BucketCounts[1] != 1 ||
		duration.Duration.BucketCounts[len(duration.Duration.BucketCounts)-1] != 1 {
		t.Fatalf("duration buckets = %v", duration.Duration.BucketCounts)
	}
	gauge := findSeries(t, snapshot, MetricMemoryCurrent).Gauge
	if gauge.Value != 4 || gauge.Min != 4 || gauge.Max != 10 || gauge.Observations != 2 {
		t.Fatalf("gauge = %+v", gauge)
	}
	if growth := findSeries(t, snapshot, MetricMemoryGrowth).Gauge; growth.Value != -6 || growth.Min != -6 || growth.Max != -6 {
		t.Fatalf("signed growth gauge = %+v", growth)
	}
	unsupported := findSeries(t, snapshot, MetricVideoCaptureDrop)
	if unsupported.Coverage != CoverageUnsupported || unsupported.CoverageReason != CoverageReasonSensorUnavailable ||
		unsupported.Counter != nil || unsupported.Duration != nil || unsupported.Gauge != nil {
		t.Fatalf("unsupported coverage = %+v", unsupported)
	}
}

func TestTimingSpansRejectBackwardOverflowUnmatchedDuplicateAndIdentityDrift(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	scope := NewMountScope()
	provider := mustMount(t, scope, runtimeBinding(t, configuration, EvidenceBrowserRuntime, 2), configuration)
	first, _ := provider.Recorder(0)
	second, _ := provider.Recorder(1)

	backward, err := first.BeginDuration(MetricReconnectDetection, 10)
	if err != nil {
		t.Fatal(err)
	}
	expectReject(t, first.CompleteDuration(backward, 9), RejectBackwardTime)
	overflow, err := first.BeginDuration(MetricReconnectDetection, 1)
	if err != nil {
		t.Fatal(err)
	}
	expectReject(t, first.CompleteDuration(overflow, MaxDurationNS+2), RejectTimingOverflow)
	expectReject(t, first.RecordDuration(MetricReconnectDetection, MaxDurationNS+1), RejectTimingOverflow)
	expectReject(t, first.CompleteDuration(Span{}, 1), RejectUnmatchedSpan)
	completed, err := first.BeginDuration(MetricReconnectDetection, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CompleteDuration(completed, 110); err != nil {
		t.Fatal(err)
	}
	expectReject(t, first.CompleteDuration(completed, 111), RejectDuplicateSpan)
	crossSource, err := first.BeginDuration(MetricReconnectRecovery, 100)
	if err != nil {
		t.Fatal(err)
	}
	expectReject(t, second.CompleteDuration(crossSource, 101), RejectIdentityMismatch)
	if err := first.CompleteDuration(crossSource, 101); err != nil {
		t.Fatal(err)
	}

	otherScope := NewMountScope()
	other := mustMount(t, otherScope, runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	otherRecorder, _ := other.Recorder(0)
	foreign, _ := otherRecorder.BeginDuration(MetricReconnectRecovery, 1)
	expectReject(t, first.CompleteDuration(foreign, 2), RejectIdentityMismatch)
	if err := otherRecorder.CompleteDuration(foreign, 2); err != nil {
		t.Fatal(err)
	}

	snapshot := mustSnapshot(t, provider)
	for reason, want := range map[RejectionReason]uint64{
		RejectBackwardTime: 1, RejectTimingOverflow: 2, RejectUnmatchedSpan: 1,
		RejectDuplicateSpan: 1, RejectIdentityMismatch: 2,
	} {
		if got := rejectionCount(snapshot, reason); got != want {
			t.Errorf("rejection %s = %d, want %d", reason, got, want)
		}
	}
}

func TestAllHardAndSelectedBoundsFailClosed(t *testing.T) {
	t.Run("configuration hard ceilings", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxSources = MaxSources + 1
		if _, err := FreezeConfiguration(Configuration{FormatVersion: ConfigurationVersion, Mode: ObservabilityRelease,
			Sampling: Sampling{Numerator: 1, Denominator: 1}, Limits: limits}); err == nil {
			t.Fatal("oversized source limit accepted")
		}
		if _, err := DefaultConfiguration(ObservabilityDisabled); err == nil {
			t.Fatal("disabled collector configuration accepted")
		}
	})
	t.Run("runtime source bound", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxSources = 1
		configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, limits)
		binding := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 2)
		if _, err := NewMountScope().Mount(binding, configuration); err == nil || !strings.Contains(err.Error(), "sources") {
			t.Fatalf("source bound error = %v", err)
		}
		provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
		_, err := provider.Recorder(1)
		expectReject(t, err, RejectSourceBound)
	})
	t.Run("dimension series pending observation", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxDimensionsPerSeries = 1
		limits.MaxSeries = 2
		limits.MaxPendingSpans = 1
		limits.MaxObservationsPerSeries = 1
		configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, limits)
		provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
		recorder, _ := provider.Recorder(0)
		expectReject(t, recorder.SetGauge(MetricMemoryCurrent, 1,
			Dimension{Name: DimensionMedia, Value: ValueAudio},
			Dimension{Name: DimensionTransport, Value: ValueWebSocket}), RejectDimensionBound)
		if err := recorder.SetGauge(MetricMemoryCurrent, 1); err != nil {
			t.Fatal(err)
		}
		expectReject(t, recorder.SetGauge(MetricMemoryCurrent, 2), RejectObservationBound)
		pending, err := recorder.BeginDuration(MetricReconnectDetection, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = recorder.BeginDuration(MetricReconnectRecovery, 1)
		expectReject(t, err, RejectPendingSpanBound)
		if err := recorder.CompleteDuration(pending, 2); err != nil {
			t.Fatal(err)
		}
		expectReject(t, recorder.AddCounter(MetricAudioGap, 1), RejectSeriesBound)
		snapshot := mustSnapshot(t, provider)
		for _, reason := range []RejectionReason{RejectDimensionBound, RejectObservationBound, RejectPendingSpanBound, RejectSeriesBound} {
			if rejectionCount(snapshot, reason) != 1 {
				t.Errorf("rejection %s missing", reason)
			}
		}
	})
	t.Run("artifact ceiling", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxArtifactBytes = 4_096
		configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, limits)
		provider, err := NewMountScope().Mount(runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
		if err != nil {
			t.Skipf("minimum bounded identity is larger than the selected artifact slice: %v", err)
		}
		recorder, _ := provider.Recorder(0)
		var rejected bool
		metrics := sortedMetrics()
		for _, metric := range metrics {
			family, _ := FamilyOf(metric)
			switch family {
			case FamilyDuration:
				err = recorder.RecordDuration(metric, 1)
			case FamilyCounter:
				err = recorder.AddCounter(metric, 1)
			case FamilyGauge:
				err = recorder.SetGauge(metric, 1)
			}
			if reason, ok := RejectionReasonOf(err); ok && reason == RejectArtifactBound {
				rejected = true
				break
			}
		}
		if !rejected {
			t.Fatal("artifact ceiling did not reject a bounded series")
		}
		snapshot := mustSnapshot(t, provider)
		payload, err := MarshalSnapshot(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > int(limits.MaxArtifactBytes) {
			t.Fatalf("artifact = %d bytes", len(payload))
		}
		if rejectionCount(snapshot, RejectArtifactBound) == 0 {
			t.Fatal("artifact rejection is not observable")
		}
	})
}

func TestClosedEnumsAndRedactionByConstruction(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	expectReject(t, recorder.RecordDuration(Metric("https://secret.example/session/response-id?token=credential"), 1), RejectUnknownEnum)
	expectReject(t, recorder.SetGauge(MetricMemoryCurrent, 1, Dimension{Name: DimensionName("label"), Value: DimensionValue("secret")}), RejectUnknownEnum)
	expectReject(t, recorder.AddCounter(MetricMemoryCurrent, 1), RejectWrongMetricFamily)
	if err := recorder.RecordDuration(MetricReducerToViewCommit, 7,
		Dimension{Name: DimensionRender, Value: ValueConversation}); err != nil {
		t.Fatal(err)
	}
	span, err := recorder.BeginDuration(MetricReconnectDetection, 1)
	if err != nil {
		t.Fatal(err)
	}
	encodedSpan, err := json.Marshal(span)
	if err != nil || string(encodedSpan) != "{}" {
		t.Fatalf("opaque span serialized as %s, %v", encodedSpan, err)
	}
	if err := recorder.CompleteDuration(span, 2); err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalSnapshot(mustSnapshot(t, provider))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret.example", "response-id", "credential", `"label"`, `"secret"`} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("snapshot retained forbidden data %q", forbidden)
		}
	}
	if got := rejectionCount(mustSnapshot(t, provider), RejectUnknownEnum); got != 2 {
		t.Fatalf("unknown enum = %d", got)
	}
}

func TestDeterministicSamplingAndGenerationBinding(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilitySampled, Sampling{Numerator: 3, Denominator: 7, Seed: 42}, DefaultLimits())
	binding := runtimeBinding(t, configuration, EvidenceDeterministicFixture, 1)
	collect := func(t *testing.T) Snapshot {
		provider := mustMount(t, NewMountScope(), binding, configuration)
		recorder, _ := provider.Recorder(0)
		for index := uint64(0); index < 100; index++ {
			if err := recorder.AddCounter(MetricAudioGap, index+1, Dimension{Name: DimensionMedia, Value: ValueAudio}); err != nil {
				t.Fatal(err)
			}
		}
		return mustSnapshot(t, provider)
	}
	left, right := collect(t), collect(t)
	if !reflect.DeepEqual(left, right) || left.Fingerprint != right.Fingerprint {
		t.Fatal("same identity/configuration did not produce deterministic evidence")
	}
	series := findSeries(t, left, MetricAudioGap)
	if series.Attempts != 100 || series.SamplingDropped == 0 || series.SamplingDropped == 100 ||
		left.Drops[0].Count != series.SamplingDropped {
		t.Fatalf("sampling aggregate = %+v drops=%+v", series, left.Drops)
	}
	otherConfiguration := mustConfiguration(t, ObservabilitySampled, Sampling{Numerator: 3, Denominator: 7, Seed: 43}, DefaultLimits())
	otherBinding := runtimeBinding(t, otherConfiguration, EvidenceDeterministicFixture, 1)
	other := mustMount(t, NewMountScope(), otherBinding, otherConfiguration)
	otherRecorder, _ := other.Recorder(0)
	for index := uint64(0); index < 100; index++ {
		_ = otherRecorder.AddCounter(MetricAudioGap, index+1, Dimension{Name: DimensionMedia, Value: ValueAudio})
	}
	if mustSnapshot(t, other).Fingerprint == left.Fingerprint {
		t.Fatal("distinct bound sampling configuration has same evidence fingerprint")
	}
}

func TestIdentityMismatchProviderLossRemountAndImmutability(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	binding := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
	tamperedConfiguration := configuration
	tamperedConfiguration.Sampling.Seed = 99
	if _, err := NewMountScope().Mount(binding, tamperedConfiguration); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered configuration accepted: %v", err)
	}
	mismatched := binding
	mismatched.ConfigurationDigest = alternateDigest
	if _, err := NewMountScope().Mount(mismatched, configuration); err == nil || !strings.Contains(err.Error(), "configuration identity") {
		t.Fatalf("configuration identity mismatch accepted: %v", err)
	}

	scope := NewMountScope()
	provider := mustMount(t, scope, binding, configuration)
	recorder, _ := provider.Recorder(0)
	if err := recorder.AddCounter(MetricAudioGap, 1); err != nil {
		t.Fatal(err)
	}
	inputName := binding.Sources[0].Descriptor.Name
	binding.Sources[0].Descriptor.Name = "mutated.source"
	returned := provider.Identity()
	returned.Sources[0].Descriptor.Name = "mutated.return"
	if provider.Identity().Sources[0].Descriptor.Name != inputName {
		t.Fatal("runtime identity aliases caller storage")
	}
	first := mustSnapshot(t, provider)
	first.Identity.Sources[0].Descriptor.Name = "mutated.snapshot"
	first.Series[0].Dimensions = append(first.Series[0].Dimensions, Dimension{Name: DimensionMedia, Value: ValueAudio})
	if current := mustSnapshot(t, provider); current.Identity.Sources[0].Descriptor.Name != inputName || len(current.Series[0].Dimensions) != 0 {
		t.Fatal("snapshot mutation changed provider state")
	}
	if err := scope.Lose(provider); err != nil {
		t.Fatal(err)
	}
	if err := scope.Lose(provider); err != nil {
		t.Fatalf("loss is not idempotent: %v", err)
	}
	if err := recorder.AddCounter(MetricAudioGap, 1); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("stale recorder error = %v", err)
	}
	finalFirst := mustSnapshot(t, provider)
	if findSeries(t, finalFirst, MetricAudioGap).Counter.Value != 1 {
		t.Fatal("provider loss changed final aggregate")
	}

	binding = runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
	remounted := mustMount(t, scope, binding, configuration)
	if remounted.Identity().Generation != provider.Identity().Generation+1 {
		t.Fatalf("remount generation = %d", remounted.Identity().Generation)
	}
	secondRecorder, _ := remounted.Recorder(0)
	if err := secondRecorder.AddCounter(MetricAudioGap, 7); err != nil {
		t.Fatal(err)
	}
	if findSeries(t, mustSnapshot(t, remounted), MetricAudioGap).Counter.Value != 7 {
		t.Fatal("remount inherited old aggregate")
	}
}

func TestSnapshotCanonicalRoundTripStrictDecoderAndTamperResistance(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 2), configuration)
	first, _ := provider.Recorder(0)
	second, _ := provider.Recorder(1)
	_ = first.SetGauge(MetricMemoryCurrent, 9, Dimension{Name: DimensionRender, Value: ValueTrace})
	_ = second.AddCounter(MetricAudioGap, 2, Dimension{Name: DimensionTransport, Value: ValueWebSocket})
	snapshot := mustSnapshot(t, provider)
	payload, err := MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSnapshot(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, snapshot) {
		t.Fatal("canonical round trip changed snapshot")
	}
	if _, err := DecodeSnapshot(bytes.NewReader(bytes.TrimSpace(payload))); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("missing canonical newline error = %v", err)
	}
	pretty := bytes.ReplaceAll(payload, []byte(`,"schema"`), []byte(", \"schema\""))
	if _, err := DecodeSnapshot(bytes.NewReader(pretty)); err == nil {
		t.Fatal("non-canonical whitespace accepted")
	}
	unknown := bytes.Replace(payload, []byte(`"format_version":1`), []byte(`"unknown":0,"format_version":1`), 1)
	if _, err := DecodeSnapshot(bytes.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicate := bytes.Replace(payload, []byte(`"format_version":1`), []byte(`"format_version":1,"format_version":1`), 1)
	if _, err := DecodeSnapshot(bytes.NewReader(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key error = %v", err)
	}
	tampered := snapshot.Clone()
	tampered.Series[0].Attempts++
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("canonical tamper error = %v", err)
	}
	tampered = snapshot.Clone()
	tampered.Series[0].Metric = Metric("new_metric")
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "unknown metric") {
		t.Fatalf("unknown enum snapshot error = %v", err)
	}
	tampered = snapshot.Clone()
	tampered.Series[0], tampered.Series[1] = tampered.Series[1], tampered.Series[0]
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("canonical order tamper error = %v", err)
	}
	oversized := append(bytes.Repeat([]byte(" "), MaxSnapshotBytes), 'x')
	if _, err := DecodeSnapshot(bytes.NewReader(oversized)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized decoder error = %v", err)
	}
}

func TestConcurrentRecordingIsRaceSafeAndBounded(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	const workers = 8
	const calls = 64
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < calls; index++ {
				if err := recorder.AddCounter(MetricAudioUnderflow, 1); err != nil {
					t.Errorf("record: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	series := findSeries(t, mustSnapshot(t, provider), MetricAudioUnderflow)
	if series.Counter.Value != workers*calls || series.Counter.Observations != workers*calls {
		t.Fatalf("concurrent counter = %+v", series.Counter)
	}
}

func TestDefaultCeilingsAdmitAllCanonicalSeriesWithinArtifactLimit(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	choices := [][]Dimension{
		{{}, {Name: DimensionTransport, Value: ValueWebSocket}, {Name: DimensionTransport, Value: ValueWebRTC}},
		{{}, {Name: DimensionMedia, Value: ValueAudio}, {Name: DimensionMedia, Value: ValueVideo}, {Name: DimensionMedia, Value: ValueData}},
		{{}, {Name: DimensionQueue, Value: ValueOutboundMessage}, {Name: DimensionQueue, Value: ValueOutboundBytes}, {Name: DimensionQueue, Value: ValueAudioPlayout}, {Name: DimensionQueue, Value: ValueVideoCapture}},
		{{}, {Name: DimensionRender, Value: ValueConversation}, {Name: DimensionRender, Value: ValueLiveSnapshot}, {Name: DimensionRender, Value: ValueDelta}, {Name: DimensionRender, Value: ValueTrace}},
	}
	count := 0
	for _, transport := range choices[0] {
		for _, media := range choices[1] {
			for _, queue := range choices[2] {
				for _, render := range choices[3] {
					dimensions := make([]Dimension, 0, 4)
					for _, dimension := range []Dimension{transport, media, queue, render} {
						if dimension.Name != "" {
							dimensions = append(dimensions, dimension)
						}
					}
					if err := recorder.RecordDuration(MetricReconnectRecovery, 1, dimensions...); err != nil {
						t.Fatalf("series %d: %v", count, err)
					}
					count++
					if count == int(MaxSeries) {
						goto complete
					}
				}
			}
		}
	}

complete:
	if count != int(MaxSeries) {
		t.Fatalf("generated %d series", count)
	}
	snapshot := mustSnapshot(t, provider)
	if len(snapshot.Series) != int(MaxSeries) {
		t.Fatalf("snapshot has %d series", len(snapshot.Series))
	}
	payload, err := MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > MaxSnapshotBytes {
		t.Fatalf("snapshot has %d bytes", len(payload))
	}
	expectReject(t, recorder.RecordDuration(MetricReconnectDetection, 1), RejectSeriesBound)
}

func TestObservationSaturatesAtExactSchemaCeiling(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	for index := uint32(0); index < MaxObservationsPerSeries; index++ {
		if err := recorder.AddCounter(MetricAudioCaptureDrop, 1); err != nil {
			t.Fatalf("observation %d: %v", index, err)
		}
	}
	expectReject(t, recorder.AddCounter(MetricAudioCaptureDrop, 1), RejectObservationBound)
	series := findSeries(t, mustSnapshot(t, provider), MetricAudioCaptureDrop)
	if series.Counter.Observations != uint64(MaxObservationsPerSeries) ||
		series.Counter.Value != uint64(MaxObservationsPerSeries) ||
		series.Attempts != uint64(MaxObservationsPerSeries) {
		t.Fatalf("saturated aggregate = %+v attempts=%d", series.Counter, series.Attempts)
	}
}

func TestConfigurationRuntimeIdentityAndClockValidationMatrix(t *testing.T) {
	valid := Configuration{
		FormatVersion: ConfigurationVersion, Mode: ObservabilityRelease,
		Sampling: Sampling{Numerator: 1, Denominator: 2}, Limits: DefaultLimits(),
	}
	configurationCases := []struct {
		name   string
		mutate func(*Configuration)
	}{
		{"format", func(value *Configuration) { value.FormatVersion++ }},
		{"unknown mode", func(value *Configuration) { value.Mode = "verbose" }},
		{"disabled", func(value *Configuration) { value.Mode = ObservabilityDisabled }},
		{"zero denominator", func(value *Configuration) { value.Sampling.Denominator = 0 }},
		{"large denominator", func(value *Configuration) { value.Sampling.Denominator = 1<<20 + 1 }},
		{"zero numerator", func(value *Configuration) { value.Sampling.Numerator = 0 }},
		{"large numerator", func(value *Configuration) { value.Sampling.Numerator = 3; value.Sampling.Denominator = 2 }},
		{"zero limit", func(value *Configuration) { value.Limits.MaxSeries = 0 }},
	}
	for _, test := range configurationCases {
		t.Run("configuration/"+test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := FreezeConfiguration(candidate); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}

	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 2}, DefaultLimits())
	bindingCases := []struct {
		name   string
		mutate func(*RuntimeBinding)
	}{
		{"unknown evidence", func(value *RuntimeBinding) { value.EvidenceClass = "unknown" }},
		{"fixture digest absent", func(value *RuntimeBinding) {
			value.EvidenceClass = EvidenceDeterministicFixture
			value.Platform = PlatformHeadless
			value.Clock = ClockIdentity{Domain: ClockVirtualInteger, ResolutionNS: 1, Provenance: ClockFromFixture}
		}},
		{"browser platform", func(value *RuntimeBinding) { value.Platform = PlatformMacOS }},
		{"browser fixture", func(value *RuntimeBinding) { value.FixtureDigest = testDigest }},
		{"macos platform", func(value *RuntimeBinding) { value.EvidenceClass = EvidenceMacOSNative }},
		{"unknown platform", func(value *RuntimeBinding) {
			value.EvidenceClass = EvidenceDeterministicFixture
			value.Platform = "watch"
			value.FixtureDigest = testDigest
			value.Clock = ClockIdentity{Domain: ClockVirtualInteger, ResolutionNS: 1, Provenance: ClockFromFixture}
		}},
		{"clock domain", func(value *RuntimeBinding) { value.Clock.Domain = "wall" }},
		{"clock provenance", func(value *RuntimeBinding) { value.Clock.Provenance = "claimed" }},
		{"clock resolution zero", func(value *RuntimeBinding) { value.Clock.ResolutionNS = 0 }},
		{"clock resolution large", func(value *RuntimeBinding) { value.Clock.ResolutionNS = MaxDurationNS + 1 }},
		{"clock error large", func(value *RuntimeBinding) { value.Clock.CrossHostErrorBoundNS = MaxDurationNS + 1 }},
		{"clock error without sync", func(value *RuntimeBinding) { value.Clock.CrossHostSynchronized = false }},
		{"sync provenance without sync", func(value *RuntimeBinding) {
			value.Clock.CrossHostSynchronized = false
			value.Clock.CrossHostErrorBoundNS = 0
		}},
		{"sync with local provenance", func(value *RuntimeBinding) { value.Clock.Provenance = ClockFromBrowserAPI }},
		{"profile digest", func(value *RuntimeBinding) { value.ProfileFingerprint = "SHA256:bad" }},
		{"fixture digest invalid", func(value *RuntimeBinding) {
			value.EvidenceClass = EvidenceDeterministicFixture
			value.Platform = PlatformHeadless
			value.FixtureDigest = "bad"
			value.Clock = ClockIdentity{Domain: ClockVirtualInteger, ResolutionNS: 1, Provenance: ClockFromFixture}
		}},
		{"collector", func(value *RuntimeBinding) { value.Collector.Name = "invalid" }},
		{"no sources", func(value *RuntimeBinding) { value.Sources = nil }},
		{"source entry", func(value *RuntimeBinding) { value.Sources[0].EntryFingerprint = "bad" }},
		{"source descriptor", func(value *RuntimeBinding) { value.Sources[0].Descriptor.Name = "invalid" }},
		{"source implementation", func(value *RuntimeBinding) { value.Sources[0].ImplementationDigest = "bad" }},
		{"source configuration", func(value *RuntimeBinding) { value.Sources[0].ConfigurationDigest = "bad" }},
		{"duplicate source", func(value *RuntimeBinding) { value.Sources = append(value.Sources, value.Sources[0]) }},
	}
	for _, test := range bindingCases {
		t.Run("binding/"+test.name, func(t *testing.T) {
			candidate := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
			test.mutate(&candidate)
			if _, err := NewMountScope().Mount(candidate, configuration); err == nil {
				t.Fatal("invalid runtime identity accepted")
			}
		})
	}

	mac := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
	mac.EvidenceClass = EvidenceMacOSNative
	mac.Platform = PlatformMacOS
	mac.Clock = ClockIdentity{Domain: ClockMachContinuous, ResolutionNS: 1, Provenance: ClockFromMachClock}
	if _, err := NewMountScope().Mount(mac, configuration); err != nil {
		t.Fatalf("valid macos native identity: %v", err)
	}
	local := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
	local.Clock = ClockIdentity{Domain: ClockBrowserMonotonic, ResolutionNS: 1, Provenance: ClockFromBrowserAPI}
	provider := mustMount(t, NewMountScope(), local, configuration)
	recorder, _ := provider.Recorder(0)
	expectReject(t, recorder.RecordDuration(MetricServerOutputToRenderedAudio, 1), RejectIdentityMismatch)
	if err := recorder.SetCoverage(MetricServerOutputToRenderedAudio, CoverageUnsupported, CoverageReasonClockUnsynchronized); err != nil {
		t.Fatal(err)
	}
}

func TestMountLifecycleRefusesWrongScopeAndProtectsNewGeneration(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	binding := runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1)
	if _, err := (*MountScope)(nil).Mount(binding, configuration); err == nil {
		t.Fatal("nil scope mounted")
	}
	scope := NewMountScope()
	first := mustMount(t, scope, binding, configuration)
	if _, err := scope.Mount(binding, configuration); err == nil {
		t.Fatal("second active provider mounted")
	}
	wrongScope := NewMountScope()
	if err := wrongScope.Lose(first); err == nil {
		t.Fatal("wrong scope disposed active provider")
	}
	if err := scope.Lose(nil); err == nil {
		t.Fatal("nil provider disposed")
	}
	if err := (*MountScope)(nil).Lose(first); err == nil {
		t.Fatal("nil scope disposed")
	}
	if err := scope.Lose(first); err != nil {
		t.Fatal(err)
	}
	second := mustMount(t, scope, binding, configuration)
	if err := scope.Lose(first); err != nil {
		t.Fatalf("stale disposal was not idempotent: %v", err)
	}
	secondRecorder, err := second.Recorder(0)
	if err != nil {
		t.Fatal("stale disposal reached new generation")
	}
	if err := secondRecorder.AddCounter(MetricAudioGap, 1); err != nil {
		t.Fatal(err)
	}
	overflow := NewMountScope()
	overflow.generation = math.MaxUint64
	if _, err := overflow.Mount(binding, configuration); err == nil || !strings.Contains(err.Error(), "generation overflow") {
		t.Fatalf("generation overflow = %v", err)
	}
	if identity := (*Provider)(nil).Identity(); len(identity.Sources) != 0 {
		t.Fatalf("nil identity = %+v", identity)
	}
	if _, err := (*Provider)(nil).Recorder(0); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("nil recorder = %v", err)
	}
	if _, err := (*Provider)(nil).Snapshot(); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("nil snapshot = %v", err)
	}
	if err := (Recorder{}).AddCounter(MetricAudioGap, 1); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("zero recorder = %v", err)
	}
	rejected := RejectedError{Reason: RejectSeriesBound}
	if rejected.Error() == "" || !errors.Is(rejected, RejectedError{Reason: RejectSeriesBound}) || errors.Is(rejected, RejectedError{Reason: RejectSourceBound}) {
		t.Fatal("typed rejection error identity is not stable")
	}
}

func TestSnapshotStructuralValidationRejectsEveryAggregateDrift(t *testing.T) {
	configuration := mustConfiguration(t, ObservabilityRelease, Sampling{Numerator: 1, Denominator: 1}, DefaultLimits())
	provider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	recorder, _ := provider.Recorder(0)
	if err := recorder.RecordDuration(MetricReconnectRecovery, 10); err != nil {
		t.Fatal(err)
	}
	base := mustSnapshot(t, provider)

	cases := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"format", func(value *Snapshot) { value.FormatVersion++ }},
		{"schema", func(value *Snapshot) { value.Schema = "other" }},
		{"contract", func(value *Snapshot) { value.Contract.Digest = alternateDigest }},
		{"generation", func(value *Snapshot) { value.Identity.Generation = 0 }},
		{"mode identity", func(value *Snapshot) { value.Identity.ObservabilityMode = ObservabilitySampled }},
		{"source order", func(value *Snapshot) {
			value.Identity.Sources = append(value.Identity.Sources, value.Identity.Sources[0])
		}},
		{"buckets", func(value *Snapshot) { value.DurationBucketsNS[0]++ }},
		{"source index", func(value *Snapshot) { value.Series[0].Source = 1 }},
		{"family", func(value *Snapshot) { value.Series[0].Family = FamilyCounter }},
		{"dimension", func(value *Snapshot) {
			value.Series[0].Dimensions = []Dimension{{Name: DimensionTransport, Value: ValueAudio}}
		}},
		{"coverage", func(value *Snapshot) {
			value.Series[0].Coverage = CoverageUnsupported
			value.Series[0].CoverageReason = CoverageReasonSensorUnavailable
		}},
		{"sampling attempts", func(value *Snapshot) { value.Series[0].SamplingDropped = value.Series[0].Attempts + 1 }},
		{"no aggregate", func(value *Snapshot) { value.Series[0].Duration = nil }},
		{"two aggregates", func(value *Snapshot) { value.Series[0].Counter = &CounterAggregate{} }},
		{"bucket shape", func(value *Snapshot) {
			value.Series[0].Duration.BucketCounts = value.Series[0].Duration.BucketCounts[:1]
		}},
		{"bucket total", func(value *Snapshot) { value.Series[0].Duration.BucketCounts[0]++ }},
		{"empty duration nonzero", func(value *Snapshot) {
			histogram := value.Series[0].Duration
			histogram.Count = 0
			histogram.SumNS = 1
			clear(histogram.BucketCounts)
		}},
		{"duration range", func(value *Snapshot) { value.Series[0].Duration.MinNS = 11; value.Series[0].Duration.MaxNS = 10 }},
		{"observation bound", func(value *Snapshot) {
			histogram := value.Series[0].Duration
			histogram.Count = uint64(MaxObservationsPerSeries) + 1
			clear(histogram.BucketCounts)
			histogram.BucketCounts[0] = histogram.Count
			value.Series[0].Attempts = histogram.Count
		}},
		{"attempt aggregate", func(value *Snapshot) { value.Series[0].Attempts = 0 }},
		{"rejection vocabulary", func(value *Snapshot) { value.Rejections = value.Rejections[:1] }},
		{"rejection order", func(value *Snapshot) {
			value.Rejections[0], value.Rejections[1] = value.Rejections[1], value.Rejections[0]
		}},
		{"drop vocabulary", func(value *Snapshot) { value.Drops = nil }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := base.Clone()
			test.mutate(&candidate)
			candidate = refingerprint(candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("structurally invalid snapshot accepted")
			}
		})
	}

	counterProvider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	counterRecorder, _ := counterProvider.Recorder(0)
	_ = counterRecorder.AddCounter(MetricAudioGap, 1)
	counterSnapshot := mustSnapshot(t, counterProvider)
	counterSnapshot.Series[0].Gauge = &GaugeAggregate{}
	counterSnapshot = refingerprint(counterSnapshot)
	if err := counterSnapshot.Validate(); err == nil {
		t.Fatal("counter with gauge accepted")
	}

	gaugeProvider := mustMount(t, NewMountScope(), runtimeBinding(t, configuration, EvidenceBrowserRuntime, 1), configuration)
	gaugeRecorder, _ := gaugeProvider.Recorder(0)
	_ = gaugeRecorder.SetGauge(MetricMemoryCurrent, 5)
	gaugeSnapshot := mustSnapshot(t, gaugeProvider)
	gaugeSnapshot.Series[0].Gauge.Min = 6
	gaugeSnapshot = refingerprint(gaugeSnapshot)
	if err := gaugeSnapshot.Validate(); err == nil {
		t.Fatal("gauge range accepted")
	}
}

func TestStrictDecoderRejectsNilEmptyMalformedAndReadFailure(t *testing.T) {
	if _, err := DecodeSnapshot(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	if _, err := DecodeSnapshot(bytes.NewReader(nil)); err == nil {
		t.Fatal("empty snapshot accepted")
	}
	if _, err := DecodeSnapshot(strings.NewReader("{")); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if _, err := DecodeSnapshot(errorReader{}); err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("reader failure = %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func refingerprint(snapshot Snapshot) Snapshot {
	snapshot.Fingerprint = ""
	payload, err := canonicalSnapshotPayload(snapshot)
	if err != nil {
		panic(err)
	}
	snapshot.Fingerprint = digest(payload)
	return snapshot
}

func mustConfiguration(t *testing.T, mode ObservabilityMode, sampling Sampling, limits Limits) Configuration {
	t.Helper()
	configuration, err := FreezeConfiguration(Configuration{
		FormatVersion: ConfigurationVersion, Mode: mode, Sampling: sampling, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func runtimeBinding(t *testing.T, configuration Configuration, class EvidenceClass, sources int) RuntimeBinding {
	t.Helper()
	collector, err := CollectorDescriptorIdentity()
	if err != nil {
		t.Fatal(err)
	}
	binding := RuntimeBinding{
		EvidenceClass: class, Platform: PlatformBrowser,
		Clock: ClockIdentity{Domain: ClockBrowserMonotonic, ResolutionNS: 1_000,
			Provenance: ClockFromSynchronized, CrossHostSynchronized: true, CrossHostErrorBoundNS: 1_000_000},
		ProfileFingerprint: testDigest, LockFingerprint: testDigest, PlanFingerprint: testDigest,
		ManifestFingerprint: testDigest, Collector: collector, ImplementationDigest: testDigest,
		ConfigurationDigest: configuration.Fingerprint,
	}
	if class == EvidenceDeterministicFixture {
		binding.Platform = PlatformHeadless
		binding.Clock = ClockIdentity{
			Domain: ClockVirtualInteger, ResolutionNS: 1, Provenance: ClockFromFixture,
			CrossHostSynchronized: true,
		}
		binding.FixtureDigest = alternateDigest
	}
	for index := 0; index < sources; index++ {
		descriptorDigest := testDigest
		entryDigest := testDigest
		if index%2 == 1 {
			descriptorDigest, entryDigest = alternateDigest, alternateDigest
		}
		binding.Sources = append(binding.Sources, SourceIdentity{
			EntryFingerprint:     entryDigest,
			Descriptor:           plugin.Identity{Name: "test.performance.source" + string(rune('a'+index)), Revision: 1, Digest: descriptorDigest},
			ImplementationDigest: descriptorDigest, ConfigurationDigest: testDigest,
		})
	}
	return binding
}

func mustMount(t *testing.T, scope *MountScope, binding RuntimeBinding, configuration Configuration) *Provider {
	t.Helper()
	provider, err := scope.Mount(binding, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func mustSnapshot(t *testing.T, provider *Provider) Snapshot {
	t.Helper()
	snapshot, err := provider.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func expectReject(t *testing.T, err error, want RejectionReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection %s", want)
	}
	got, ok := RejectionReasonOf(err)
	if !ok || got != want {
		t.Fatalf("rejection = %q (%v), want %q", got, err, want)
	}
}

func rejectionCount(snapshot Snapshot, reason RejectionReason) uint64 {
	for _, counter := range snapshot.Rejections {
		if counter.Reason == reason {
			return counter.Count
		}
	}
	return 0
}

func findSeries(t *testing.T, snapshot Snapshot, metric Metric) Series {
	t.Helper()
	for _, series := range snapshot.Series {
		if series.Metric == metric {
			return series
		}
	}
	t.Fatalf("series %s not found", metric)
	return Series{}
}

func sortedMetrics() []Metric {
	result := make([]Metric, 0, len(metricSpecifications))
	for metric := range metricSpecifications {
		result = append(result, metric)
	}
	slices.Sort(result)
	return result
}
