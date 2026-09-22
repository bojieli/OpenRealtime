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
| 9160 | `services/audio-observer.sh` | Audio Flamingo 3, bounded observer off the critical path |

Recognisers speak the start/chunk/finish contract with a committed prefix, and
synthesisers `openrealtime-incremental-speech/1`; both are specified in the
module docstring of `tools/duplexmodels/common.py`.

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
| `microturn-voxtral-qwen3-deepgram` | synthesiser substitution (C4), closed |
| `cascade-voxtral-qwen3-kyutai`, `cascade-nemotron-qwen3-kyutai` | the engine-floor cascade with the same components |
| `cascade-…-smartturn-observe`, `cascade-…-smartturn-control` | endpoint evidence observed versus controlling (I0) |
| `closed-gemini-live`, `closed-openai-live` | closed live references (R0); the voice path leaves the machine |

## Running the end-to-end check

```bash
deploy/duplex/run-e2e.sh microturn-voxtral-qwen3-kyutai 10 12
deploy/duplex/run-matrix.sh 8 8 profile-a profile-b …      # sequentially
python tools/duplexmodels/e2e_summary.py
```

`run-e2e.sh` starts the profile on a private port, drives a stratified FDB v1.5
subset (the same number of recordings from each category) and an FD-Bench slice
through the public Realtime protocol, and stops the server. It records the
revision, profile digest, host load average and GPU memory with every run,
because a latency number measured on a loaded machine has to say so. A subset
is a smoke measurement: the bench itself reports NOT REPORTABLE for anything
short of a full campaign, and so should any summary of it.
