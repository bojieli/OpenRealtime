// Package simtime provides the shared seeded timing model for reference experiments.
package simtime

import (
	"errors"
	"math/rand/v2"
)

const nanosecondsPerMillisecond = uint64(1_000_000)

type Delay struct {
	BaseNS   uint64 `json:"base_ns"`
	JitterNS uint64 `json:"jitter_ns"`
}

type Model struct {
	StreamingRevision  Delay `json:"streaming_revision"`
	PerceptionFinalize Delay `json:"perception_finalize"`
	PerceptionQueue    Delay `json:"perception_queue"`
	Cognition          Delay `json:"cognition"`
	CognitionQueue     Delay `json:"cognition_queue"`
	SpeechFirstChunk   Delay `json:"speech_first_chunk"`
	PlaybackQueue      Delay `json:"playback_queue"`
}

type Stages struct {
	PerceptionFinalizeNS uint64 `json:"perception_finalize_ns"`
	PerceptionQueueNS    uint64 `json:"perception_queue_ns"`
	CognitionNS          uint64 `json:"cognition_ns"`
	CognitionQueueNS     uint64 `json:"cognition_queue_ns"`
	SpeechFirstChunkNS   uint64 `json:"speech_first_chunk_ns"`
	PlaybackQueueNS      uint64 `json:"playback_queue_ns"`
}

func (stages Stages) Sum() uint64 {
	return stages.PerceptionFinalizeNS + stages.PerceptionQueueNS +
		stages.CognitionNS + stages.CognitionQueueNS +
		stages.SpeechFirstChunkNS + stages.PlaybackQueueNS
}

type Sampled struct {
	StreamingRevisionNS uint64
	Stages              Stages
}

func DefaultModel() Model {
	ms := func(base, jitter uint64) Delay {
		return Delay{BaseNS: base * nanosecondsPerMillisecond, JitterNS: jitter * nanosecondsPerMillisecond}
	}
	return Model{
		StreamingRevision:  ms(30, 5),
		PerceptionFinalize: ms(60, 12),
		PerceptionQueue:    ms(3, 1),
		Cognition:          ms(80, 20),
		CognitionQueue:     ms(4, 1),
		SpeechFirstChunk:   ms(55, 10),
		PlaybackQueue:      ms(2, 1),
	}
}

func Sample(model Model, seed uint64) (Sampled, error) {
	values := []*Delay{
		&model.StreamingRevision, &model.PerceptionFinalize, &model.PerceptionQueue,
		&model.Cognition, &model.CognitionQueue, &model.SpeechFirstChunk, &model.PlaybackQueue,
	}
	for _, delay := range values {
		if delay.JitterNS > delay.BaseNS {
			return Sampled{}, errors.New("timing jitter must not exceed its base duration")
		}
	}
	random := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return Sampled{
		StreamingRevisionNS: jitter(model.StreamingRevision, random),
		Stages: Stages{
			PerceptionFinalizeNS: jitter(model.PerceptionFinalize, random),
			PerceptionQueueNS:    jitter(model.PerceptionQueue, random),
			CognitionNS:          jitter(model.Cognition, random),
			CognitionQueueNS:     jitter(model.CognitionQueue, random),
			SpeechFirstChunkNS:   jitter(model.SpeechFirstChunk, random),
			PlaybackQueueNS:      jitter(model.PlaybackQueue, random),
		},
	}, nil
}

func jitter(delay Delay, random *rand.Rand) uint64 {
	if delay.JitterNS == 0 {
		return delay.BaseNS
	}
	width := delay.JitterNS*2 + 1
	offset := random.Uint64N(width)
	return delay.BaseNS - delay.JitterNS + offset
}
