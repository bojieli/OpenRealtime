// Package fixture loads deterministic audio and its canonical OpenAI input trace.
package fixture

import (
	"bytes"
	"encoding/base64"
	"errors"

	"github.com/bojieli/OpenRealtime/engine"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/replay"
	"github.com/bojieli/OpenRealtime/trace"
)

type Input struct {
	Records     []trace.Record
	Frames      []engine.AudioFrame
	EndpointNS  uint64
	SampleCount uint64
}

func Load(path string, frameMS uint32, sessionID string) (Input, error) {
	var eventOutput, traceOutput bytes.Buffer
	summary, err := replay.WAV(path, replay.Options{
		SessionID: sessionID, FrameDurationMS: frameMS, Events: &eventOutput, Trace: &traceOutput,
	})
	if err != nil {
		return Input{}, err
	}
	result := Input{EndpointNS: summary.DurationNS, SampleCount: summary.InputSampleCount}
	var sampleOffset uint64
	_, err = trace.Read(bytes.NewReader(traceOutput.Bytes()), func(record trace.Record) error {
		result.Records = append(result.Records, record)
		message, err := openaiwire.Decode(record.Message)
		if err != nil {
			return err
		}
		if message.Type() != openaiwire.EventInputAudioBufferAppend {
			return nil
		}
		var payload struct {
			Audio string `json:"audio"`
		}
		if err := message.Unmarshal(&payload); err != nil {
			return err
		}
		pcm, err := base64.StdEncoding.DecodeString(payload.Audio)
		if err != nil {
			return err
		}
		result.Frames = append(result.Frames, engine.AudioFrame{
			Index: uint64(len(result.Frames)), SampleOffset: sampleOffset,
			SampleRateHz: 24_000, PCM16LE: pcm,
		})
		sampleOffset += uint64(len(pcm) / 2)
		return nil
	})
	if err != nil {
		return Input{}, err
	}
	if sampleOffset != summary.InputSampleCount {
		return Input{}, errors.New("input trace sample count does not match replay summary")
	}
	return result, nil
}
