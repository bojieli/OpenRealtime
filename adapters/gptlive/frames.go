package gptlive

import (
	"bytes"
	"context"
	"encoding/base64"
	"time"
)

// GPT-Live runs on a real-time audio clock, and nothing happens without one.
//
// This is the difference that no amount of reading the event contract reveals,
// and it fails silently. A session with no input frames arriving accepts a
// commentary append, never injects it, never acknowledges it, never speaks, and
// never reports an error - it simply sits there. Against the real endpoint a
// hand-off sent into a silent session produced nothing for seventy-five
// seconds; the identical hand-off sent while silence was streaming was spoken
// in about a second. The vendor says as much in passing - acknowledgements
// "wait until frame progress reaches the estimated end of context injection",
// and "if frame progress stops, an acknowledgment can remain pending" - and its
// own guidance for a greeting is to "keep input audio running, including
// silence before the caller speaks".
//
// The Realtime protocol has no such requirement. A caller speaking it pauses
// input whenever the user is quiet, and every one of them would stall this
// endpoint. Since the whole point of this package is that callers do not have
// to know what is on the other side, keeping the clock running belongs here.
//
// What this sends is a gap filler, not a mixer: a frame goes out only when the
// caller has supplied nothing for a whole frame interval, so real audio is
// never padded or displaced - it is what a microphone in a quiet room produces,
// which is what the endpoint is expecting to hear.
const (
	// defaultFrameInterval is how often input audio is expected. Twenty
	// milliseconds is the ordinary packetisation of live speech.
	defaultFrameInterval = 20 * time.Millisecond
)

// frameSamples is one interval of audio at the session's rate.
func (client *Client) frameSamples() int {
	samples := int(client.config.SessionSampleRateHz) * int(client.config.FrameInterval/time.Millisecond) / 1000
	if samples <= 0 {
		samples = 1
	}
	return samples
}

// silenceFrame is one interval of silence in the session's format. PCM
// silence is zero samples; each G.711 law spells zero its own way.
func (client *Client) silenceFrame() []byte {
	samples := client.frameSamples()
	switch client.config.SessionFormat {
	case FormatMuLaw:
		return bytes.Repeat([]byte{0xFF}, samples)
	case FormatALaw:
		return bytes.Repeat([]byte{0xD5}, samples)
	default:
		return make([]byte, samples*2)
	}
}

// startFrameClock begins filling gaps in the caller's input stream.
func (client *Client) startFrameClock(ctx context.Context) {
	if client.config.FrameInterval <= 0 {
		return
	}
	client.silence = base64.StdEncoding.EncodeToString(client.silenceFrame())
	client.armFrameClock(ctx)
}

// armFrameClock schedules the next gap check.
func (client *Client) armFrameClock(ctx context.Context) {
	timer := client.config.Scheduler.AfterFunc(client.config.FrameInterval, func() {
		select {
		case <-client.closed:
			return
		default:
		}
		client.fillFrameGap(ctx)
		client.armFrameClock(ctx)
	})
	client.writeMu.Lock()
	client.frameTimer = timer
	client.writeMu.Unlock()
}

// fillFrameGap sends one frame of silence if the caller sent nothing this tick.
//
// The test is a flag set by the caller's own appends rather than a comparison
// against the clock, because a time window has a boundary and this does not: a
// caller streaming at exactly the frame interval lands on that boundary every
// tick, and whether its audio counted would come down to which of two events
// the scheduler ran first. "Did anything arrive since the last tick" has one
// answer.
//
// There is no check that the session has started. The clock is armed by the
// session.started handler and nowhere else, so it cannot run before the
// handshake; a guard for that would be a branch no test could reach.
func (client *Client) fillFrameGap(ctx context.Context) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.callerAppended {
		// The caller is streaming. Its audio is the clock, and padding a live
		// stream would displace real speech on the endpoint's timeline.
		client.callerAppended = false
		return
	}
	if err := client.write(ctx, map[string]any{
		"type": "session.input_audio.append", "audio": client.silence,
	}); err != nil {
		// A failed keepalive is not a session failure on its own: the next
		// caller frame or the next tick carries the clock forward, and the
		// read side reports a connection that has actually gone.
		return
	}
}

// stopFrameClock halts the gap filler.
func (client *Client) stopFrameClock() {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.frameTimer != nil {
		client.frameTimer.Stop()
		client.frameTimer = nil
	}
}
