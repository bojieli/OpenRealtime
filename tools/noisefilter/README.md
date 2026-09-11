# Filtered room pipeline

For competing-voice suppression using an initial three-second user reference,
use the separate [target-room pipeline](../targetvoice/README.md). This page
describes the RNNoise noise-suppression variant.

`profile filtered-room` creates a separate, opt-in version of the existing
Deepgram → Qwen interaction → Gemini → Fish TTS room pipeline. Its graph is:

```text
PCM microphone → acoustic.NoiseFilter (RNNoise) → EnergyAdmission → Deepgram
                                                         ↓            ↓
                                                 filtered activity   partial/final
                                                         └──── Qwen ───┘
                                                                 ↓
                                                        Gemini → Fish → playback
```

The filter completes **before** EnergyAdmission sees a frame. Consequently,
neither acoustic interruption events nor ASR partial transcripts can be caused
by the unfiltered version of that frame. There is no parallel raw-audio bypass.
Qwen still receives partial transcripts; filtering does not wait for a final
transcript or for speaker enrollment.

## What it filters

RNNoise performs streaming neural noise suppression. This helps with stationary
noise and non-speech interference. This implementation uses the upstream default
model, pinned with the source revision in `build.sh`.

It is **not target-speaker extraction**. TV speech, another nearby person, and
the session user talking to somebody else can all survive a speech denoiser.
The existing speaker-embedding service and Qwen addressee/backchannel policy
remain responsible for those cases. Their existing enrollment and short-speech
limitations remain. Echo cancellation is also a separate capture-side task.
Do not describe this pipeline as Seeduplex-equivalent acoustic scene understanding.

Compared with the original room graph, the new profile also sets
`overlap_barge_in.unclassified=keep_speaking`. Unresolved acoustic overlap can
no longer cancel the agent merely because a timer expired. Semantically directed
interruptions still use the existing addressed cancellation path. This trades
some responsiveness to ambiguous interruptions for fewer false interruptions.

## Timing contract

- RNNoise processes 480 samples at 48 kHz: 10 ms of audio per model step.
- The sidecar accepts mono PCM16LE at 16, 24, or 48 kHz. It uses causal linear
  interpolation/block-average decimation at the lower rates. It does not buffer
  a whole utterance or repeatedly reset a resampler/model at packet boundaries.
- RNNoise's 10 ms algorithmic delay plus a fixed 10 ms packetization FIFO gives
  **20 ms waveform delay**, independent of incoming packet boundaries. The
  output has exactly the input's sample count. Capture positions are retained;
  acoustic content appears 20 ms later in that timeline.
- HTTP requests contain at most 100 ms of audio. The default processing deadline
  is **50 ms per ingress packet**, including transport and reading the full response.
  The configurable bound is 1–50 ms. Large ingress packets are processed in
  bounded chunks that share one deadline; batching cannot multiply the budget.
  Use 20–100 ms capture packets. Large offline uploads can exceed the budget
  and should be streamed in small packets.
- No filter error, timeout, mismatched sequence, or malformed response releases
  raw PCM. The graph fails explicitly; recovery requires a new session. This is
  an availability tradeoff, not an assurance that an overloaded OS will meet
  every deadline. There is no automatic audio retry against uncertain RNN state.
- Continuous microphone audio, including silence after speech, drains the delayed
  tail before automatic endpointing. The underlying scenario graph does not
  support manual `CommitAudio`; an offline source must include trailing silence.
- The graph's input depth is one frame. Existing gateway backpressure remains in
  place. Successful filtering is a causal prerequisite for each downstream partial;
  this does not mean total first-partial latency is unchanged from the baseline.

For the upstream framing and streaming delay, see the
[RNNoise example](https://github.com/xiph/rnnoise/blob/70f1d256acd4b34a572f999a05c87bf00b67730d/examples/rnnoise_demo.c).

## Build and run

The frontend needs Python 3.10+, a C toolchain, autoconf, automake, libtool,
make, Git, and wget. Its server has no third-party Python dependencies.
Builds and downloaded model weights live under `.runtime`.

From the repository root:

```bash
bash tools/noisefilter/build.sh
python3 tools/noisefilter/server.py \
  --library "$PWD/.runtime/rnnoise-filter/.libs/librnnoise.so"
```

The service binds to loopback port 8125 by default, warms the model before
announcing readiness, and exposes `GET /health`. Each graph session gets an
independent random session identity and RNN state. Requests must advance their
sequence exactly; changing sample rate mid-session is rejected. Sessions are
bounded to 32 by default and idle states are reclaimed lazily after 120 seconds.
The graph deletes its state on shutdown. Run behind an authenticated boundary
if relocating the service off this host; it deliberately defaults to loopback.

In another terminal, build and freeze the new pipeline with the same executable
that will serve it:

```bash
go build -o .runtime/openrealtime-filtered ./cmd/openrealtime
.runtime/openrealtime-filtered profile filtered-room \
  -out "$PWD/.runtime/filtered-room.launch.yaml" \
  -graph-out "$PWD/.runtime/filtered-room.graph.json" \
  -values-out "$PWD/.runtime/filtered-room.values.json"
.runtime/openrealtime-filtered serve \
  -launch-profile "$PWD/.runtime/filtered-room.launch.yaml"
```

Profile output files are create-only: choose new paths for a subsequent build.
The normal room dependencies must be running: Qwen at port 8000, Fish at 8123,
speaker embeddings at 8124, and word timing at 8003. The profile reads
`DEEPGRAM_API_KEY` and `GEMINI_API_KEY` from the environment. As with other frozen
profiles, use provider flags when these addresses differ. For example:

```bash
.runtime/openrealtime-filtered profile filtered-room \
  -noise-filter-url http://127.0.0.1:18125 \
  -noise-filter-timeout-ms 50 \
  -policy-url http://172.17.0.1:8000/v1 \
  -out "$PWD/.runtime/filtered-room-custom.launch.yaml"
```

The application readiness checks process a small silent frame through the real
filter service. Freeze/compile alone does not open cloud providers or prove that
the runtime services are available. The original `profile scenario`, room defaults,
and `openrealtime.deepgram.yaml` keep their original topology unless explicitly
configured with a noise filter.

## Verification

```bash
RNNOISE_LIBRARY="$PWD/.runtime/rnnoise-filter/.libs/librnnoise.so" \
  python3 -m unittest discover -s tools/noisefilter -v
python3 tools/noisefilter/benchmark.py --seconds 60 \
  --report .runtime/noisefilter-benchmark.json

go test ./perception/noisefilter ./elements/... \
  ./graph/binding/scenarioconversation ./graphs ./cmd/openrealtime
go test -race ./perception/noisefilter ./elements/acoustic \
  ./graph/binding/scenarioconversation
```

`benchmark.py` includes the HTTP round trip, reports every processing-deadline
miss, and exits nonzero on any miss. `--wav` and `--out-wav` accept/export mono
PCM16 WAV for listening and transcription checks. The noise-only attenuation
score is not a speech intelligibility or speaker-separation score.

An optional live streaming ASR comparison sends the same recording to Deepgram,
first raw and then filtered, at real time. It requires `websockets==16.0` and
uses the configured API key:

```bash
python3 tools/noisefilter/compare_deepgram.py \
  --wav recording-with-trailing-silence.wav \
  --report .runtime/noisefilter-deepgram.json
```

This diagnostic measures late requests without stopping them, so it can report
what would have failed the selected production deadline. It is not the production
failure policy. A single sequential raw/filtered comparison cannot establish
WER equivalence or an ASR latency improvement.

Go tests cover fail-closed processing, request sequencing, packet splitting,
immutable input bytes, graph ordering before downstream audio, causal provenance,
unchanged capture positions, frozen topology/values, schema registration, and a
Realtime gateway round trip through ASR, cognition, tools, synthesis and playback
with test providers. Python tests include actual RNNoise packet-invariance and
noise-attenuation checks when the library is available.

## Initial measurements (2026-09-09)

On this workspace host, using the default build and loopback service:

| Input/run | Requests | Filter round-trip p50 | p99 | Maximum | >30 ms |
|---|---:|---:|---:|---:|---:|
| 60 seconds deterministic white noise | 600 | 3.40 ms | 6.16 ms | 7.08 ms | 0 |
| 18 seconds recorded speech, isolated run | 180 | 3.45 ms | 13.74 ms | 24.63 ms | 0 |
| Same speech during concurrent compilation | 180 | 3.52 ms | 45.78 ms | 77.01 ms | 4 |

All used 100 ms packets and the initial 30 ms experimental budget, in addition
to the fixed 20 ms waveform delay. The shipped default is 50 ms, selected after
observing these tails; changing the deadline does not introduce a wait.
White-noise energy fell 42.64 dB after the first second. That synthetic result
must not be generalized to arbitrary environmental noise or overlapping voices.

One live Deepgram comparison returned `what's the capital of france` in both
conditions. The first nonempty result arrived at approximately 925 ms raw and
928 ms filtered. The diagnostic had five filter requests over 30 ms, maximum
39.35 ms, including asynchronous scheduling overhead. Those requests would fit the shipped
50 ms deadline. It establishes basic
speech preservation on one utterance; it does not establish production deadline
reliability. Re-run on the deployment machine under representative load before
relying on the selected deadline.

Further acoustic evaluation should cover fans, keyboard transients, TV/navigation,
echo, distant and overlapping speakers, whispered requests, brief genuine
interruptions, and multilingual speech. Target-speaker isolation before the first
partial requires an additional validated acoustic model/reference signal; neither
RNNoise nor the existing 1.5-second speaker comparison provides that capability.
