# OpenRealtime LiveKit agent

Joins a LiveKit room as an agent participant and proxies it to an OpenRealtime
endpoint.

This is a **separate Go module**, and deliberately so. It is an agent that
joins a room and speaks the protocol — a client, not a component. It can ship
on its own release cycle, be replaced by an equivalent for another RTC
provider, or be rewritten by somebody else entirely, and the server does not
need to know it exists.

Because it is separate, its dependency on the LiveKit SDK stays out of the
server's build. That SDK currently requires a newer Go toolchain than the
server does; keeping it here is what stops that from becoming everyone's
problem.

## Run it

```sh
export LIVEKIT_API_KEY=... LIVEKIT_API_SECRET=... OPENREALTIME_TOKEN=...
go run ./cmd/openrealtime-livekit \
  -livekit-url wss://your-project.livekit.cloud \
  -room demo \
  -endpoint ws://127.0.0.1:8765/v1/realtime
```

The agent joins `demo`, subscribes to every participant's audio, and publishes
its own track. Anyone else in the room talks to the agent.

## What it does

| Direction | Path |
| --- | --- |
| Participant audio | Opus track → decoded to 8 kHz → mu-law → `input_audio_buffer.append` |
| Agent audio | `response.output_audio.delta` → 20 ms mu-law packets → published track |
| Participant events | room data packet → protocol event, unchanged |
| Server events | protocol event → room data packet, unchanged |

Opus decoding is pure Go, so the agent stays a static binary with no codec
library and no cgo.

Protocol video events sent by a custom participant also cross the data-packet
row unchanged. Ordinary LiveKit camera and screen-share tracks do not: this
agent currently subscribes only to audio tracks and has no VP8/H.264-to-JPEG
decoder. A stock meeting room is therefore audio-only to the agent until the
publisher sends retained frames as
`openrealtime.input_video_frame.append` events or a video-track bridge is
deployed. Do not infer video-track support from the generic data path.

## Rules it follows

The same two that govern the in-process WebRTC adapter:

1. **It is a protocol client.** It connects over a real WebSocket and speaks
   the events any other client speaks. It has no privileged path into the
   server.
2. **It expresses nothing a plain client cannot.** It forwards participant
   events unchanged and originates only `session.update` and
   `input_audio_buffer.append`, both of which any client can send.

The one thing it changes is the session's audio format, and only because it
terminates media: a participant's audio travels on a LiveKit track, so its
opinion about the protocol connection's audio format would break the media path
without meaning anything. That single field is dropped; everything else in the
same event survives.

## Clean audio is the publisher's job

Echo cancellation and noise suppression happen before audio reaches the room.
The agent does neither and does not compensate for their absence — a browser
publishing into a LiveKit room already does both well, and a server guessing at
unknown client-side processing would make it worse.

## What is verified here

The protocol behaviour, the mu-law encoding, and the configuration contract are
covered by tests in this module. The room join, track subscription, and
publication paths need a live LiveKit server and are exercised by running the
command against one — there is no useful way to fake an SFU.
