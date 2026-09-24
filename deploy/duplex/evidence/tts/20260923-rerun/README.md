# TTS component rerun, 2026-09-23 (retained reports)

The earlier `results/tts/` component reports were lost in the 10:16 UTC
cleanup. This rerun uses `tools/duplexmodels/ttsbench.py --phases
complete,incremental,cancel` against the local synthesis services still
running: Kyutai TTS 1.6B (en), Qwen3-TTS 0.6B (en, zh) and VibeVoice-Realtime
0.5B (en). The runs were sequential, so they did not contend with each other.
The host was heavily loaded (load 38–51 on 32 CPUs), which inflates real-time
factors. `SUMMARY.md` is the tool's own table; the `*.json.gz` files are the
full reports. Synthesized audio was not retained; the scores are in the reports.

- Intelligibility was scored afterwards (`--phases score`) with a
  temporarily started Qwen3-ASR service. Complete text: WER 0.000 for all
  English services, CER 0.004 for Qwen3 Mandarin. Held-back and
  word-by-word outputs: WER 0.000.
- CosyVoice 3 0.5B was measured on 2026-09-24 once its environment was
  rebuilt and a fresh service was running. That service was restarted outside
  this session, and the earlier HTTP 500 was the lost venv. The host load was
  about 60, and the gated PersonaPlex campaign shared the GPU. Complete-text
  RTF was 1.49 (en) and 2.39 (zh), so slower than real time here. Audio came
  before the held-back rest on 4/6 (en) and 2/6 (zh) sentences, nothing after
  cancel, and WER 0.000 / CER 0.007.
- Deepgram Aura 2 (`aura-2-thalia-en`, :9126 service, authorized) was run later: complete-text TTFA p50 0.325 s, RTF 0.44, WER 0.003; audio before the held-back rest on 6/6 sentences; 0 frames after cancel.

Held-back prefix (first half appended, 1.5 s wait): Qwen3-TTS produced audio
before the rest on 6/6 English and 6/6 Mandarin sentences, and Kyutai on 4/6.
This agrees with the earlier component report's Qwen3 6/6 noted in the
results document. The results table's 0/4 for Qwen3 comes from a different
probe and run (`tools/ttsprobe`). Complete-text RTF means: Kyutai 0.28,
VibeVoice 0.28, Qwen3 1.26 (en) and 0.86 (zh). Qwen3 English was slower than
real time under this load.
