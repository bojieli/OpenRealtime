# Full-duplex implementation handoff — 2026-09-23

Prepared at approximately 08:00 UTC at the user's request to wrap up and
continue later. The full goal is **incomplete**. This document supplements
[the roadmap](full-duplex-streaming-plan.md) and
[the results record](full-duplex-results.md); it does not narrow their scope.

## First action on resumption

Inspect the existing PersonaPlex campaign before starting anything else.
It was live at handoff, with conversation **233/293 started**, not completed.
Do not restart merely because a tool handle or observation times out.

```bash
cd /home/ubuntu/OpenRealtime
ps -p 3810341,3808281 -o pid,etime,stat
tail -n 3 .runtime/duplex-plan/results/e2e/native-personaplex/latest/fdbench.log
ls .runtime/duplex-plan/results/e2e/native-personaplex/latest/{fdbench,finished}.json
df -h .runtime
```

- Runner PID **3810341**, tool session **75798**; backend PID **3808281**,
  launcher/flock **3808278**, port **9146**, holds the large-model lease.
- Run: `.runtime/duplex-plan/results/e2e/native-personaplex/runs/20260923T012919Z-td0w3l3o`.
- Running checkout `/home/ubuntu/OpenRealtime-duplex-next-validation`, pinned
  **1d221b9b**. Do not change it while the campaign runs.
- Binary `.runtime/duplex-plan/bin/openrealtime-68c15b3e`, SHA256
  `32db2bfc648c81d729e2712b6802bfa8dd5dea733c35014a0dfa629450966b58`.
- Command: `OPENREALTIME_BIN=/home/ubuntu/OpenRealtime/.runtime/duplex-plan/bin/openrealtime-68c15b3e python3 tools/duplexmodels/e2e_run.py native-personaplex --full-selected`.
- Scope: all four FDB categories and **one** FD-Bench condition,
  `cosyvoice2-single-round-combine-med`, 293 conversations. Not all 21 conditions.
- PersonaPlex checkpoint `nvidia/personaplex-7b-v1@fdaf4090a61cb315c138a1faee287ffd6c716309`;
  upstream revision `3428dfd95309a7f3c84fd93259ded0f810d1ff91`; voice NATF2.
- Run `external-runtime.json` records provenance. Backend statistics:
  `.runtime/duplex-plan/results/native/personaplex-stats.jsonl`; filter PID 3808281.
- Resource sampler session **24830**, `resources-resumed.jsonl`, last observed
  sample **07:59:16 UTC**. Parse newline-terminated JSONL records only.
- No final `finished.json` at handoff. Leave the campaign running during pause.
  Backend may remain resident after runner termination; stop only after retaining
  final evidence and checking ownership. Do not stop unrelated Qwen3 on :9100.

## Results already established

PersonaPlex completed all 498 FDB recordings, zero task errors:

| Category | Passed/applicable | Not applicable |
| --- | ---: | ---: |
| interruption | 174/180 | 20 |
| backchannel | 88/88 | 10 |
| background speech | 54/57 | 43 |
| talking to other | 69/70 | 30 |

Interruption yield p50 440.229 ms, p90 709.7352 ms. N/A means no assistant
speech at the event; it is not a successful hold. Evidence is under
`deploy/duplex/evidence/native-personaplex/`. Scoring uses received audio,
not physical playback. Adapter boundaries use RMS/hangover, not native EOS.

VoiceChat completed the same FD condition: 1,089/1,382 answered, 293 missed,
66 premature, 852 overrun; 84/293 conversation passes, zero FD task errors.
FDB interruption: 73/138 applicable, 61 N/A, one task error. Final evidence
commit `db2dc2dc`; reconnect evidence `512c3c30`. See results document for
other models and component measurements. Do not treat smoke as acceptance.

## Ready follow-up work

1. On PersonaPlex termination, inspect command statuses, population, final JSON,
   digests, errors, statistics and resource gaps; retain final evidence, update
   results, commit and push. A successful process exit alone is insufficient.
2. Stop only the owned PersonaPlex service, then use the prepared clean checkout
   `/home/ubuntu/OpenRealtime-duplex-lifecycle-validation` at **f6f0a3a4**.
   It shares `.runtime`. CPU verification there: **21 tests + 2 subtests passed**.
   Preflight: `.runtime/duplex-plan/results/native/personaplex-followup-preflight.json`.
3. Run real GPU reconnect and matched readiness comparisons. The standalone
   `tools/duplexmodels/realtime_probe.py --wait-configured` waits for
   `session.updated`, separately records connection/setup time, and preserves
   the default ungated mode. Real WebSocket tests passed. Match recordings,
   prompts, duration and runtime; do not subtract setup from one condition
   without reporting it. Benchmark WebSocket replay currently starts without
   the acknowledgement; WebRTC waits. Startup buffering is plausible but unproven.
4. New Moshi/PersonaPlex diagnostics include configuration duration, first 64
   packet arrivals, queue depth and first-drop counters. Shutdown joins workers
   before releasing shared model ownership. These fixes are **not running in
   the current campaign**. Old backend drops several frames/session despite
   frame compute usually well below its 80 ms budget.
5. Freeze-Omni: added cache/storage/CUDA allocator diagnostics (`b934979b`), CPU
   tests passed. Run the endurance reproduction; history and deep-copied KV
   snapshots grow. Previous 600 s probe grew ~17.3 to 22.5 GiB, and OOM occurred
   under contention. No cache truncation or memory-growth fix was applied.
6. Lychee: retained 26-stall audit under
   `deploy/duplex/evidence/lychee/20260923-stall-audit/`. Seven stalls had no
   inference starts in prior 8 s; 19 kept computing, all with upstream PCM
   production evidence. Upstream content-hash suppression is only a hypothesis.
   New sidecar `audio_delivery` events record forward/muted/empty/interrupt
   disposition. Next GPU run must correlate production, suppression, SSE
   delivery and adapter state, with continuous paced input and preserved fixtures.

## DeepFilterNet work and critical regression

New explicit model **deepfilternet-fir** uses causal FIR conversion, supported
by the Python service, Go client, graph schema and frozen-profile settings.
Original defaults remain unchanged. Usage: [FIR.md](../tools/noisefilter/FIR.md).

- Tests: 11 FIR tests including real model/HTTP; measured **42.0 ms** delay at
  16/24/48 kHz; packet independence, sample counts and headers verified.
- Passband: old identity conversion lost 6.17 dB at 6 kHz/16 kHz input; FIR
  loss under 0.1 dB, alias rejection above 70 dB for the tested out-of-band tone.
- Single paced HTTP session: p99 8.13 ms, zero 50 ms misses / 300 requests.
- Four sessions with default two-state pool: startup deadline misses retained.
  Added `--state-pool`; pool four gave zero misses / 1,200 requests in one short
  comparison, first requests 10–11 ms, maximum 24.25 ms. Not general capacity proof.
- CLI `noise-filter-model` now persists in frozen graph values; tests passed.
- **Do not promote FIR:** last quiet-speech probe found a serious regression.
  Twenty existing clean backchannels, 16 kHz, original and -30 dB input scaling,
  full recording, delay-aligned energy windows, fresh states, no ASR. Mean energy
  retention at original level: legacy -0.646 dB, FIR -0.168 dB. At -30 dB:
  legacy -19.963 dB, FIR -98.522 dB; 12/20 FIR clips lost over 60 dB versus 2/20
  legacy. Values use a 1e-12 power floor, so near-silence is not precise acoustic
  measurement. Inspect scaling, PCM quantization, model input/noise-floor and
  low-level output before claiming cause. Recognition/noisy-backchannel quality
  remains unmeasured for FIR.
- All results under `deploy/duplex/evidence/deepfilternet/20260923-fir/`, including
  `quiet-clean.json`. Temporary services on :9166 (PID3296756) and :9167
  (PID3309116) were terminated at handoff; recreate explicitly if needed.

## Remaining plan requirements

Broad integration exists; full acceptance does not. Outstanding work includes:

- Graph-native sidecar-v4 acceptance versus native v1/v3 compatibility.
- Rendered-playback receipts, cancellation/reconnect/endurance/concurrency,
  stale generation rejection, corrections and tool-execution receipts.
- DuplexCascade long responses (~436–547 words) exceed 180 s deadlines;
  do not inflate timeouts or truncate just to pass. Real component probes work.
- MiniCPM-o poor interruption/overlap; Moshi premature answers and GPU lifecycle;
  Lychee stalls; Freeze-Omni memory growth; PersonaPlex input drops.
- Fish is sentence input, not true incremental text input.
- DeepFilterNet quiet-speech suppression and FIR regression described above.
- X2-Turn causal prefix proof; offline scores excluded due invalid timing.
- ELLSA/BayLing live task acceptance; currently feasibility, not action acceptance.
- Controlled repeated system comparisons, recognition/quality stress fixtures,
  all required benchmark conditions, and final requirement-by-requirement audit.
  All 21 FD conditions comprise 6,147 conversations (~77 source hours); only
  one condition is covered by the current native campaigns.
- Closed-provider legacy evidence lacks full provenance; verified reruns pending.
- DuplexOmni hardware deferred; SALMONN-omni/OmniFlatten matching assets deferred.

## Repository and operational constraints

User authorizes organized commits and direct pushes to `origin/main`.
All work through `a634a18e` was pushed before this handoff commit.
Use `/usr/local/go/bin/go`; default system Go cannot parse this repository's
Go 1.25 toolchain directives. Python environments differ by model.

User has **never trained checkpoints**: C2 is not applicable. Do not ask for
user-trained assets again. HF access to DuplexCascade and PersonaPlex was
verified and assets downloaded; `env -u HF_TOKEN hf ...` uses the saved authorized
credentials instead of the shell override. Do not print credentials.

Unrelated dirty work is intentionally preserved in `bench/capability/*`,
`bench/scenario/interaction/`, `tools/interactionstudy/`, `docs/experiments/`,
`docs/interaction-capability-*`, and graph scenario-conversation outputs/tests.
Do not stage, reset, or commit it as part of this goal.

Shared disk had only **6.4 GiB free** at handoff and filled twice during the
campaign. Original `resources.jsonl` ends with an incomplete record; preserve
it. Gap/incident evidence commits `a1fb0b26`, `e041f30c`. Logging completeness
cannot be assumed. Prior task-owned cache cleanup is already largely exhausted;
preserve model assets, installed environments, results and unrelated caches.

No full-goal completion claim is justified. Resume from the existing campaign,
not from a fresh download/reimplementation pass.
