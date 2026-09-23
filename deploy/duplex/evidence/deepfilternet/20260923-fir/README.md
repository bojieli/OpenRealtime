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

Four-session follow-up (`concurrency4/`): 1,200 requests total, one exceeded
50 ms (89.76 ms). One benchmark process correctly exited 1; three exited 0.
Per-session p99 ranged 8.85–9.51 ms. This fails a zero-deadline-miss
four-session gate despite low typical latency. The reports do not retain
per-request timestamps, so the outlier's position/cause is not established.

Traced repeat (`concurrency4-traced/`): three deadline misses, all at
sequence 0 or 1. Two first requests took about 89 ms round trip and
86.5 ms server time; one second request took 55.5/54.6 ms. The service's
prebuilt-state pool defaults to two, and pool exhaustion calls `df_create`
on the request path. This is a startup bottleneck candidate, not a measured
allocation attribution. Per-request timings are retained; failures are not
excluded as warmup. Next comparison should provision four fresh states and
retain all first packets in the scoring population.

Four-prebuilt-state comparison (`pool4/`): `--state-pool 4`, four independently
paced 30-second sessions, all 1,200 requests scored including startup.
Zero 50 ms misses; first requests 10.10–11.28 ms, overall maximum 24.25 ms.
This supports pool exhaustion as a startup bottleneck, but is not a repeated,
randomized capacity campaign. Shared-host load and request phasing can differ.
The previous failed runs remain retained above.

## Quiet-speech regression — do not promote

`quiet-clean.json` reuses the 20 user-backchannel fixture IDs from the earlier
`denoise-quiet.json`. Full recordings are polyphase-resampled to 16 kHz,
scaled by 0 or -30 dB, rounded to PCM16, and replayed unpaced in 100 ms
packets through fresh real model states. A 100 ms silence tail is appended.
Energy windows are aligned by the respective 40/42 ms delays. No ASR was run.

Mean per-clip retention at original level: legacy -0.646 dB, FIR -0.168 dB.
At -30 dB input scaling: legacy -19.963 dB, FIR -98.522 dB. Twelve of twenty
FIR clips lose over 60 dB, versus two legacy clips. The calculation floors
power at 1e-12; very negative values include effectively silent PCM, not
meaningful acoustic precision. Root cause is unproven. Do not promote FIR
to defaults based on passband/latency results; inspect low-level PCM, model
behavior, scaling, and noise-floor differences before any quality claim.

## Quiet-speech cause: the model's local-SNR gate, not the FIR conversion

`quiet_diagnose.py` (in `tools/noisefilter/`) repeats the probe above with
libDF's per-frame local SNR (`df_process_frame`'s return value) and a count
of exactly-zero 10 ms output frames. It measures retention over the **full
recording** rather than the backchannel window, so failing clips read tens
of dB instead of about -160 dB. The same 12 clips fail: each has over 95% of
its FIR output frames exactly zero. Repeat runs are bit-identical
(`quiet-lsnr-input-repeat.json`, maximum difference 0.0 dB).

| input.wav, -30 dB | mean retention | clips >60 dB lost |
| --- | ---: | ---: |
| legacy stream | -7.35 dB | 0 |
| FIR stream | -43.14 dB | 4 |
| offline `resample_poly` to 48 kHz, fed straight to the model (`native48`) | -40.88 dB | 3 |
| FIR with ±1 LSB TPDF dither before PCM16 rounding | -43.24 dB | 4 |

At original level all four variants retain within 0.6 dB (`quiet-lsnr-input.json`;
`quiet-lsnr-clean-input.json` gives the same picture for `clean_input.wav`).

What this establishes:

- Whenever a clip's maximum per-frame local SNR stays below about -10 dB, the
  model zeroes every output frame (clips 2, 43 and 67 reach at most about -11 dB
  in the FIR path and are 100% zero). The gate acts per frame: clip 22 reaches
  +22 dB on a few frames and is still 99.6% zero. Audible speech frames at the
  original level read up to about +35 dB.
- A correct offline conversion (`native48`) fails like FIR. The FIR stream is
  not corrupting the signal; digital zeros are not the trigger (dither changes
  nothing).
- The legacy path's higher local SNR on the same clips (for example 43: +32 dB
  legacy versus -11.5 dB FIR) coincides with its linear-interpolation images
  above 8 kHz. Legacy "preservation" therefore depends on a conversion artifact
  and is not a property to restore.
- Bounding attenuation with `atten_lim_db=30` (`quiet-lsnr-clean-input-atten30.json`)
  removes the >60 dB collapses but still loses 18.2 dB mean for FIR at -30 dB.
  That is a cap, not preservation.

Not established: recognition impact, which clips a real user produces at such
levels, and whether an input-level normalisation stage would be preferable to
an attenuation limit. Neither path is a quiet-speech-safe default. FIR remains
unpromoted; the regression is a property of DeepFilterNet3 on band-limited
low-level input that the legacy path masks.
