package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

// renderSpeech is a separately labelled synthesis and paced playback probe.
// The sink is the benchmark recorder, not a physical speaker or human listener.
// Each chunk is acknowledged only after its sample duration has elapsed.
func renderSpeech(ctx context.Context, config speechsocket.Config, text, dir string) error {
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	started := time.Now()
	speech, err := speechsocket.Open(ctx, config)
	if err != nil {
		return err
	}
	defer speech.Close()
	var ledger capability.Playback
	if err = ledger.Add("segment-1", text, 0, int(speech.SampleRate())); err != nil {
		return err
	}
	marks, err := os.Create(filepath.Join(dir, "playback.jsonl"))
	if err != nil {
		return err
	}
	defer marks.Close()
	encoder := json.NewEncoder(marks)
	if err = speech.Append(ctx, text); err != nil {
		return err
	}
	if err = speech.End(ctx); err != nil {
		return err
	}
	var pcm []byte
	var played int64
	for {
		select {
		case <-ctx.Done():
			ledger.Cancel("segment-1")
			return ctx.Err()
		case chunk, ok := <-speech.Audio():
			if !ok {
				if err = speech.Err(); err != nil {
					return err
				}
				if len(pcm) == 0 {
					return fmt.Errorf("synthesis returned no audio")
				}
				if err = ledger.End("segment-1"); err != nil {
					return err
				}
				// Last chunk was acknowledged before EOF was known. Ending synthesis
				// now establishes that all generated samples have actually been played.
				if err = ledger.FinishPlayback("segment-1", time.Since(started)); err != nil {
					return err
				}
				wav, encodeErr := audio.EncodeWAVMono16(pcm, speech.SampleRate())
				if encodeErr != nil {
					return encodeErr
				}
				if err = os.WriteFile(filepath.Join(dir, "output.wav"), wav, 0644); err != nil {
					return err
				}
				return writeJSON(filepath.Join(dir, "result.json"), map[string]any{
					"ledger": ledger, "playback_sink": "paced-benchmark-recorder",
					"physical_playback": false, "word_alignment": "unavailable",
					"capabilities": speech.Capabilities(), "model": speech.Model(),
					"sample_rate": speech.SampleRate(), "wall_ns": time.Since(started),
				})
			}
			samples := int64(len(chunk.PCM16LE) / 2)
			if samples == 0 || len(chunk.PCM16LE)%2 != 0 {
				return fmt.Errorf("invalid PCM chunk length")
			}
			if err = ledger.Generated("segment-1", samples); err != nil {
				return err
			}
			timer := time.NewTimer(time.Duration(samples) * time.Second / time.Duration(speech.SampleRate()))
			select {
			case <-ctx.Done():
				timer.Stop()
				ledger.Cancel("segment-1")
				return ctx.Err()
			case <-timer.C:
			}
			pcm = append(pcm, chunk.PCM16LE...)
			played += samples
			at := time.Since(started)
			if err = ledger.Mark("segment-1", played, at); err != nil {
				return err
			}
			if err = encoder.Encode(map[string]any{
				"at_ns": at, "played_samples": played, "segment_id": "segment-1",
				"mark": "paced-recorder-acknowledgement", "sample_rate": speech.SampleRate(),
			}); err != nil {
				return err
			}
		}
	}
}
