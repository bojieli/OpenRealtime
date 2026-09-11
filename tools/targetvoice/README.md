# Target voice room pipeline

**Experimental; not validated for live use.** Subsequent [end-to-end diagnostics](E2E_RESULTS.md)
found competing-speech transcription leakage. Bounded timeout recovery now
keeps the graph alive through isolated late packets, but a playback diagnostic
using Qwen in place of inaccessible Gemini still observed a false interruption.
The current extractor is not a reliable target-presence gate. The isolated
measurements below do not establish conversational reliability.

`profile target-room` is an opt-in pipeline for suppressing **competing speech
in microphone audio before Deepgram**. It retains Deepgram Nova-3 → Qwen-fast
interaction policy → Gemini → Fish TTS. It uses the direct-visual architecture
without the old per-utterance speaker identity service.

```text
Microphone PCM → initial-reference / target extraction → EnergyAdmission → Deepgram
                                                                          ↓
                                                                  Qwen → Gemini → TTS
```

The audio filter element's `model=real-tse` selects actual target-speaker
extraction. `profile filtered-room` still selects RNNoise and is a different
noise-suppression experiment; RNNoise does not remove overlapping speakers.

## Enrollment and partial transcripts

1. A session-local Silero VAD detects the first speech in 32 ms frames.
2. Capture the first **three seconds from that detected onset**, including any
   pauses in that window. Assume this initial voice is the intended user and
   the window is clean. No speaker comparison is performed.
3. Encode that recording once on a CPU worker, using the extraction checkpoint's
   own WeSpeaker ECAPA encoder. Its representation is not interchangeable with
   the project's existing SpeechBrain speaker-ID service.
4. Keep the reference fixed for the session. Run the causal separator on later
   microphone audio with persistent recurrent state. Later voices cannot replace
   the reference. A new session starts a new enrollment.

Audio continues through the fixed FIFO during collection and encoding: **ASR
never waits three seconds for enrollment**. Competing speech is not suppressed
until the response state becomes `extracting`. The response header
`X-Target-Voice-State` reports `waiting-for-speech`, `collecting-reference`,
`preparing-reference`, or `extracting`. The Go client rejects missing or regressing
states, including any attempt to return to unfiltered enrollment after activation.

This assumes the user speaks first and is alone for the enrollment window. The
system does not establish that the reference is clean. A short greeting followed
by another person within that window can contaminate it. The reference remains
in memory for the session; it is not written as an enrollment WAV by the server.

## Timing

The streaming adapter maintains STFT overlap, synthesis overlap, and all six
temporal LSTM states. It does not re-run a growing audio history or reset the
separator between utterances. It batches only audio already in the request.

- Model rate: **16 kHz**; processing blocks: **32 ms**.
- Fixed output FIFO: **64 ms**; advertised delay bound: **65 ms**, including
  resampling for 24/48 kHz microphone streams.
- Delivery deadline: **50 ms per incoming graph packet**, including all
  subrequests if a packet exceeds 100 ms. A late packet is replaced with
  same-length silence. A single ordered worker finishes its state update; late
  audio is discarded, never replayed in a later packet. There is no raw fallback.
- Outstanding audio and each job's lifetime are bounded at **500 ms**; the queue
  also has an eight-packet cap. A protocol failure, exhausted backlog, or eight
  consecutive delivery misses latches `degraded` and mutes subsequent input
  until a new session. This preserves the graph but makes user speech unavailable.
  It does not silently re-enroll. `Client.Status()` reports delivery state,
  late-packet count, muted duration, and reason; graph state transitions are
  logged. An on-time result after an isolated miss restores `healthy`. These
  states describe delivery, not confidence that the target speaker is present.
- The filter runs before both acoustic admission and Deepgram. Post-enrollment
  partial transcripts can therefore only use audio that has passed extraction.

Capture must continue through trailing silence to drain the FIFO; abruptly
stopping microphone delivery can leave the final audio buffered.

The 50 ms processing budget and 65 ms audio delay are separate quantities. This
is not a guarantee of less than 50 ms total added latency. VAD and reference
encoding do not add a recurring 1.5-second collection wait. A CPU-only test under concurrent conversation load exceeded the budget even
with four threads (96 ms audio: median 68.1 ms, maximum 75.2 ms, 20 samples).
Concurrent-session throughput has not been qualified against this budget.
The default maximum is four sessions; it is a resource cap, not a concurrency
performance guarantee.

Native 16 kHz avoids resampling. Other supported rates use causal linear
interpolation and block averaging through a 48 kHz clock, retaining resampler
state across packets. This is a simple conversion, not a high-quality polyphase
resampler; speech content above 8 kHz is not recovered by this model.

## Run

The implementation was tested with Python 3.10, PyTorch/Torchaudio 2.10.0+cu128,
Silero VAD 6.2.1, ONNX Runtime 1.23.2, NumPy 2.2.6, SciPy 1.15.3, SoundFile
0.13.1, and Requests 2.33.1. Use an environment with CUDA-compatible PyTorch,
Torchaudio, and these Python dependencies installed. Git is needed by setup.

From the repository root:

```bash
python3 tools/targetvoice/setup.py
python3 tools/targetvoice/server.py --port 8126
```

Setup pins both upstream source revisions and verifies the checkpoint SHA-256.
The official download is a roughly 1 GB archive; setup extracts only the causal
speaker-embedding checkpoint. `--archive /path/to/real-tse-ckpt.zip` reuses a
previous download. Model files and source dependencies live in `.runtime/targetvoice`.
The default bind address is loopback. Weights, GPU kernels, and VAD sessions are
warmed before the server reports readiness.

Freeze the new profile with the project's existing provider configuration:

```bash
go build -o .runtime/openrealtime-target ./cmd/openrealtime
.runtime/openrealtime-target profile target-room \
  -out "$PWD/.runtime/target-room.launch.yaml" \
  -graph-out "$PWD/.runtime/target-room.graph.json" \
  -values-out "$PWD/.runtime/target-room.values.json"
```

The profile defaults to `http://127.0.0.1:8126`; `-noise-filter-url` can select
another local service address. The normal launch workflow and provider
credentials still apply. Freeze outputs must use new paths if they already
exist. The original room and RNNoise profiles remain independently selectable.

## Verification and measured limits

```bash
python3 -m unittest tools.targetvoice.test_server
TARGETVOICE_RUNTIME=.runtime/targetvoice python3 -m unittest tools.targetvoice.test_model
go test ./perception/noisefilter ./elements/acoustic ./graphs ./cmd/openrealtime

python3 tools/targetvoice/benchmark.py \
  --target /path/to/clean-user.wav \
  --competitor /path/to/clean-other-person.wav \
  --rate 24000 --out .runtime/targetvoice-evaluation
```

The benchmark needs two clean mono recordings of different people, at least
seven seconds each. It sends one second of leading silence, four seconds of the
user alone, then overlapping speech. It records state transitions, HTTP timing,
mixture/filtered/target WAVs, and SI-SDR on the later overlapping segment.
It exits unsuccessfully if enrollment misses the overlap or any packet exceeds
the 50 ms processing budget. Audio is sent only to the specified filter service.

Measurements on this machine (RTX PRO 6000 Blackwell, one active filter session):

| Check | Result |
|---|---:|
| 24 kHz HTTP, 100 ms packets, 120 requests | median 11.15 ms; p99 12.42 ms; max 22.41 ms |
| Requests over 50 ms in that run | 0 |
| Automatic enrollment | first detected speech at 1.0 s; extraction at 4.0 s (100 ms reporting granularity) |
| Held-out overlapping segment, input → extracted SI-SDR | 0.23 → 5.77 dB |
| Stateful PCM vs full-waveform inference, three chunk sizes | maximum absolute difference below 0.000008 |

A separate isolated 16 kHz model experiment using a three-second reference and
held-out speech improved SI-SDR from 0.05 to 14.50 dB. Swapping the reference
selected the other speaker (14.29 dB); an absent-target example attenuated output
energy by 31.47 dB. These are small synthetic tests using the two enrollment WAVs
from [OpenSpeakerBeam-SS's sample directory](https://github.com/helloooideeeeea/OpenSpeakerBeam-SS/tree/main/data/sample),
not broad performance claims. The HTTP result above is the more representative
automatic-enrollment measurement. Before batching existing packet audio into
one GPU call, a test exceeded the 50 ms deadline twice; that version is not the
final implementation.

A live sequential Deepgram check on the 24 kHz mixture returned the first partial
at 2.121 s raw and 1.983 s filtered, measured from stream start (including one
second of leading silence). Both arrived before enrollment completed. Filter
round trips peaked at 25.79 ms with no deadline misses. Cloud variability means
this does **not** establish that filtering speeds up ASR. No word-error-rate
claim is made without an independently transcribed target reference.

To reproduce the opt-in cloud check (sends audio to Deepgram):

```bash
python3 tools/noisefilter/compare_deepgram.py \
  --wav .runtime/targetvoice-evaluation/mixture.wav \
  --filter-url http://127.0.0.1:8126 \
  --report .runtime/targetvoice-deepgram.json
```

This needs `DEEPGRAM_API_KEY` and websockets 16. Real room recordings, similar
voices, target-absent speech, and concurrent TTS/LLM load still need a larger
acceptance set before enabling the profile by default. Background speech can
still leak through, and the user's speech can be distorted. This implementation
is target extraction, not a reproduction of Seeduplex's full interference policy.

## Model provenance

- [REAL-TSE WeSep baseline](https://github.com/REAL-TSE/wesep-real-tse), revision
  `2a540977a348fbaa92e623210505430e2cec608d`, `spk_emb_causal_100` checkpoint.
- [WeSpeaker](https://github.com/wenet-e2e/wespeaker), revision
  `8f53b6485d9f88a207bd17e7f8dba899495ec794`.
- Checkpoint SHA-256:
  `42b73219eefefcbba4b5da0dcb89dce292359c9f93b1a951c7b6d196da2155b3`.

Upstream source licenses are retained beside the installed source. The setup
script disables only the copied packages' eager CLI imports; neural module
source remains pinned and unmodified. Checkpoint loading uses
`torch.load(weights_only=True)` and strict state-dictionary matching.
