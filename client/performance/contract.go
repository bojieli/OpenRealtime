// Package performance implements the portable, payload-free client
// performance-evidence service described by docs/composable-presentation.md.
//
// The package is deliberately independent of any view toolkit. A client
// runtime mounts a collector with exact immutable identities and injects a
// scoped Recorder into selected plugins. Recording callers can supply only
// closed enums and integers; they cannot attach labels, event payloads, URLs,
// session identifiers, or diagnostics.
package performance

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/bojieli/OpenRealtime/plugin"
)

const (
	SchemaIdentity              = "openrealtime/presentation/client-performance-evidence/v1"
	ConfigurationSchemaIdentity = "openrealtime/presentation/client-performance-evidence-config/v1"
	SnapshotFormatVersion       = uint64(1)
	ConfigurationVersion        = uint64(1)

	MaxSources               = uint16(128)
	MaxSeries                = uint16(256)
	MaxDimensionsPerSeries   = uint8(4)
	MaxPendingSpans          = uint16(256)
	MaxObservationsPerSeries = uint32(4096)
	MaxSnapshotBytes         = 256 << 10
	MaxDurationNS            = uint64(24 * 60 * 60 * 1_000_000_000)
)

const serviceDefinition = SchemaIdentity + ":scoped-runtime-bound-payload-free-integer-recorder-immutable-canonical-aggregate-snapshot"
const configurationDefinition = ConfigurationSchemaIdentity + ":mode-deterministic-sampling-hard-bounded-limits-fingerprinted"

// ServiceContract returns a copy of the exact service contract used by client
// profile dependency resolution. Returning a value, rather than exporting a
// mutable package variable, prevents callers from changing the identity seen
// by later registrations.
func ServiceContract() plugin.Contract {
	digest := sha256.Sum256([]byte(serviceDefinition))
	return plugin.Contract{
		Name:     "presentation.client.performance_evidence",
		Revision: 1,
		Digest:   "sha256:" + hex.EncodeToString(digest[:]),
	}
}

// ConfigurationContract identifies the separately fingerprinted collector
// configuration schema used by descriptor/profile compilation.
func ConfigurationContract() plugin.Contract {
	digest := sha256.Sum256([]byte(configurationDefinition))
	return plugin.Contract{
		Name:     "presentation.client.performance_evidence.config",
		Revision: 1,
		Digest:   "sha256:" + hex.EncodeToString(digest[:]),
	}
}

// CollectorDescriptor returns the immutable semantic descriptor for the
// portable collector. Implementations and their artifacts remain deployment
// bindings and are not smuggled into this descriptor.
func CollectorDescriptor() plugin.Descriptor {
	configuration := ConfigurationContract()
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.performance_evidence.collector",
		Revision:      1,
		Realm:         plugin.ClientRealm,
		Platforms:     []string{"portable"},
		Provides:      []plugin.Contract{ServiceContract()},
		ConfigSchema:  &configuration,
		Lifecycle: plugin.Lifecycle{
			QuiesceTimeoutMS: 500,
			DisposeTimeoutMS: 500,
		},
	}
}

// CollectorDescriptorIdentity is convenient for descriptor locks and runtime
// registration tests.
func CollectorDescriptorIdentity() (plugin.Identity, error) {
	return CollectorDescriptor().Identity()
}

// EvidenceClass says what environment can truthfully support the evidence.
type EvidenceClass string

const (
	EvidenceDeterministicFixture EvidenceClass = "deterministic_fixture"
	EvidenceBrowserRuntime       EvidenceClass = "browser_runtime"
	EvidenceMacOSNative          EvidenceClass = "macos_native"
)

// ObservabilityMode is part of the frozen collector configuration. Disabled
// is a closed vocabulary member but cannot be mounted: a disabled profile must
// omit both collector and probes so it remains a valid overhead baseline.
type ObservabilityMode string

const (
	ObservabilityDisabled ObservabilityMode = "disabled"
	ObservabilitySampled  ObservabilityMode = "sampled"
	ObservabilityRelease  ObservabilityMode = "release"
)

type ClientPlatform string

const (
	PlatformHeadless ClientPlatform = "headless"
	PlatformBrowser  ClientPlatform = "browser"
	PlatformMacOS    ClientPlatform = "macos"
)

type ClockDomain string

const (
	ClockVirtualInteger   ClockDomain = "virtual_integer_ns"
	ClockProcessMonotonic ClockDomain = "process_monotonic_ns"
	ClockBrowserMonotonic ClockDomain = "browser_monotonic_ns"
	ClockMachContinuous   ClockDomain = "mach_continuous_ns"
)

type ClockProvenance string

const (
	ClockFromFixture      ClockProvenance = "deterministic_fixture"
	ClockFromGoMonotonic  ClockProvenance = "go_monotonic"
	ClockFromBrowserAPI   ClockProvenance = "browser_performance_api"
	ClockFromMachClock    ClockProvenance = "mach_continuous_time"
	ClockFromSynchronized ClockProvenance = "bounded_cross_host_sync"
)

type MetricFamily string

const (
	FamilyDuration MetricFamily = "duration_ns"
	FamilyCounter  MetricFamily = "counter"
	FamilyGauge    MetricFamily = "gauge"
)

// Metric is the complete v1 metric vocabulary. Units and families are fixed
// by metricSpecifications; a caller cannot rename or reinterpret a series.
type Metric string

const (
	MetricEndpointToFirstRenderedAudio Metric = "endpoint_to_first_rendered_audio_ns"
	MetricServerOutputToRenderedAudio  Metric = "server_output_to_rendered_audio_ns"
	MetricAudioScheduled               Metric = "audio_scheduled_ns"
	MetricAudioCompleted               Metric = "audio_completed_ns"
	MetricVideoCaptureToAdmissionAck   Metric = "video_capture_to_admission_ack_ns"
	MetricReconnectDetection           Metric = "reconnect_detection_ns"
	MetricReconnectRecovery            Metric = "reconnect_recovery_ns"
	MetricReducerToViewCommit          Metric = "reducer_publish_to_view_commit_ns"
	MetricLongTask                     Metric = "long_task_ns"
	MetricMainLoopStall                Metric = "main_loop_stall_ns"
	MetricLiveSnapshotRender           Metric = "live_snapshot_render_ns"
	MetricDeltaRender                  Metric = "delta_render_ns"
	MetricTraceRender                  Metric = "trace_render_ns"

	MetricAudioGap           Metric = "audio_gap_count"
	MetricAudioUnderflow     Metric = "audio_underflow_count"
	MetricAudioOverrun       Metric = "audio_overrun_count"
	MetricAudioCaptureDrop   Metric = "audio_capture_drop_count"
	MetricVideoCaptureDrop   Metric = "video_capture_drop_count"
	MetricReconnectLostEvent Metric = "reconnect_lost_event_count"
	MetricReconnectDuplicate Metric = "reconnect_duplicate_event_count"

	MetricClientMessageQueue Metric = "client_message_queue_depth"
	MetricClientByteQueue    Metric = "client_byte_queue_bytes"
	MetricMemoryBaseline     Metric = "memory_baseline_bytes"
	MetricMemoryCurrent      Metric = "memory_current_bytes"
	MetricMemoryHighWater    Metric = "memory_high_water_bytes"
	MetricMemoryGrowth       Metric = "memory_growth_bytes"
)

type metricSpecification struct {
	family    MetricFamily
	crossHost bool
}

var metricSpecifications = map[Metric]metricSpecification{
	MetricEndpointToFirstRenderedAudio: {family: FamilyDuration},
	MetricServerOutputToRenderedAudio:  {family: FamilyDuration, crossHost: true},
	MetricAudioScheduled:               {family: FamilyDuration},
	MetricAudioCompleted:               {family: FamilyDuration},
	MetricVideoCaptureToAdmissionAck:   {family: FamilyDuration, crossHost: true},
	MetricReconnectDetection:           {family: FamilyDuration},
	MetricReconnectRecovery:            {family: FamilyDuration},
	MetricReducerToViewCommit:          {family: FamilyDuration},
	MetricLongTask:                     {family: FamilyDuration},
	MetricMainLoopStall:                {family: FamilyDuration},
	MetricLiveSnapshotRender:           {family: FamilyDuration},
	MetricDeltaRender:                  {family: FamilyDuration},
	MetricTraceRender:                  {family: FamilyDuration},
	MetricAudioGap:                     {family: FamilyCounter},
	MetricAudioUnderflow:               {family: FamilyCounter},
	MetricAudioOverrun:                 {family: FamilyCounter},
	MetricAudioCaptureDrop:             {family: FamilyCounter},
	MetricVideoCaptureDrop:             {family: FamilyCounter},
	MetricReconnectLostEvent:           {family: FamilyCounter},
	MetricReconnectDuplicate:           {family: FamilyCounter},
	MetricClientMessageQueue:           {family: FamilyGauge},
	MetricClientByteQueue:              {family: FamilyGauge},
	MetricMemoryBaseline:               {family: FamilyGauge},
	MetricMemoryCurrent:                {family: FamilyGauge},
	MetricMemoryHighWater:              {family: FamilyGauge},
	MetricMemoryGrowth:                 {family: FamilyGauge},
}

func FamilyOf(metric Metric) (MetricFamily, bool) {
	specification, found := metricSpecifications[metric]
	return specification.family, found
}

// DimensionName and DimensionValue are separate closed vocabularies. Only
// the name/value pairs in dimensionPairs are valid.
type DimensionName string
type DimensionValue string

const (
	DimensionTransport DimensionName = "transport"
	DimensionMedia     DimensionName = "media"
	DimensionQueue     DimensionName = "queue"
	DimensionRender    DimensionName = "render"

	ValueWebSocket       DimensionValue = "websocket"
	ValueWebRTC          DimensionValue = "webrtc"
	ValueAudio           DimensionValue = "audio"
	ValueVideo           DimensionValue = "video"
	ValueData            DimensionValue = "data"
	ValueOutboundMessage DimensionValue = "outbound_messages"
	ValueOutboundBytes   DimensionValue = "outbound_bytes"
	ValueAudioPlayout    DimensionValue = "audio_playout"
	ValueVideoCapture    DimensionValue = "video_capture"
	ValueConversation    DimensionValue = "conversation"
	ValueLiveSnapshot    DimensionValue = "live_snapshot"
	ValueDelta           DimensionValue = "delta"
	ValueTrace           DimensionValue = "trace"
)

var dimensionPairs = map[DimensionName]map[DimensionValue]struct{}{
	DimensionTransport: {ValueWebSocket: {}, ValueWebRTC: {}},
	DimensionMedia:     {ValueAudio: {}, ValueVideo: {}, ValueData: {}},
	DimensionQueue: {
		ValueOutboundMessage: {}, ValueOutboundBytes: {}, ValueAudioPlayout: {}, ValueVideoCapture: {},
	},
	DimensionRender: {ValueConversation: {}, ValueLiveSnapshot: {}, ValueDelta: {}, ValueTrace: {}},
}

type Dimension struct {
	Name  DimensionName  `json:"name"`
	Value DimensionValue `json:"value"`
}

type Coverage string

const (
	CoverageMeasured    Coverage = "measured"
	CoverageSimulated   Coverage = "simulated"
	CoverageUnsupported Coverage = "unsupported"
)

type CoverageReason string

const (
	CoverageReasonNone                  CoverageReason = "none"
	CoverageReasonPlatformUnavailable   CoverageReason = "platform_unavailable"
	CoverageReasonCapabilityUnavailable CoverageReason = "capability_unavailable"
	CoverageReasonSensorUnavailable     CoverageReason = "sensor_unavailable"
	CoverageReasonPermissionUnavailable CoverageReason = "permission_unavailable"
	CoverageReasonClockUnsynchronized   CoverageReason = "clock_unsynchronized"
)

type RejectionReason string

const (
	RejectUnknownEnum         RejectionReason = "unknown_enum"
	RejectWrongMetricFamily   RejectionReason = "wrong_metric_family"
	RejectBackwardTime        RejectionReason = "backward_time"
	RejectTimingOverflow      RejectionReason = "timing_overflow"
	RejectUnmatchedSpan       RejectionReason = "unmatched_span"
	RejectDuplicateSpan       RejectionReason = "duplicate_span"
	RejectSourceBound         RejectionReason = "source_bound"
	RejectSeriesBound         RejectionReason = "series_bound"
	RejectDimensionBound      RejectionReason = "dimension_bound"
	RejectPendingSpanBound    RejectionReason = "pending_span_bound"
	RejectObservationBound    RejectionReason = "observation_bound"
	RejectCounterOverflow     RejectionReason = "counter_overflow"
	RejectCoverageConflict    RejectionReason = "coverage_conflict"
	RejectIdentityMismatch    RejectionReason = "identity_mismatch"
	RejectArtifactBound       RejectionReason = "artifact_bound"
	RejectProviderUnavailable RejectionReason = "provider_unavailable"
)

var rejectionOrder = []RejectionReason{
	RejectUnknownEnum, RejectWrongMetricFamily, RejectBackwardTime, RejectTimingOverflow,
	RejectUnmatchedSpan, RejectDuplicateSpan, RejectSourceBound, RejectSeriesBound,
	RejectDimensionBound, RejectPendingSpanBound, RejectObservationBound,
	RejectCounterOverflow, RejectCoverageConflict, RejectIdentityMismatch,
	RejectArtifactBound, RejectProviderUnavailable,
}

type DropReason string

const DropDeterministicSampling DropReason = "deterministic_sampling"

var durationBucketUpperBoundsNS = [...]uint64{
	100_000, 250_000, 500_000,
	1_000_000, 2_000_000, 5_000_000, 10_000_000, 20_000_000, 50_000_000,
	100_000_000, 250_000_000, 500_000_000,
	1_000_000_000, 2_000_000_000, 5_000_000_000, 10_000_000_000,
	30_000_000_000, 60_000_000_000, 300_000_000_000, 3_600_000_000_000,
	MaxDurationNS,
}

func DurationBucketUpperBoundsNS() []uint64 {
	return append([]uint64(nil), durationBucketUpperBoundsNS[:]...)
}
