package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	reviewSampleRateHz = uint32(24_000)
	reviewChannels     = uint16(2)
	maximumAudioBytes  = uint64(128 << 20)
	maximumAudioChunks = 100_000
)

// EncodeStereoWAV aligns exact room input and timed agent output on the
// session playback clock. Overlapping agent chunks are mixed with saturation.
func EncodeStereoWAV(capture bench.SessionAudioCapture) ([]byte, AudioSpec, error) {
	rate := capture.SampleRateHz
	if rate == 0 {
		rate = reviewSampleRateHz
	}
	if rate != reviewSampleRateHz {
		return nil, AudioSpec{}, fmt.Errorf("capture sample rate is %d Hz, want %d Hz", rate, reviewSampleRateHz)
	}
	if len(capture.Agent) > maximumAudioChunks {
		return nil, AudioSpec{}, fmt.Errorf("review audio exceeds %d agent chunks", maximumAudioChunks)
	}
	frames := uint64(len(capture.RoomPCM16))
	starts := make([]uint64, len(capture.Agent))
	for index, chunk := range capture.Agent {
		if len(chunk.PCM16) == 0 {
			return nil, AudioSpec{}, fmt.Errorf("agent chunk %d is empty", index)
		}
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < 0 {
			return nil, AudioSpec{}, fmt.Errorf("agent chunk %d has invalid start %.3f ms", index, chunk.AtMS)
		}
		startFloat := math.Round(chunk.AtMS * float64(rate) / 1000)
		// float64(math.MaxUint64) rounds to 2^64 and is not convertible to
		// uint64. Reject the rounded boundary, not just values above it.
		if startFloat >= float64(math.MaxUint64) {
			return nil, AudioSpec{}, fmt.Errorf("agent chunk %d start exceeds the WAV timeline", index)
		}
		start := uint64(startFloat)
		if uint64(len(chunk.PCM16)) > math.MaxUint64-start {
			return nil, AudioSpec{}, fmt.Errorf("agent chunk %d exceeds the WAV timeline", index)
		}
		starts[index] = start
		if end := start + uint64(len(chunk.PCM16)); end > frames {
			frames = end
		}
	}
	if frames == 0 {
		return nil, AudioSpec{}, errors.New("review audio contains no samples")
	}
	maxFrames := min(
		uint64(math.MaxUint32-36)/(uint64(reviewChannels)*2),
		(maximumAudioBytes-44)/(uint64(reviewChannels)*2),
	)
	if frames > maxFrames {
		return nil, AudioSpec{}, errors.New("review audio exceeds the WAV container size")
	}
	dataBytes := frames * uint64(reviewChannels) * 2
	interleaved := make([]int16, int(frames)*int(reviewChannels))
	for index, sample := range capture.RoomPCM16 {
		interleaved[index*2] = sample
	}
	for chunkIndex, chunk := range capture.Agent {
		start := starts[chunkIndex]
		for sampleIndex, sample := range chunk.PCM16 {
			position := int(start+uint64(sampleIndex))*2 + 1
			interleaved[position] = saturatingAdd16(interleaved[position], sample)
		}
	}
	wav := make([]byte, 44+len(interleaved)*2)
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+dataBytes))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], reviewChannels)
	binary.LittleEndian.PutUint32(wav[24:28], rate)
	binary.LittleEndian.PutUint32(wav[28:32], rate*uint32(reviewChannels)*2)
	binary.LittleEndian.PutUint16(wav[32:34], reviewChannels*2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(dataBytes))
	for index, sample := range interleaved {
		binary.LittleEndian.PutUint16(wav[44+index*2:], uint16(sample))
	}
	spec := AudioSpec{
		Container: "wav", Encoding: "pcm_s16le", SampleRateHz: rate, Channels: reviewChannels,
		ChannelLayout: []string{"left:scripted_room_user", "right:agent_output"},
		Frames:        frames, DurationMS: float64(frames) * 1000 / float64(rate),
	}
	return wav, spec, nil
}

func saturatingAdd16(left, right int16) int16 {
	sum := int32(left) + int32(right)
	if sum > math.MaxInt16 {
		return math.MaxInt16
	}
	if sum < math.MinInt16 {
		return math.MinInt16
	}
	return int16(sum)
}
