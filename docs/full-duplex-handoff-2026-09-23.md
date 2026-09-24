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

## Progress after resumption (2026-09-23, 08:05–10:45 UTC)

Every item below is committed and pushed; the results document links the evidence.

- **Item 1, PersonaPlex campaign: done.** 293/293 FD-Bench conversations,
  zero task errors, 77 passes; evidence `native-personaplex/20260923T012919Z-td0w3l3o-fdbench/`.
  The runner died around 08:05 without its `finally`, so completion was
  reconstructed from the surviving benchmark child. Every one of the 791
  sessions dropped input frames.
- **Items 2–4, lifecycle and readiness: done for PersonaPlex and Moshi.**
  Reconnect passed 3/3 on each. PersonaPlex drops come from audio sent before
  `session.updated` while the sidecar configures (2–5 s). The benchmark now
  has an opt-in `-wait-configured` flag (`9f6dd353`). A matched ABBA rerun
  (`native-personaplex/20260923-gating-ab/`) showed ungated replay turns FDB
  interruption applicability from 19/20 into 7/20 and misses 6/46 FD-Bench
  turns, versus 0/46 gated. Every earlier native campaign ran ungated.
  Moshi configures in under 0.25 s and drops nothing, but only 5/12 replies
  addressed the question.
- **Item 5, Freeze-Omni: diagnosed, not fixed.** The KV history is never
  truncated (10,062 tokens after 10 minutes), and the deep-copied snapshot
  shares no storage (865 MB each). Process memory grew from 17.3 to 25.1 GiB.
  The launcher bug that made it wait forever is fixed (`2c56bb40`).
- **Item 6, Lychee: diagnosed; fix committed but not GPU-verified.** Finite
  input halts its input-clocked generation, which explains all 8 FDB stalls.
  It also runs slower than real time: RTF 1.21 rising to 3.3. The sidecar now
  idle-fills wall-clock silence (`c2bdb04f`). GPU verification is **blocked**:
  the patched sm_120 vLLM tree in `.runtime/duplex-plan/build/` was deleted.
- **DeepFilterNet FIR:** the quiet-speech loss is the model's local-SNR gate.
  A clean offline 48 kHz conversion fails the same way; legacy "retention"
  came from interpolation images. FIR stays unpromoted.
- **X2-Turn:** with padding frames excluded and the buffer length fixed, no
  future audio leaks (exact at a 7-frame delay). Shorter buffers still shift
  probabilities by up to 0.12, so the scoring guard is unchanged. Accepting
  that gap is a decision still to make.

## Lost in the 10:16 UTC space cleanup

The checkout was deleted and re-cloned. `.runtime` was kept, except for
`.runtime/duplex-plan/results/` (raw campaign data and non-committed probe
output) and `.runtime/duplex-plan/build/`. `build/` held the Lychee vLLM
build, its patch and build script, and the 600 s long-session input
`long-input-600s.wav`. The worktrees `OpenRealtime-duplex-next-validation`
and `OpenRealtime-duplex-lifecycle-validation` are gone; all their commits
are on main. Only committed evidence under `deploy/duplex/evidence/` remains.

## Still open

- Rebuild the Lychee vLLM tree (or restore it from backup), then run the
  idle-fill verification (same fixtures as `lychee/20260923-stall-reproduction/`).
- Decide whether native campaigns should run with `-wait-configured`. If so,
  rerun them; the gated results are not comparable with earlier ungated ones.
- Freeze-Omni context bounding, Lychee throughput and a missing yield signal
  after speech, and Moshi answer relevance each need a design decision and a
  matched quality run.
- Unchanged from above: graph-native sidecar-v4 acceptance, playback receipts,
  DuplexCascade long responses, MiniCPM-o full campaign, Fish incremental text,
  ELLSA/BayLing tasks, the remaining 20 FD-Bench conditions (~77 source hours
  per model), closed-provider reruns, and the final requirement audit.

## Second round (2026-09-23, 10:45–12:10 UTC, after the fresh clone)

These follow the user's decisions: rebuild vLLM, gate native campaigns by
default if gating works, and accept the X2-Turn probability gap.

- **Lychee vLLM rebuilt and versioned.** `deploy/duplex/services/build-lychee-vllm.sh`
  and `lychee-vllm-sm120.patch` (`35b62d1d`, `b8948731`) rebuild the tree. The
  lost patch had one undocumented part, now added: disabling xformers' Hopper-only
  FA3, which aborts on sm_120.
- **Lychee idle fill verified** (`lychee/20260923-idle-fill/`): both
  finite-input probes finished without stalls, with ~28 s of silence filled.
  The run's FDB outcomes are not a behavior measurement. The host was
  saturated by another project's OpenROAD jobs, and rounds ran at RTF 2–5.
  **Rerun the FDB check on a quiet host.**
- **Native campaigns are gated by default** (`f7d279f4`): `e2e_run.py` passes
  `-wait-configured` and records `wait_configured`; `--ungated` reproduces
  the earlier conditions.
- **X2-Turn accepted and scored** (`d762fbc1`, `interaction/20260923-x2-scored/`).
  The I0 and I1 work directories were rebuilt, and their baselines reproduce
  the earlier tables. Used only once its frames exist, X2-Turn is at chance:
  endpoint AUC 0.502 and barge-in balanced accuracy 0.50.
- **MiniCPM-o full campaign queued, gated.** The first start was OOM-killed:
  host RAM is held by seven OpenROAD jobs from another project.
  `.runtime/duplex-plan/results/native/minicpm-campaign-waiter-4536c718.sh` (detached; binary `openrealtime-4536c718`, which records the playout metrics)
  starts the model once 45 GB is available, then runs `e2e_run.py
  native-minicpm-o --full-selected`. Progress:
  `.runtime/duplex-plan/results/native/minicpm-campaign-waiter.log` and
  `.runtime/duplex-plan/results/e2e/native-minicpm-o/latest/`.

## Requirement audit

[The requirement-by-requirement audit](full-duplex-requirements-audit.md)
(2026-09-23) checks every plan gate against committed evidence. No gate is
fully met. The first release scope is not met, because no item has complete
traces with playback or cancellation validation. Its "Blocking gaps" list
orders the remaining work.

## Third round (2026-09-23, 12:10–13:45 UTC)

- **Component evidence re-retained** after the cleanup. Every rerun
  reproduces the published values; see each README:
  - `asr/20260923-rerun/`: Qwen3-ASR, Voxtral en/zh, Nemotron EN and 3.5,
    Kyutai STT, FunASR, all within 0.9 points of the published error rates.
  - `tts/20260923-rerun/`: Kyutai, Qwen3-TTS and VibeVoice timing, held-back
    prefix, cancel and intelligibility.
  - `s0/20260923-rerun/`: speaker attribution under overlap.

  Not rerun: Deepgram ASR and Aura (billed), CosyVoice (its service answers
  `/health` with 500), and the audio observer.
- **FDB playout metrics** (`d6ab0070`, `4536c718`): yield latency and
  contact/after-event audio on the speaker clock as well as by arrival; reported, not scored.
- **DuplexCascade generation** is unbounded exactly as in the released
  reference (`0c9edbf6`). Bounding it would be a labeled variant.
- **Running now: the gated micro-turn cascade campaign** (first-release item 1),
  `microturn-voxtral-qwen3-kyutai --full-selected` on `openrealtime-4536c718`.
  It started 13:38 UTC, detached with `setsid`; log
  `.runtime/duplex-plan/results/native/microturn-campaign.log`, results
  `.runtime/duplex-plan/results/e2e/microturn-voxtral-qwen3-kyutai/latest/`.
  Voxtral stops when it ends. The MiniCPM-o waiter needs 24 GB of free GPU and
  45 GB of host RAM, so it will start after this campaign if memory allows.
  **On completion:** check `finished.json`, retain the evidence under
  `deploy/duplex/evidence/`, and update the results and audit documents.

## Playback receipts and tool correctness (2026-09-23, evening)

The user agreed to two simple projects:

- **Playback receipts** (`1bab9039`, fixed server side in `d0225ace`) are
  validated on the micro-turn cascade: 35/35 truncations confirmed, audible
  yield p50 44 ms (`playback-receipts/20260923-microturn/`). Only the
  micro-turn sidecar marks interrupted turns so far. Moshi, PersonaPlex,
  Lychee and the other native sidecars still end every turn as completed.
- **Tool correctness** (`48b3a30d`): 8/10 on the ordinary cascade
  (`toolcall/20260923-cascade/`). Both failures are corrections: a
  fabricated weather answer with no call, and a misheard, split request that
  became a timer. The VoiceChat run waits for host memory.
- **The micro-turn cascade campaign completed** (`8fa60c70`): 242/293 FD-Bench
  conversations and all FDB categories, with no task errors.

## State at 2026-09-24 05:40 UTC

- **Gated PersonaPlex campaign complete** (`6074c4c3`): 0 input drops across
  791 sessions and 129/293 FD-Bench passes (77 ungated). All FDB categories
  improved or held.
- **Environments rebuilt after the cleanup:** `kyutai`, `freezeomni`,
  `cosyvoice`, `bayling` and `seamless-cu128`. Setup scripts are in
  `deploy/duplex/services/` (`setup-*-venv.sh`, `*-requirements.txt`).
  `seamless-assets` is regenerated by `seamless_eval.py` on each run.
- **CosyVoice TTS rerun retained** (`685933cc`). Every published ASR/TTS
  component row now has committed raw data.
- **Freeze-Omni:** the unbounded history matches upstream `163a248`, which
  bounds it only by resetting per recording and a 600 s idle timeout.
  Options A (release cached GPU memory between turns) and B (share rather than
  deep-copy the snapshot) do not change behaviour; C (session cap) and D
  (history window) do. **Awaiting the user's choice.**
- **DuplexCascade bounded variant:** explained to the user. Pausing generation
  while more than N seconds of speech are unplayed would be a separately
  labelled cell, not a C1 fix. **Awaiting the user's choice.**
- **Queued:** the gated MiniCPM-o campaign
  (`.runtime/duplex-plan/results/native/minicpm-campaign-queue.sh`, log
  `minicpm-campaign-queue.log`). It waits for GPU memory and the large-model
  lease, which the peer interaction-capability study currently holds.

## 2026-09-24 afternoon: GPU handed over, approved work under way

The user gave this work the whole GPU. The peer study's queued memory probe and
nine idle shared services were stopped. Kept: MiniCPM-o (running campaign),
Qwen3-8B on :9100 (its slow provider) and Kyutai TTS on :9125. The user's
"continue" was taken as approval for the recommended low-risk items. The paid
OpenAI/Gemini runs and the bounded DuplexCascade variant still need an explicit yes.

- **Freeze-Omni** (`f636ac17`): cached CUDA memory is returned after each
  answer, and the session ends with a fatal `context_limit` error past 28,000
  history tokens. Neither changes what the model says. Option B (sharing the
  snapshot) was not done, because upstream deep-copies it and aliasing safety
  was not verified.
- **Sidecar v4** (`3461f63c`): two conformance checks for stated behaviour.
  Cancellation is acknowledged while a 3 s request holds the worker (7 ms
  measured), and nothing is sent for a run after its acknowledgement. Late
  input, empty ticks and duplicate results are not defined by v4 and would need
  protocol design, not tests.
- **FD-Bench sampling** (`52270ffc`): `-sample-per-condition` / `-sample-seed`
  (seeded per condition, reported as incomplete).
- **MiniCPM-o gated campaign**: interruption 14/184, backchannel 91/91 and
  background speech 83/83 applicable passes (`1e56aff9`); the rest is running.
- **Queued after it** (`.runtime/duplex-plan/results/native/gpu-queue-20260924.sh`,
  log `gpu-queue-20260924.log`):
  1. Freeze-Omni endurance on the new 600 s input (`long_input.py`, `b7b68848`)
  2. VoiceChat tool suite
  3. Lychee FDB recheck
  4. Sampled FD-Bench for PersonaPlex, 50 × 20 conditions
  5. The same for the micro-turn cascade

  Steps 4–5 are about 17 h each. On completion, retain each step's results under
  `deploy/duplex/evidence/` and update the results and audit documents.
