// Package audio implements the small, deterministic PCM boundary used by replay.
package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	OpenAIPCMSampleRate = uint32(24_000)
	PCM16BytesPerSample = uint16(2)
	MonoChannels        = uint16(1)
)

type Metadata struct {
	SampleRateHz    uint32
	Channels        uint16
	BitsPerSample   uint16
	SampleCount     uint64
	DataLengthBytes uint64
}

func (metadata Metadata) DurationNS() uint64 {
	return metadata.SampleCount * 1_000_000_000 / uint64(metadata.SampleRateHz)
}

type PCM16MonoReader struct {
	file      *os.File
	metadata  Metadata
	remaining uint64
}

func OpenPCM16Mono(path string) (*PCM16MonoReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open WAV: %w", err)
	}
	reader, err := parse(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("parse WAV: %w", err)
	}
	return reader, nil
}

func parse(file *os.File) (*PCM16MonoReader, error) {
	var header [12]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return nil, errors.New("expected RIFF/WAVE container")
	}

	var metadata Metadata
	foundFormat := false
	for {
		var chunkHeader [8]byte
		if _, err := io.ReadFull(file, chunkHeader[:]); err != nil {
			return nil, fmt.Errorf("read chunk header: %w", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := uint64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, errors.New("WAV fmt chunk is shorter than 16 bytes")
			}
			var format [16]byte
			if _, err := io.ReadFull(file, format[:]); err != nil {
				return nil, fmt.Errorf("read fmt chunk: %w", err)
			}
			if binary.LittleEndian.Uint16(format[0:2]) != 1 {
				return nil, errors.New("only uncompressed PCM WAV is supported")
			}
			metadata.Channels = binary.LittleEndian.Uint16(format[2:4])
			metadata.SampleRateHz = binary.LittleEndian.Uint32(format[4:8])
			metadata.BitsPerSample = binary.LittleEndian.Uint16(format[14:16])
			if err := skipChunkRemainder(file, chunkSize-16); err != nil {
				return nil, err
			}
			foundFormat = true
		case "data":
			if !foundFormat {
				return nil, errors.New("WAV data chunk precedes fmt chunk")
			}
			if metadata.Channels != MonoChannels || metadata.BitsPerSample != 16 {
				return nil, fmt.Errorf(
					"OpenAI PCM input requires mono 16-bit audio; got %d channels and %d bits",
					metadata.Channels,
					metadata.BitsPerSample,
				)
			}
			if metadata.SampleRateHz != OpenAIPCMSampleRate {
				return nil, fmt.Errorf(
					"OpenAI PCM input requires %d Hz; got %d Hz",
					OpenAIPCMSampleRate,
					metadata.SampleRateHz,
				)
			}
			if chunkSize%uint64(PCM16BytesPerSample) != 0 {
				return nil, errors.New("PCM data length is not sample-aligned")
			}
			metadata.DataLengthBytes = chunkSize
			metadata.SampleCount = chunkSize / uint64(PCM16BytesPerSample)
			return &PCM16MonoReader{file: file, metadata: metadata, remaining: chunkSize}, nil
		default:
			if err := skipChunkRemainder(file, chunkSize); err != nil {
				return nil, err
			}
		}
	}
}

func skipChunkRemainder(reader io.Seeker, length uint64) error {
	padded := length + length%2
	if padded > uint64(^uint64(0)>>1) {
		return errors.New("WAV chunk is too large")
	}
	if _, err := reader.Seek(int64(padded), io.SeekCurrent); err != nil {
		return fmt.Errorf("skip WAV chunk: %w", err)
	}
	return nil
}

func (reader *PCM16MonoReader) Metadata() Metadata {
	return reader.metadata
}

func (reader *PCM16MonoReader) ReadFrame(buffer []byte) (int, error) {
	if len(buffer) == 0 || len(buffer)%int(PCM16BytesPerSample) != 0 {
		return 0, errors.New("frame buffer must contain a positive whole number of PCM16 samples")
	}
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	requested := uint64(len(buffer))
	if requested > reader.remaining {
		requested = reader.remaining
	}
	read, err := io.ReadFull(reader.file, buffer[:requested])
	reader.remaining -= uint64(read)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return read, errors.New("WAV data chunk ended before its declared length")
	}
	if err != nil {
		return read, err
	}
	return read, nil
}

func (reader *PCM16MonoReader) Close() error {
	return reader.file.Close()
}

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
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataLength)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], MonoChannels)
	binary.LittleEndian.PutUint32(header[24:28], OpenAIPCMSampleRate)
	binary.LittleEndian.PutUint32(header[28:32], OpenAIPCMSampleRate*uint32(PCM16BytesPerSample))
	binary.LittleEndian.PutUint16(header[32:34], PCM16BytesPerSample)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataLength)
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
