# FIR filter component validation

Revision tested: `b23b9bec` (2026-09-23). Both paths use the same local
DeepFilterNet3 library/archive, hashed in `artifacts.json`.

Real-library Python checks: **11 passed**. Go client checks:
`/usr/local/go/bin/go test ./perception/noisefilter` passed.
Reproduction commands and optional deployment: [FIR.md](../../../../../tools/noisefilter/FIR.md).

Measured waveform delay with voiced probes: 672 samples at 16 kHz,
1,008 at 24 kHz, 2,016 at 48 kHz: **42.0 ms** each. Whole-buffer and
137-part input produced identical PCM. HTTP checks used all three rates,
including single-sample packets, and verified identity, sequence, delay,
output length and session deletion.

`resampler-frequency-response-20260923.json` is the OLD path with an identity
frame function: 6 kHz attenuation was 6.174 dB at 16 kHz. New isolated FIR
tests constrain 6 kHz attenuation below 0.1 dB, 12 kHz alias rejection above
70 dB when decimating to 16 kHz, and prove causal prefix/packet invariance.

`fir-processing-20260923.json` is a direct-stream CPU microbenchmark:
100 consecutive 100 ms packets of a synthetic voiced signal per rate/path.
Each path starts a fresh model state. Legacy then FIR order was fixed;
there is no randomized repetition. Setup/model allocation is excluded.
FIR median processing cost was 7.0–7.7 ms, maximum 12.7 ms. These are
unpaced component measurements, not HTTP/concurrency capacity evidence.

Recognition quality, quiet-backchannel preservation, and paced concurrent
HTTP performance remain unverified. Default profiles were not changed.

## Paced HTTP follow-up

`http-paced.json`: 30 seconds of deterministic white noise, one session,
24 kHz, 300 requests of 100 ms each, wall-clock paced. Observed identity
`deepfilternet-fir`, declared waveform delay 42 ms; HTTP p50 5.34 ms,
p99 8.13 ms, maximum 8.85 ms, 0 requests over 50 ms. Maximum send lateness
was 2.70 ms. This does not establish multi-session capacity or speech quality.
The benchmark now records delay from response headers instead of its old
hard-coded RNNoise value of 20 ms; the initial run with that bad report field
is excluded here. Command: `python3 tools/noisefilter/benchmark.py --url
http://127.0.0.1:9166 --realtime --seconds 30 --report <output>`.
