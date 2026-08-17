package livebench

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// ReadWAV reads uncompressed 16-bit PCM WAV and mixes multiple channels to mono.
func ReadWAV(filename string) (Audio, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Audio{}, fmt.Errorf("open WAV: %w", err)
	}
	defer file.Close()
	return decodeWAV(file)
}

func ReadWAVBytes(data []byte) (Audio, error) {
	return decodeWAV(bytes.NewReader(data))
}

func decodeWAV(reader io.ReadSeeker) (Audio, error) {
	var riff [12]byte
	if _, err := io.ReadFull(reader, riff[:]); err != nil {
		return Audio{}, fmt.Errorf("read WAV header: %w", err)
	}
	if string(riff[:4]) != "RIFF" || string(riff[8:]) != "WAVE" {
		return Audio{}, errors.New("expected a RIFF/WAVE file")
	}

	var channels, bits uint16
	var sampleRate uint32
	for {
		var header [8]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return Audio{}, fmt.Errorf("read WAV chunk: %w", err)
		}
		size := binary.LittleEndian.Uint32(header[4:])
		switch string(header[:4]) {
		case "fmt ":
			if size < 16 {
				return Audio{}, errors.New("WAV fmt chunk is too short")
			}
			format := make([]byte, size)
			if _, err := io.ReadFull(reader, format); err != nil {
				return Audio{}, fmt.Errorf("read WAV format: %w", err)
			}
			if binary.LittleEndian.Uint16(format[:2]) != 1 {
				return Audio{}, errors.New("only uncompressed PCM WAV is supported")
			}
			channels = binary.LittleEndian.Uint16(format[2:4])
			sampleRate = binary.LittleEndian.Uint32(format[4:8])
			bits = binary.LittleEndian.Uint16(format[14:16])
		case "data":
			if channels == 0 || sampleRate == 0 || bits != 16 {
				return Audio{}, fmt.Errorf("WAV must declare 16-bit PCM before data; channels=%d rate=%d bits=%d", channels, sampleRate, bits)
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(reader, data); err != nil {
				return Audio{}, fmt.Errorf("read WAV samples: %w", err)
			}
			mono, err := mixMonoPCM16(data, int(channels))
			if err != nil {
				return Audio{}, err
			}
			return Audio{PCM16: mono, SampleRateHz: int(sampleRate)}, nil
		default:
			if _, err := reader.Seek(int64(size), io.SeekCurrent); err != nil {
				return Audio{}, fmt.Errorf("skip WAV chunk: %w", err)
			}
		}
		if size%2 == 1 {
			if _, err := reader.Seek(1, io.SeekCurrent); err != nil {
				return Audio{}, fmt.Errorf("skip WAV padding: %w", err)
			}
		}
	}
}

func mixMonoPCM16(data []byte, channels int) ([]byte, error) {
	if channels <= 0 || len(data)%(channels*2) != 0 {
		return nil, errors.New("PCM data is not channel-aligned")
	}
	if channels == 1 {
		return slices.Clone(data), nil
	}
	frames := len(data) / (channels * 2)
	mono := make([]byte, frames*2)
	for frame := range frames {
		var sum int64
		for channel := range channels {
			offset := (frame*channels + channel) * 2
			sum += int64(int16(binary.LittleEndian.Uint16(data[offset : offset+2])))
		}
		binary.LittleEndian.PutUint16(mono[frame*2:], uint16(int16(sum/int64(channels))))
	}
	return mono, nil
}

// Resample performs deterministic linear PCM resampling. It is intentionally
// state-free so every benchmark run produces byte-identical provider input.
func Resample(input Audio, sampleRateHz int) (Audio, error) {
	if input.SampleRateHz <= 0 || sampleRateHz <= 0 || len(input.PCM16)%2 != 0 {
		return Audio{}, errors.New("invalid PCM16 audio or sample rate")
	}
	if input.SampleRateHz == sampleRateHz {
		return Audio{PCM16: slices.Clone(input.PCM16), SampleRateHz: sampleRateHz}, nil
	}
	inSamples := len(input.PCM16) / 2
	if inSamples == 0 {
		return Audio{SampleRateHz: sampleRateHz}, nil
	}
	outSamples := (inSamples*sampleRateHz + input.SampleRateHz/2) / input.SampleRateHz
	output := make([]byte, outSamples*2)
	for index := range outSamples {
		positionNum := int64(index) * int64(input.SampleRateHz)
		left := int(positionNum / int64(sampleRateHz))
		fraction := positionNum % int64(sampleRateHz)
		if left >= inSamples-1 {
			left = inSamples - 1
			fraction = 0
		}
		leftSample := int64(int16(binary.LittleEndian.Uint16(input.PCM16[left*2:])))
		rightSample := leftSample
		if left+1 < inSamples {
			rightSample = int64(int16(binary.LittleEndian.Uint16(input.PCM16[(left+1)*2:])))
		}
		value := (leftSample*(int64(sampleRateHz)-fraction) + rightSample*fraction) / int64(sampleRateHz)
		binary.LittleEndian.PutUint16(output[index*2:], uint16(int16(value)))
	}
	return Audio{PCM16: output, SampleRateHz: sampleRateHz}, nil
}

// AlignChunks models immediate playback: a received chunk starts at its arrival
// time unless previously received audio is still playing.
func AlignChunks(chunks []OutputChunk, sampleRateHz int) Audio {
	if sampleRateHz <= 0 {
		return Audio{}
	}
	var output []byte
	cursorSamples := 0
	for _, chunk := range chunks {
		arrivalSamples := int(chunk.Arrival * time.Duration(sampleRateHz) / time.Second)
		if chunk.Flush {
			if arrivalSamples < len(output)/2 {
				clear(output[arrivalSamples*2:])
				output = output[:arrivalSamples*2]
			}
			cursorSamples = arrivalSamples
			continue
		}
		start := max(arrivalSamples, cursorSamples)
		end := start + len(chunk.PCM16)/2
		if required := end * 2; required > len(output) {
			output = append(output, make([]byte, required-len(output))...)
		}
		copy(output[start*2:], chunk.PCM16)
		cursorSamples = end
	}
	return Audio{PCM16: output, SampleRateHz: sampleRateHz}
}

// FitDuration pads or crops audio to an exact benchmark timeline.
func FitDuration(audio Audio, duration time.Duration) Audio {
	if audio.SampleRateHz <= 0 || duration < 0 {
		return Audio{}
	}
	targetBytes := int(duration*time.Duration(audio.SampleRateHz)/time.Second) * 2
	pcm := make([]byte, targetBytes)
	copy(pcm, audio.PCM16)
	return Audio{PCM16: pcm, SampleRateHz: audio.SampleRateHz}
}

func WriteWAV(filename string, audio Audio) (string, error) {
	encoded, err := EncodeWAV(audio)
	if err != nil {
		return "", err
	}
	file, err := os.Create(filename)
	if err != nil {
		return "", fmt.Errorf("create WAV: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	digest := sha256.New()
	writer := io.MultiWriter(file, digest)
	if _, err := writer.Write(encoded); err != nil {
		return "", fmt.Errorf("write WAV: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync WAV: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close WAV: %w", err)
	}
	closed = true
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func EncodeWAV(audio Audio) ([]byte, error) {
	if audio.SampleRateHz <= 0 || len(audio.PCM16)%2 != 0 {
		return nil, errors.New("invalid output PCM16 audio")
	}
	header := make([]byte, 44)
	copy(header[:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(audio.PCM16)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], 1)
	binary.LittleEndian.PutUint32(header[24:28], uint32(audio.SampleRateHz))
	binary.LittleEndian.PutUint32(header[28:32], uint32(audio.SampleRateHz*2))
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(audio.PCM16)))
	return append(header, audio.PCM16...), nil
}

func HashFile(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
