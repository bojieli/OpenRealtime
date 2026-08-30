package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVideoFramePTSRequiresExactOrderedEvidence(t *testing.T) {
	payload := []byte(`{"frames":[{"best_effort_timestamp_time":"0.125123","pkt_duration_time":"0.300333"},{"best_effort_timestamp_time":"0.425456","pkt_duration_time":"0.325333"},{"best_effort_timestamp_time":"0.750789","pkt_duration_time":"0.374088"}]}`)
	pts, err := parseVideoFramePTS(payload, 3, 125_123)
	if err != nil || len(pts) != 3 || pts[0] != 125_123 || pts[2] != 750_789 {
		t.Fatalf("parseVideoFramePTS() = %v, %v", pts, err)
	}
	for _, test := range []struct {
		name    string
		payload []byte
		count   int
		start   int64
	}{
		{"duplicate pts", []byte(`{"frames":[{"best_effort_timestamp_time":"0.125123"},{"best_effort_timestamp_time":"0.125123"}]}`), 2, 125_123},
		{"wrong count", payload, 2, 125_123},
		{"wrong start", payload, 3, 125_124},
		{"negative", []byte(`{"frames":[{"best_effort_timestamp_time":"-0.000001"}]}`), 1, -1},
		{"duplicate key", []byte(`{"frames":[],"frames":[]}`), 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseVideoFramePTS(test.payload, test.count, test.start); err == nil {
				t.Fatal("invalid decoded-frame evidence unexpectedly passed")
			}
		})
	}
}

func TestParseVideoPacketTimelineRequiresExactRawMediaCoverage(t *testing.T) {
	payload := []byte(`{"packets":[{"pts_time":"0.000000","duration_time":"0.300333"},{"pts_time":"0.300333","duration_time":"0.325333"},{"pts_time":"0.625666","duration_time":"0.499211"}]}`)
	pts, err := parseVideoPacketTimeline(payload, 3, 0, 1_124_877)
	if err != nil || len(pts) != 3 || pts[1] != 300_333 {
		t.Fatalf("parseVideoPacketTimeline() = %v, %v", pts, err)
	}
	for _, test := range []struct {
		name    string
		payload []byte
		end     int64
	}{
		{"gap", []byte(`{"packets":[{"pts_time":"0.000000","duration_time":"0.300332"},{"pts_time":"0.300333","duration_time":"0.824544"}]}`), 1_124_877},
		{"overlap", []byte(`{"packets":[{"pts_time":"0.000000","duration_time":"0.300334"},{"pts_time":"0.300333","duration_time":"0.824544"}]}`), 1_124_877},
		{"zero duration", []byte(`{"packets":[{"pts_time":"0.000000","duration_time":"0.000000"}]}`), 1},
		{"wrong end", payload, 1_124_878},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseVideoPacketTimeline(test.payload, lenPacketFixture(test.payload), 0, test.end); err == nil {
				t.Fatal("invalid raw packet timeline unexpectedly passed")
			}
		})
	}
}

func TestParseAudioFrameTimelineDistinguishesPresentationAndAACPadding(t *testing.T) {
	payload := audioFrameFixture(t, 304)
	evidence, err := parseAudioFrameTimeline(payload, 1_250_000)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.FrameCount != 30 || evidence.DecodedSamples != 30_720 ||
		evidence.PresentedSamples != 30_000 || evidence.TailPaddingSamples != 720 {
		t.Fatalf("audio evidence = %+v", evidence)
	}

	nonaligned, err := parseAudioFrameTimeline(audioFrameFixture(t, 307), 1_250_123)
	if err != nil || nonaligned.PresentedSamples != 30_003 || nonaligned.TailPaddingSamples != 717 {
		t.Fatalf("nonaligned audio evidence = %+v, %v", nonaligned, err)
	}
	for _, test := range []struct {
		name    string
		payload []byte
		endUS   int64
	}{
		{"wrong presentation end", audioFrameFixture(t, 305), 1_250_000},
		{"empty", []byte(`{"frames":[]}`), 1_250_000},
		{"invalid json", []byte(`{"frames":[}`), 1_250_000},
		{"zero attempt", payload, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseAudioFrameTimeline(test.payload, test.endUS); err == nil {
				t.Fatal("invalid decoded-audio evidence unexpectedly passed")
			}
		})
	}
}

func TestParseDecimalUSAcceptsOnlyCanonicalMicroseconds(t *testing.T) {
	for value, want := range map[string]int64{
		"0.000000":             0,
		"0.000001":             1,
		"-0.000001":            -1,
		"9223372036854.775807": math.MaxInt64,
	} {
		got, err := parseDecimalUS(value)
		if err != nil || got != want {
			t.Fatalf("parseDecimalUS(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{
		"", "+0.000000", "0", "0.0", "0.0000000", "00.000001", "-0.000000",
		"9223372036854.775808", "9223372036855.000000", "nan", "1e-6",
	} {
		if _, err := parseDecimalUS(value); err == nil {
			t.Fatalf("parseDecimalUS(%q) unexpectedly passed", value)
		}
	}
}

func TestSensitiveGuardAndBoundedBufferFailClosedAtLimits(t *testing.T) {
	values := make([]string, maximumSensitiveValues)
	for index := range values {
		values[index] = strings.Repeat("x", 32) + string(rune(0x1000+index))
	}
	guard, err := newSensitiveGuard(values)
	if err != nil || !guard.rejects([]byte("prefix"+values[17]+"suffix")) {
		t.Fatalf("bounded guard = %+v, %v", guard, err)
	}
	if got := guard.safeError(context.Background(), "operation", errors.New(values[17]), nil); got == nil || got.Error() != "operation failed" {
		t.Fatalf("secret diagnostic = %v", got)
	}
	guard.destroy()
	if guard.rejects([]byte(values[17])) {
		t.Fatal("destroyed guard retained a declared value")
	}
	if _, err := newSensitiveGuard(append(values, "one-too-many")); err == nil {
		t.Fatal("oversized sensitive-value cardinality unexpectedly passed")
	}
	if _, err := newSensitiveGuard([]string{strings.Repeat("x", maximumSensitiveValue+1)}); err == nil {
		t.Fatal("oversized sensitive value unexpectedly passed")
	}

	buffer := &boundedBuffer{maximum: 3}
	if count, err := buffer.Write([]byte("ab")); count != 2 || err != nil {
		t.Fatalf("first bounded write = %d, %v", count, err)
	}
	if count, err := buffer.Write([]byte("cd")); count != 2 || err != nil {
		t.Fatalf("overflowing bounded write = %d, %v", count, err)
	}
	if got := string(buffer.Bytes()); got != "abc" || buffer.seen != 4 {
		t.Fatalf("bounded buffer = %q seen=%d", got, buffer.seen)
	}
}

func TestRemovePrivateTreeHandlesSealedEntriesWithoutFollowingSymlinks(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "private")
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(nested, "sealed.bin")
	if err := os.WriteFile(sealed, []byte("evidence"), 0o400); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(parent, "external.bin")
	if err := os.WriteFile(external, []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(nested, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := removePrivateTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("private root survived cleanup: %v", err)
	}
	if payload, err := os.ReadFile(external); err != nil || string(payload) != "retain" {
		t.Fatalf("external symlink target changed: %q, %v", payload, err)
	}
}

func BenchmarkParseVideoPacketTimeline(b *testing.B) {
	payload := []byte(`{"packets":[{"pts_time":"0.000000","duration_time":"0.300333"},{"pts_time":"0.300333","duration_time":"0.325333"},{"pts_time":"0.625666","duration_time":"0.499211"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := parseVideoPacketTimeline(payload, 3, 0, 1_124_877); err != nil {
			b.Fatal(err)
		}
	}
}

func audioFrameFixture(t testing.TB, finalDuration int64) []byte {
	t.Helper()
	frames := make([]probeAudioFrame, 30)
	for index := range frames {
		frames[index] = probeAudioFrame{PTS: int64(index * 1024), Duration: 1024, Samples: 1024}
	}
	frames[len(frames)-1].Duration = finalDuration
	payload, err := json.Marshal(audioFrameEnvelope{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func lenPacketFixture(payload []byte) int {
	var envelope packetEnvelope
	if json.Unmarshal(payload, &envelope) != nil {
		return 1
	}
	return len(envelope.Packets)
}
