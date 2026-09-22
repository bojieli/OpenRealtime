# Duplex model deployment

The services, profiles and runner behind the
[streaming and full-duplex plan](../../docs/full-duplex-streaming-plan.md).
Measured outcomes are in [the results record](../../docs/full-duplex-results.md);
this page is how to run them.

Nothing here downloads weights implicitly. Each service script names the exact
Hugging Face repository it needs, and `manifest.json` pins the revision,
size and license of every asset the profiles use:

```bash
python tools/duplexmodels/manifest.py --out deploy/duplex/manifest.json
```

## Services

Each script takes `start`, `stop` or `status`, logs to
`.runtime/duplex-plan/logs/<name>.log`, and records its PID under
`.runtime/duplex-plan/pids/`.

| Port | Script | What it serves |
| --- | --- | --- |
| 9100 | `services/llm-qwen3-8b.sh` | Qwen3-8B on vLLM: cascade foreground/background model and micro-turn controller |
| 9101 | `services/asr-voxtral.sh` | Voxtral Mini 4B Realtime on vLLM's `/v1/realtime` |
| 9102 | `services/asr-qwen3.sh` | Qwen3-ASR 0.6B, the start/chunk/finish baseline |
| 9110, 9111 | `services/nemotron-en.sh`, `services/nemotron-multi.sh` | Nemotron cache-aware streaming RNN-T, English and multilingual |
| 9112 | `services/kyutai-stt.sh` | Kyutai STT 1B |
| 9113 | `tools/duplexmodels/asr_funasr.py` | FunASR streaming Paraformer (Mandarin) |
| 9120, 9121, 9123, 9124 | `services/tts-*.sh` | CosyVoice 3, VibeVoice-Realtime, Qwen3-TTS, Fish S2 Pro |
| 9125 | `services/kyutai-tts.sh` | Kyutai TTS 1.6B |
| 9126 | `tools/duplexmodels/tts_deepgram.py` | Deepgram Aura streaming WebSocket (closed comparison) |
| 9130 | `services/turn.sh` | Smart Turn, LiveKit turn detector, VAP, DualTurn |
| 9140-9159 | `services/{voicechat,minicpm-o-duplex,lychee-fd,freeze-omni}.sh` | native duplex models |
| 9146 | `services/personaplex.sh` | PersonaPlex 7B, pinned upstream runtime and NATF2 voice; session instructions supply the role prompt |
| 9148 | `services/moshi.sh` | Moshi 7B, pinned checkpoint/tokenizer/codec; no instruction input |
| 9160 | `services/audio-observer.sh` | Audio Flamingo 3, bounded observer off the critical path |

Recognisers speak the start/chunk/finish contract with a committed prefix, and
synthesisers `openrealtime-incremental-speech/1`; both are specified in the
module docstring of `tools/duplexmodels/common.py`.

Each family has its own virtual environment under
`.runtime/duplex-plan/venvs/` (`nemo`, `kyutai`, `cosyvoice`, `vibevoice`,
`qwen3tts`, `turn`, `perception`, `freezeomni`, `lychee`, `minicpm-duplex`,
`microturn`, ...), because their pinned stacks conflict: several upstream
projects pin a PyTorch with no sm_120 kernels, and each service script names
the interpreter it needs. Every native sidecar also runs with `--mock`, which
speaks the protocol without weights - that is how `openrealtime conformance
sidecar` checks the plumbing before a GPU is involved.

## Sharing one GPU

This is one 96 GB card. Small services (under 8 GB) run side by side; anything
larger takes the single large-model lease so two 20 GB models never load at
once:

```bash
setsid nohup flock -w 14400 .runtime/duplex-plan/gpu/large.lock <command> > log 2>&1 &
```

The lease is held exactly while the process runs. Check free memory before
loading (`nvidia-smi --query-gpu=memory.used --format=csv`), and stop a service
when its measurements are done. `stop` kills the whole process group, because
vLLM's engine core outlives an API server that is signalled alone and keeps its
memory.

## Profiles

Each file in `profiles/` is an ordinary `openrealtime serve -config` file and
one experiment cell:

| Profile | Cell |
| --- | --- |
| `microturn-voxtral-qwen3-kyutai` | the micro-turn cascade: streaming ASR, a 500 ms controller, incremental TTS |
| `microturn-clock-only` | the same without acoustic evidence or between-tick decisions: what the clock's phase costs |
| `microturn-nemotron-qwen3-kyutai`, `microturn-kyutaistt-qwen3-kyutai` | recogniser substitution (C3) |
| `microturn-voxtral-qwen3-{vibevoice,cosyvoice,deepgram}` | synthesiser substitution (C4); the last one is closed |
| `cascade-voxtral-qwen3-kyutai`, `cascade-nemotron-qwen3-kyutai` | the engine-floor cascade with the same components |
| `cascade-nemotron-qwen3-fish` | Fish S2 Pro sentence-input synthesis substitution; public smoke measured, full acceptance pending |
| `cascade-…-smartturn-observe` | endpoint evidence recorded but deciding nothing (I0) |
| `cascade-…-{smartturn,livekit,vap,dualturn,fusion}-control` | one endpoint predictor deciding the pause, at its calibrated threshold (I0) |
| `native-freeze-omni` | a native duplex model through the sidecar relay (P5/P8) |
| `native-voicechat` | experimental VoiceChat duplex and tool cell (N2); acceptance in progress |
| `closed-gemini-live`, `closed-openai-live` | closed live references (R0); the voice path leaves the machine |

## Running the end-to-end check

VoiceChat uses a separate source pin because vLLM-Omni `9ebef4b` loads its
weights but disables its legacy duplex endpoint. Prepare the compatible
`9005d789033b8c3ec5876a7a68c4e2d9238f5c69` source with
`services/setup-voicechat-source.sh` before `services/voicechat.sh start`.
The launcher uses the existing `vllm-omni-native` environment and sets
`PYTHONPATH` for this source only. Its talker reserves 2 GiB of KV cache for
8,192 positions (about ten minutes at 80 ms per frame); longer sessions need
a separately validated larger budget. A successful `/health` response is
only startup evidence; the native sidecar and tool tests establish duplex
operation.

The retained native drivers are `tools/duplexmodels/native_probe.py` (sidecar
protocol) and `tools/duplexmodels/realtime_probe.py` (public Realtime socket).
Run them with the model's environment. For example, after VoiceChat is ready:

```bash
.runtime/duplex-plan/venvs/vllm-omni-native/bin/python tools/duplexmodels/native_probe.py \
  --sidecar '.runtime/duplex-plan/venvs/vllm-omni-native/bin/python sidecars/voicechat_sidecar.py --server ws://127.0.0.1:9140/v1/realtime' \
  --protocol 3 --scenario question \
  --question .runtime/full-duplex-bench-v1.5/dataset/user_interruption/1/context.wav \
  --duration 30 --out .runtime/duplex-plan/results/native/question-new.json
```

Use a new output basename for each attempt. The native driver writes the
summary, received-event trace and audio beside it. If `--event-log` is passed
to the sidecar, choose a different filename from the driver's
`<basename>.events.jsonl`, which contains the engine-facing trace. These
drivers record packet arrival timing and do not simulate rendered-playback
acknowledgments. The `tool` scenario supports `--tools`, `--tool-delay` and
`--tool-output`; the `interrupt` scenario accepts a second recorded utterance.

```bash
deploy/duplex/run-e2e.sh microturn-voxtral-qwen3-kyutai 10 12
deploy/duplex/run-matrix.sh 8 8 profile-a profile-b …      # sequentially
python tools/duplexmodels/e2e_summary.py
```

`run-e2e.sh` starts the profile on a private port, drives a stratified FDB v1.5
subset (the same number of recordings from each category) and an FD-Bench slice
through the public Realtime protocol, and stops the server. It records the
revision, profile digest, host load average and GPU memory with every run,
because a latency number measured on a loaded machine has to say so.
It also snapshots `/health` for loopback component URLs, retaining settings such
as Fish's compilation mode and sample rate. Missing or invalid health responses
are recorded as errors in that snapshot; they do not certify a component. A subset
is a smoke measurement: the bench itself reports NOT REPORTABLE for anything
short of a full campaign, and so should any summary of it.

Each attempt is retained under `results/e2e/<profile>/runs/<run-id>/`; the
`latest` symlink identifies the latest attempt, including failed attempts.
The runner snapshots the profile, hashes the executable and results, verifies
that its own server owns the listening socket, and records command exit codes
and task errors. Failed commands, startup failures, and task errors produce a
nonzero exit status. The summary excludes incomplete, changed, failed, and
legacy runs whose provenance cannot be verified. Earlier artifacts remain
available for inspection but need a new run to enter this summary.

### Released DuplexCascade profile (experimental)

`native-duplexcascade.yaml` uses the pinned released checkpoint, the native
500 ms micro-turn backend, append-only Kyutai ASR fragments, and persistent
Kyutai synthesis. It requires ASR on :9112 and TTS on :9125. The profile connects to a warmed sidecar on :9147. Start it with `deploy/duplex/services/duplexcascade.sh start`; the launcher pins
the source and both checkpoints and holds the large-model lease.
It checks for 20,000 MiB free before loading; subsequent sequential sessions
reuse the model and each receive a fresh micro-turn history.

Output PCM is paced in 20 ms frames with a bounded 30 s queue. Model-directed
cancellation discards unsent audio. Output turn boundaries use the model's
thinking state plus a drained delivery buffer and 600 ms without a synthesis
packet. This is an adapter policy, not a native terminal token or rendered
playback acknowledgement. The released protocol does not consume session
instructions or injected background text. Public Realtime validation is still
pending; component probe results do not establish profile acceptance.

### Moshi native reference

`services/moshi.sh start` loads `kyutai/moshiko-pytorch-bf16` revision
`2bfc9ae6e89079a5cc7ed2a68436010d91a3d289` in the existing Kyutai environment
and serves sequential sessions on :9148. The launcher requires 22,000 MiB free
and takes the large-model lease without queuing. `native-moshi.yaml` connects
to this service. Model, codec and tokenizer use the same explicit Hub revision.
Moshi has no session instruction or injected-text channel. Output turn
boundaries are the adapter's RMS/hangover policy. Existing component smoke
results do not establish acceptance of this deployment profile; a new public
run remains required.
