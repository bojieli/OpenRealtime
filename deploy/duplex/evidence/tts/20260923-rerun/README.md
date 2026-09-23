# TTS component rerun, 2026-09-23 (retained reports)

The earlier `results/tts/` component reports were lost in the 10:16 UTC
cleanup. This rerun uses `tools/duplexmodels/ttsbench.py --phases
complete,incremental,cancel` against the local synthesis services still
running: Kyutai TTS 1.6B (en), Qwen3-TTS 0.6B (en, zh) and VibeVoice-Realtime
0.5B (en). The runs were sequential, so they did not contend with each other.
The host was heavily loaded (load 38–51 on 32 CPUs), which inflates real-time
factors. `SUMMARY.md` is the tool's own table; the `*.json.gz` files are the
full reports. Synthesized audio was not retained.

- Not scored for intelligibility: the `score` phase needs the Qwen3-ASR
  service, which was not running.
- CosyVoice was not measured: its long-running service answered `/health`
  with HTTP 500 (`cosyvoice-health-failure.log`). Restarting it would
  require a model load, which host memory did not allow.
- Deepgram Aura was not rerun: it is a billed external API.

Held-back prefix (first half appended, 1.5 s wait): Qwen3-TTS produced audio
before the rest on 6/6 English and 6/6 Mandarin sentences, and Kyutai on 4/6.
This agrees with the earlier component report's Qwen3 6/6 noted in the
results document. The results table's 0/4 for Qwen3 comes from a different
probe and run (`tools/ttsprobe`). Complete-text RTF means: Kyutai 0.28,
VibeVoice 0.28, Qwen3 1.26 (en) and 0.86 (zh). Qwen3 English was slower than
real time under this load.
