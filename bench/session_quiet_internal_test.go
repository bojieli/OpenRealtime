package bench

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

func TestPassiveEventsDoNotExtendQuietOrWorkingDeadline(t *testing.T) {
	for _, test := range []struct {
		name         string
		openResponse int
		openTool     int
	}{
		{name: "settled"},
		{name: "outstanding response", openResponse: 1},
		{name: "outstanding tool", openTool: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				client := &toolAnswerSession{events: make(chan realtimeclient.Event)}
				recorder := &recorder{
					started: time.Now(), audio: newSessionAudioRecorder(nil),
					openResponses: test.openResponse, openTools: test.openTool,
				}
				recorder.playbackDone(0)
				go func() {
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							for _, event := range []realtimeclient.Event{
								{Type: openrealtime.EventObservationAdded, Raw: []byte(`{"observer":"camera","source":"screen","text":"new frame"}`)},
								{Type: openrealtime.EventDebug, Raw: []byte(`{"category":"session","name":"session.updated","attributes":{"runtime":{"binding":"fixture"}}}`)},
							} {
								select {
								case client.events <- event:
								case <-ctx.Done():
									return
								}
							}
						}
					}
				}()
				started := time.Now()
				transcript := recorder.collect(ctx, client, SessionConfig{
					PostPlaybackQuiet: 100 * time.Millisecond, WorkingTimeout: 500 * time.Millisecond,
					CaptureRuntimeEvidence: true,
				})
				elapsed := time.Since(started)
				if ctx.Err() != nil || elapsed > time.Second {
					t.Fatalf("passive events extended the collection deadline: elapsed=%s error=%v", elapsed, ctx.Err())
				}
				if (test.openResponse > 0 || test.openTool > 0) && elapsed < 500*time.Millisecond {
					t.Fatalf("outstanding work ended on the idle quiet timer: %s", elapsed)
				}
				if transcript.OutstandingResponses != test.openResponse || transcript.OutstandingTools != test.openTool {
					t.Fatalf("outstanding work disappeared from the transcript: %+v", transcript)
				}
				if len(transcript.Moments) == 0 || transcript.Moments[0].Kind != MomentObservation ||
					transcript.Runtime == nil || transcript.Runtime.Binding != "fixture" {
					t.Fatalf("passive evidence was dropped: %+v", transcript)
				}
			})
		})
	}
}

// Actual response progress still resets the working timer, and a terminal
// starts the ordinary quiet interval. Ignoring all events would truncate this
// stream while it is still producing text.
func TestResponseProgressStillExtendsCollection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		client := &toolAnswerSession{events: make(chan realtimeclient.Event)}
		recorder := &recorder{started: time.Now(), audio: newSessionAudioRecorder(nil)}
		recorder.playbackDone(0)
		go func() {
			send := func(kind string, payload any) bool {
				raw, _ := json.Marshal(payload)
				select {
				case client.events <- realtimeclient.Event{Type: kind, Raw: raw}:
					return true
				case <-ctx.Done():
					return false
				}
			}
			if !send("response.created", map[string]any{"response": map[string]any{"id": "answer"}}) {
				return
			}
			for range 4 {
				select {
				case <-time.After(250 * time.Millisecond):
				case <-ctx.Done():
					return
				}
				if !send("response.output_audio_transcript.delta", map[string]any{"response_id": "answer", "delta": "still answering"}) {
					return
				}
			}
			send("response.done", map[string]any{"response": map[string]any{"id": "answer", "status": "completed"}})
		}()
		started := time.Now()
		transcript := recorder.collect(ctx, client, SessionConfig{PostPlaybackQuiet: 100 * time.Millisecond, WorkingTimeout: 500 * time.Millisecond})
		if ctx.Err() != nil || time.Since(started) < 1100*time.Millisecond || transcript.OutstandingResponses != 0 {
			t.Fatalf("response progress did not preserve the response lifecycle: elapsed=%s transcript=%+v", time.Since(started), transcript)
		}
		if len(transcript.Moments) != 5 || transcript.Moments[4].Kind != MomentResponseDone {
			t.Fatalf("response was cut short: %+v", transcript.Moments)
		}
	})
}
