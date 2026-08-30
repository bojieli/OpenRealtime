package graphnative

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

func TestMarshalSourceReviewContextRoundsEveryMillisecondField(t *testing.T) {
	if SourceReviewContextFormatVersion != 3 {
		t.Fatalf("source review context format version = %d", SourceReviewContextFormatVersion)
	}
	metrics := map[string]float64{"reaction_latency_p50_ms": 2.5, "turn_count": 7.25}
	context := SourceReviewContext{
		Format: SourceReviewContextFormat, FormatVersion: SourceReviewContextFormatVersion,
		MediaDurationMS: 11,
		Result: scenario.Result{
			Latencies: []scenario.Latency{{MS: 3.75}},
			Transcript: bench.Transcript{
				Moments:    []bench.Moment{{AtMS: 1.4, AudioMS: 1.5}, {AtMS: 1.6}},
				PlaybackMS: 9.6,
			},
		},
		Architecture: SourceReviewArchitecture{Task: bench.TaskOutcome{Metrics: metrics}},
	}
	first, err := marshalSourceReviewContext(context)
	if err != nil {
		t.Fatal(err)
	}
	second, err := marshalSourceReviewContext(context)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || strictjson.Validate(first) != nil || first[len(first)-1] != '\n' {
		t.Fatalf("derived review context is not stable strict JSON: %q", first)
	}
	decoder := json.NewDecoder(bytes.NewReader(first))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["deterministic_result"].(map[string]any)
	transcript := result["transcript"].(map[string]any)
	moments := transcript["moments"].([]any)
	got := []json.Number{
		moments[0].(map[string]any)["at_ms"].(json.Number),
		moments[0].(map[string]any)["audio_ms"].(json.Number),
		moments[1].(map[string]any)["at_ms"].(json.Number),
		transcript["playback_ms"].(json.Number),
		decoded["media_duration_ms"].(json.Number),
	}
	want := []json.Number{"1", "2", "2", "10", "11"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized millisecond fields = %v, want %v", got, want)
	}
	latency := result["latencies"].([]any)[0].(map[string]any)["ms"].(json.Number)
	if latency.String() != "3.75" {
		t.Fatalf("non-suffix latency changed to %s", latency)
	}
	task := decoded["architecture"].(map[string]any)["task"].(map[string]any)
	gotMetrics := task["metrics"].(map[string]any)
	if gotMetrics["reaction_latency_p50_ms"].(json.Number).String() != "3" ||
		gotMetrics["turn_count"].(json.Number).String() != "7.25" ||
		metrics["reaction_latency_p50_ms"] != 2.5 || context.Result.Transcript.Moments[0].AtMS != 1.4 {
		t.Fatalf("derived metric normalization = %v; source = %v", gotMetrics, metrics)
	}
}

func TestMarshalSourceReviewContextRejectsInvalidMilliseconds(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*SourceReviewContext)
	}{
		{name: "negative playback", mutate: func(value *SourceReviewContext) {
			value.Result.Transcript.PlaybackMS = -0.1
		}},
		{name: "nonfinite event", mutate: func(value *SourceReviewContext) {
			value.Result.Transcript.Moments = []bench.Moment{{AtMS: math.NaN()}}
		}},
		{name: "nonfinite audio", mutate: func(value *SourceReviewContext) {
			value.Result.Transcript.Moments = []bench.Moment{{AudioMS: math.Inf(1)}}
		}},
		{name: "metric beyond int64", mutate: func(value *SourceReviewContext) {
			value.Architecture.Task.Metrics = map[string]float64{"latency_ms": 1e100}
		}},
		{name: "nonnumeric millisecond metric", mutate: func(value *SourceReviewContext) {
			value.Architecture.Task.Notes = map[string]string{"latency_ms": "1.2"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := SourceReviewContext{}
			test.mutate(&value)
			if _, err := marshalSourceReviewContext(value); err == nil ||
				!strings.Contains(err.Error(), "scenario source review context") {
				t.Fatalf("invalid millisecond error = %v", err)
			}
		})
	}
	if err := normalizeSourceReviewMilliseconds(nil); err == nil {
		t.Fatal("nil normalization destination accepted")
	}
}

func TestRoundSourceReviewFloatMillisecondsExactBoundaries(t *testing.T) {
	for _, test := range []struct {
		source float64
		want   float64
	}{
		{source: 0, want: 0},
		{source: math.Nextafter(0.5, 0), want: 0},
		{source: 0.5, want: 1},
		{source: math.Nextafter(1.5, 0), want: 1},
		{source: 1.5, want: 2},
		{source: math.Nextafter(math.Exp2(63), 0), want: math.Nextafter(math.Exp2(63), 0)},
	} {
		got, err := roundSourceReviewFloatMilliseconds(test.source)
		if err != nil || got != test.want {
			t.Fatalf("roundSourceReviewFloatMilliseconds(%g) = %g, %v; want %g", test.source, got, err, test.want)
		}
	}
	for _, invalid := range []float64{-0.1, math.NaN(), math.Inf(1), math.Inf(-1), math.Exp2(63)} {
		if _, err := roundSourceReviewFloatMilliseconds(invalid); err == nil {
			t.Fatalf("invalid millisecond value %g passed normalization", invalid)
		}
	}
}

func TestSourceReviewStereoWAVDurationMSUsesExactIntegerCeiling(t *testing.T) {
	for _, test := range []struct {
		name     string
		frames   int
		duration int64
	}{
		{name: "one frame", frames: 1, duration: 1},
		{name: "exact millisecond", frames: 24, duration: 1},
		{name: "fractional final millisecond", frames: 25, duration: 2},
		{name: "one second", frames: 24_000, duration: 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			duration, err := sourceReviewStereoWAVDurationMS(sourceReviewWAV(test.frames))
			if err != nil || duration != test.duration {
				t.Fatalf("duration = %d, %v; want %d", duration, err, test.duration)
			}
		})
	}
}

func TestSourceReviewStereoWAVDurationMSRejectsShapeDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "empty", mutate: func([]byte) []byte { return nil }},
		{name: "header only", mutate: func(payload []byte) []byte { return payload[:44] }},
		{name: "wrong riff", mutate: func(payload []byte) []byte {
			copy(payload[:4], "RIFX")
			return payload
		}},
		{name: "wrong riff size", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-9))
			return payload
		}},
		{name: "wrong channels", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint16(payload[22:24], 1)
			return payload
		}},
		{name: "wrong rate", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint32(payload[24:28], 16_000)
			return payload
		}},
		{name: "wrong byte rate", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint32(payload[28:32], 48_000)
			return payload
		}},
		{name: "wrong block align", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint16(payload[32:34], 2)
			return payload
		}},
		{name: "wrong data size", mutate: func(payload []byte) []byte {
			binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-45))
			return payload
		}},
		{name: "unaligned data", mutate: func(payload []byte) []byte {
			payload = append(payload, 0)
			binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
			binary.LittleEndian.PutUint32(payload[40:44], uint32(len(payload)-44))
			return payload
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := test.mutate(sourceReviewWAV(25))
			if _, err := sourceReviewStereoWAVDurationMS(payload); err == nil ||
				!strings.Contains(err.Error(), "scenario source review audio") {
				t.Fatalf("malformed WAV error = %v", err)
			}
		})
	}
}

func sourceReviewWAV(frames int) []byte {
	payload := make([]byte, 44+frames*4)
	copy(payload[0:4], "RIFF")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(payload)-8))
	copy(payload[8:12], "WAVE")
	copy(payload[12:16], "fmt ")
	binary.LittleEndian.PutUint32(payload[16:20], 16)
	binary.LittleEndian.PutUint16(payload[20:22], 1)
	binary.LittleEndian.PutUint16(payload[22:24], 2)
	binary.LittleEndian.PutUint32(payload[24:28], 24_000)
	binary.LittleEndian.PutUint32(payload[28:32], 96_000)
	binary.LittleEndian.PutUint16(payload[32:34], 4)
	binary.LittleEndian.PutUint16(payload[34:36], 16)
	copy(payload[36:40], "data")
	binary.LittleEndian.PutUint32(payload[40:44], uint32(frames*4))
	return payload
}
