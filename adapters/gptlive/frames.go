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

// fillFrameGap sends one frame of silence if the caller has genuinely stopped.
//
// The test is how long it has been since the caller's last frame, and it must
// be, because "did anything arrive since the last tick" is wrong in the case
// that matters most. A caller streaming at the frame interval runs its own
// clock beside this one; the two drift, so in any given tick its frame may not
// have landed yet. Answering "nothing arrived" there injects silence into the
// middle of somebody's sentence - the stream reaching the endpoint is then the
// user's speech chopped at twenty-millisecond boundaries and stretched past
// real time, which is exactly what a full-duplex model must not be given. It
// cost this integration a barge-in: measured against the real endpoint, a
// model that handles interruption natively took sixteen seconds to yield
// because what it was hearing had been cut to pieces on the way in.
//
// So the gap has to be longer than the jitter of a caller that is still
// talking, and shorter than a pause worth filling.
//
// There is no check that the session has started. The clock is armed by the
// session.started handler and nowhere else, so it cannot run before the
// handshake; a guard for that would be a branch no test could reach.
func (client *Client) fillFrameGap(ctx context.Context) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.config.Scheduler.NowNS()-client.lastCallerNS < uint64(client.idleGap()) {
		// The caller is streaming. Its audio is the clock, and padding a live
		// stream would displace real speech on the endpoint's timeline.
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

// idleGap is how long the caller must be quiet before this side speaks for it.
// Three frame intervals, and never less than 60 ms: past any scheduling jitter
// a caller that is still streaming can produce, and short enough that a real
// pause is carried before the endpoint's clock notices.
func (client *Client) idleGap() time.Duration {
	return max(3*client.config.FrameInterval, 60*time.Millisecond)
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
