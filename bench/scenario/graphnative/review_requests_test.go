package graphnative

import (
	"encoding/binary"
	"strings"
	"testing"
)

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
