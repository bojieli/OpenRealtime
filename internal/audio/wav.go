// Package audio implements a small, deterministic PCM WAV boundary.
//
// It reads uncompressed PCM16 in whatever rate and channel count a file
// happens to carry, and converts to the mono 16-bit form everything inside the
// runtime uses. Being strict about the container and permissive about the
// rate is deliberate: a fixture recorded at 44.1 kHz is a normal thing to be
// handed, and refusing it would push resampling onto every caller.
package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	// OpenAIPCMSampleRate is the rate the Realtime wire uses for audio/pcm.
	OpenAIPCMSampleRate = uint32(24_000)
	// PCM16BytesPerSample is the size of one sample in one channel.
	PCM16BytesPerSample = uint16(2)
	MonoChannels        = uint16(1)
)

// Metadata describes a decoded file.
type Metadata struct {
	SampleRateHz    uint32
	Channels        uint16
	BitsPerSample   uint16
	SampleCount     uint64
	DataLengthBytes uint64
}

// DurationNS is the playing time of the decoded audio.
func (metadata Metadata) DurationNS() uint64 {
	if metadata.SampleRateHz == 0 {
		return 0
	}
	return metadata.SampleCount * 1_000_000_000 / uint64(metadata.SampleRateHz)
}

// Decoded is mono PCM16 plus what it came from.
type Decoded struct {
	Metadata Metadata
	// PCM16LE is mono, little-endian, at Metadata.SampleRateHz.
	PCM16LE []byte
}

// ReadFile decodes a WAV file into mono PCM16.
func ReadFile(path string) (Decoded, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Decoded{}, fmt.Errorf("open WAV: %w", err)
	}
	return Decode(raw)
}

// Decode parses a WAV container from memory.
func Decode(raw []byte) (Decoded, error) {
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return Decoded{}, errors.New("expected a RIFF/WAVE container")
	}
	var metadata Metadata
	foundFormat := false
	encoding := uint16(1)
	offset := 12
	for offset+8 <= len(raw) {
		chunkID := string(raw[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		body := offset + 8
		if chunkSize < 0 || body+chunkSize > len(raw) {
			// A truncated final chunk is common in recordings that were cut
			// short. Taking what is there beats refusing the file.
			chunkSize = len(raw) - body
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return Decoded{}, errors.New("WAV fmt chunk is shorter than 16 bytes")
			}
			format := raw[body : body+16]
			encoding = binary.LittleEndian.Uint16(format[0:2])
			if encoding != 1 && encoding != 3 && encoding != 0xFFFE {
				return Decoded{}, fmt.Errorf("only uncompressed PCM WAV is supported, got encoding %d", encoding)
			}
			metadata.Channels = binary.LittleEndian.Uint16(format[2:4])
			metadata.SampleRateHz = binary.LittleEndian.Uint32(format[4:8])
			metadata.BitsPerSample = binary.LittleEndian.Uint16(format[14:16])
			foundFormat = true
		case "data":
			if !foundFormat {
				return Decoded{}, errors.New("WAV data chunk precedes fmt chunk")
			}
			if metadata.Channels == 0 {
				return Decoded{}, errors.New("WAV declares no channels")
			}
			if metadata.SampleRateHz == 0 {
				return Decoded{}, errors.New("WAV declares no sample rate")
			}
			payload, err := toPCM16(raw[body:body+chunkSize], metadata.BitsPerSample, encoding)
			if err != nil {
				return Decoded{}, err
			}
			metadata.BitsPerSample = 16
			mono := downmix(payload, metadata.Channels)
			metadata.DataLengthBytes = uint64(len(mono))
			metadata.SampleCount = uint64(len(mono) / int(PCM16BytesPerSample))
			return Decoded{Metadata: metadata, PCM16LE: mono}, nil
		}
		offset = body + chunkSize + chunkSize%2
	}
	return Decoded{}, errors.New("WAV contains no data chunk")
}

// toPCM16 converts a sample format to signed 16-bit.
//
// Recordings in the wild are 16-bit integer, 24-bit integer, 32-bit integer,
// or 32-bit float, and refusing three of those would push conversion onto
// every caller for no reason: the arithmetic is four lines and the result is
// exact for everything but the low bits, which are below the noise floor of
// anything this system will hear.
func toPCM16(payload []byte, bits uint16, encoding uint16) ([]byte, error) {
	switch {
	case bits == 16 && encoding != 3:
		return payload, nil
	case bits == 32 && encoding == 3:
		return convert(payload, 4, func(word []byte) int16 {
			bits := binary.LittleEndian.Uint32(word)
			value := math.Float32frombits(bits)
			return clampToInt16(float64(value) * 32767)
		}), nil
	case bits == 32:
		return convert(payload, 4, func(word []byte) int16 {
			return int16(int32(binary.LittleEndian.Uint32(word)) >> 16)
		}), nil
	case bits == 24:
		return convert(payload, 3, func(word []byte) int16 {
			value := int32(word[0]) | int32(word[1])<<8 | int32(int8(word[2]))<<16
			return int16(value >> 8)
		}), nil
	case bits == 8:
		// 8-bit WAV is unsigned by definition.
		return convert(payload, 1, func(word []byte) int16 {
			return int16(int(word[0])-128) << 8
		}), nil
	default:
		return nil, fmt.Errorf("unsupported WAV sample format: %d bits, encoding %d", bits, encoding)
	}
}

func convert(payload []byte, stride int, decode func([]byte) int16) []byte {
	count := len(payload) / stride
	converted := make([]byte, count*2)
	for index := 0; index < count; index++ {
		binary.LittleEndian.PutUint16(converted[index*2:], uint16(decode(payload[index*stride:])))
	}
	return converted
}

func clampToInt16(value float64) int16 {
	if value > 32767 {
		return 32767
	}
	if value < -32768 {
		return -32768
	}
	return int16(value)
}

// downmix averages interleaved channels into one.
func downmix(payload []byte, channels uint16) []byte {
	if channels == 1 {
		return payload
	}
	stride := int(channels) * int(PCM16BytesPerSample)
	frames := len(payload) / stride
	mono := make([]byte, frames*int(PCM16BytesPerSample))
	for frame := 0; frame < frames; frame++ {
		total := 0
		for channel := 0; channel < int(channels); channel++ {
			offset := frame*stride + channel*int(PCM16BytesPerSample)
			total += int(int16(binary.LittleEndian.Uint16(payload[offset:])))
		}
		binary.LittleEndian.PutUint16(mono[frame*2:], uint16(int16(total/int(channels))))
	}
	return mono
}

// PCM16MonoReader streams a decoded file in fixed frames.
type PCM16MonoReader struct {
	decoded Decoded
	offset  int
}

// OpenPCM16Mono decodes a file for framed reading.
func OpenPCM16Mono(path string) (*PCM16MonoReader, error) {
	decoded, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &PCM16MonoReader{decoded: decoded}, nil
}

// Metadata describes the open file.
func (reader *PCM16MonoReader) Metadata() Metadata { return reader.decoded.Metadata }

// ReadFrame fills buffer with the next samples, returning io.EOF at the end.
func (reader *PCM16MonoReader) ReadFrame(buffer []byte) (int, error) {
	if len(buffer) == 0 || len(buffer)%int(PCM16BytesPerSample) != 0 {
		return 0, errors.New("frame buffer must contain a positive whole number of PCM16 samples")
	}
	if reader.offset >= len(reader.decoded.PCM16LE) {
		return 0, io.EOF
	}
	copied := copy(buffer, reader.decoded.PCM16LE[reader.offset:])
	reader.offset += copied
	return copied, nil
}

// Close exists so callers can treat the reader like a file handle.
func (reader *PCM16MonoReader) Close() error { return nil }

// EncodeWAVMono16 wraps mono PCM16LE in a canonical 44-byte WAV header.
//
// Batch transcription endpoints take a container rather than raw samples, so
// something has to put the header on. It lives here because the decoder does
// too, and a writer that disagreed with its own reader would be found by a
// benchmark rather than by a test.
func EncodeWAVMono16(pcm []byte, sampleRateHz uint32) ([]byte, error) {
	if sampleRateHz == 0 {
		return nil, errors.New("WAV sample rate must be positive")
	}
	if len(pcm)%2 != 0 {
		return nil, errors.New("WAV payload must contain whole PCM16 samples")
	}
	if uint64(len(pcm)) > math.MaxUint32-36 {
		return nil, errors.New("WAV payload exceeds the container size field")
	}
	encoded := make([]byte, 44, 44+len(pcm))
	writeWAVHeader(encoded, sampleRateHz, uint32(len(pcm)))
	return append(encoded, pcm...), nil
}

// writeWAVHeader fills a 44-byte canonical header in place.
func writeWAVHeader(header []byte, sampleRateHz uint32, dataLength uint32) {
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataLength)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], MonoChannels)
	binary.LittleEndian.PutUint32(header[24:28], sampleRateHz)
	binary.LittleEndian.PutUint32(header[28:32], sampleRateHz*uint32(PCM16BytesPerSample))
	binary.LittleEndian.PutUint16(header[32:34], PCM16BytesPerSample)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataLength)
}

// GenerateFixture writes a deterministic two-tone WAV.
//
// It is the smallest thing that exercises the whole audio path with a known
// answer: two bursts separated by silence, at a fixed rate, hashing to a
// stable digest. Reproduction scripts assert that digest so a decode change
// that silently alters audio is caught by the fixture rather than by a
// benchmark result three steps later.
func GenerateFixture(path string) ([32]byte, error) {
	const durationMilliseconds = 1_000
	sampleCount := uint32(OpenAIPCMSampleRate) * durationMilliseconds / 1_000
	dataLength := sampleCount * uint32(PCM16BytesPerSample)
	file, err := os.Create(path)
	if err != nil {
		return [32]byte{}, fmt.Errorf("create fixture: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			file.Close()
		}
	}()

	header := make([]byte, 44)
	writeWAVHeader(header, OpenAIPCMSampleRate, dataLength)
	if _, err := file.Write(header); err != nil {
		return [32]byte{}, fmt.Errorf("write fixture header: %w", err)
	}

	pcm := make([]byte, dataLength)
	for sample := uint32(0); sample < sampleCount; sample++ {
		millisecond := sample * 1_000 / OpenAIPCMSampleRate
		var amplitude int16
		switch {
		case millisecond >= 200 && millisecond < 500:
			amplitude = squareSample(sample, 440)
		case millisecond >= 600 && millisecond < 900:
			amplitude = squareSample(sample, 660)
		default:
			amplitude = 0
		}
		binary.LittleEndian.PutUint16(pcm[sample*2:sample*2+2], uint16(amplitude))
	}
	if _, err := file.Write(pcm); err != nil {
		return [32]byte{}, fmt.Errorf("write fixture PCM: %w", err)
	}
	if err := file.Sync(); err != nil {
		return [32]byte{}, fmt.Errorf("sync fixture: %w", err)
	}
	if err := file.Close(); err != nil {
		return [32]byte{}, fmt.Errorf("close fixture: %w", err)
	}
	closed = true
	return HashFile(path)
}

func squareSample(sample uint32, frequency uint32) int16 {
	halfPeriod := OpenAIPCMSampleRate / (frequency * 2)
	if halfPeriod == 0 || (sample/halfPeriod)%2 == 0 {
		return 8_000
	}
	return -8_000
}

func HashFile(path string) ([32]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}
