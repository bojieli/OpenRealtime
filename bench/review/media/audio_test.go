package media

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestEncodeStereoWAVAlignsRoomAndAgentWithSaturation(t *testing.T) {
	capture := bench.SessionAudioCapture{
		SampleRateHz: 24_000,
		RoomPCM16:    []int16{100, -100, 200, -200},
		Agent: []bench.TimedAudioChunk{
			{AtMS: 0, PCM16: []int16{30_000, -30_000}},
			{AtMS: 0, PCM16: []int16{10_000, -10_000}},
			{AtMS: 4 * 1000 / 24_000.0, PCM16: []int16{321}},
		},
	}
	wav, spec, err := EncodeStereoWAV(capture)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Container != "wav" || spec.Encoding != "pcm_s16le" ||
		spec.SampleRateHz != 24_000 || spec.Channels != 2 || spec.Frames != 5 ||
		len(spec.ChannelLayout) != 2 || len(wav) != 44+5*4 {
		t.Fatalf("WAV spec=%+v bytes=%d", spec, len(wav))
	}
	want := [][2]int16{
		{100, math.MaxInt16}, {-100, math.MinInt16}, {200, 0}, {-200, 0}, {0, 321},
	}
	for frame := range want {
		left := int16(binary.LittleEndian.Uint16(wav[44+frame*4 : 46+frame*4]))
		right := int16(binary.LittleEndian.Uint16(wav[46+frame*4 : 48+frame*4]))
		if [2]int16{left, right} != want[frame] {
			t.Errorf("frame %d = [%d %d], want %v", frame, left, right, want[frame])
		}
	}
	if binary.LittleEndian.Uint32(wav[4:8]) != uint32(len(wav)-8) ||
		binary.LittleEndian.Uint32(wav[40:44]) != uint32(len(wav)-44) {
		t.Fatal("WAV RIFF/data lengths do not match encoded bytes")
	}
}

func TestEncodeStereoWAVRejectsUnreviewableCapturesBeforeAllocation(t *testing.T) {
	tooManyChunks := make([]bench.TimedAudioChunk, maximumAudioChunks+1)
	for _, test := range []struct {
		name    string
		capture bench.SessionAudioCapture
		match   string
	}{
		{"sample rate", bench.SessionAudioCapture{SampleRateHz: 48_000, RoomPCM16: []int16{1}}, "want 24000"},
		{"chunk count", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: tooManyChunks}, "agent chunks"},
		{"empty", bench.SessionAudioCapture{}, "no samples"},
		{"empty chunk", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: 0}}}, "is empty"},
		{"negative time", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: -1, PCM16: []int16{1}}}}, "invalid start"},
		{"NaN time", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: math.NaN(), PCM16: []int16{1}}}}, "invalid start"},
		{"uint64 conversion boundary", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: float64(math.MaxUint64) * 1000 / 24_000, PCM16: []int16{1}}}}, "exceeds the WAV timeline"},
		{"oversized timeline", bench.SessionAudioCapture{RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: 2_000_000, PCM16: []int16{1}}}}, "container size"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := EncodeStereoWAV(test.capture); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("EncodeStereoWAV() error = %v, want %q", err, test.match)
			}
		})
	}
}
