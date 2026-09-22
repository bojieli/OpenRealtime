# Interaction capability study: implementation and GPU execution plan

**Status:** proposed, 2026-09-22. This document does not claim implementation or
GPU execution. **Design:** [questions, treatments, and scoring](interaction-capability-study.md).
Inspected repository: `293e4f74b43b3116e2e2e2786e379381dde3e77f`.

## Execution target and first decision

Target the existing Linux host `rtx-pro`, checkout `/home/ubuntu/OpenRealtime`,
with its recorded 96 GB RTX PRO 6000. Those specifications come from the existing
integration record; current availability and installed environments must be
checked at execution time. Model inference and training belong on that host.
The Mac is for editing, inspection, and listening.

Start with English because the currently integrated Kyutai TTS supports English
and French. Extend to Mandarin as a separately reported cohort with a validated
synthesizer; do not combine different synthesis models into one language-neutral
architecture score. Use the existing local Qwen3-8B, Voxtral, and Kyutai services
for infrastructure smoke checks, without claiming they are the strongest models.

No additional discussion is needed to begin implementation. Later decisions
depend on evidence: trained-checkpoint access, supported TTS lookahead controls,
pilot variance, and the actual memory/time cost of adaptation. A missing resource
blocks its cell, not the entire study.

## What exists and what is missing

| Surface | Current evidence | Work for this study |
| --- | --- | --- |
| `sidecars/microturn_sidecar.py` | Ordinary instruction-model control calls plus a separate answer stream; trained DuplexCascade mode raises an unavailable error | Preserve baseline; add a joint action/content treatment |
| `deploy/duplex/profiles/microturn-clock-only.yaml` | Fixed clock without sound evidence or event-triggered decisions | Smoke reference, not yet an A2-trained cell |
| `deploy/duplex/profiles/microturn-voxtral-qwen3-kyutai.yaml` | Adds sound evidence and word/pause triggers | Useful combined reference; not an isolated acoustic-evidence ablation |
| `tools/duplexmodels/tts_kyutai.py` | Same-context incremental synthesis, fixed model lookahead, model-derived audio delay | Instrument consumed text; distinguish delivery schedule from conditioning horizon |
| `spoken/` and `docs/spoken-boundary.md` | Heard/cut/pending distinction in the runtime | Verify each experimental sidecar uses actual playback evidence or labels its estimate |
| `bench/scenario/` and `bench/scenario/graphnative/` | Scenario replay, recordings, content/timing checks, evidence bundles | Extend with counterfactual pairs and causal evidence replay |
| `bench/architecture/` and architecture catalog | Exact cell identity and evidence-channel declarations | Add study-specific definitions, preserving existing revisions |
| `deploy/duplex/run-e2e.sh` | FDB/FD-Bench subset smoke driver | Existing smoke only; new study needs unique run IDs and paired task scoring |
| `docs/full-duplex-results.md` | Earlier execution narrative plus unfinished `RESULTS_*` tables | Do not use as completed performance evidence |

## Implementation stages

The paths marked **new** below are proposed artifacts, not commands or modules
that already exist. Implement them under the existing runtime/benchmark design;
do not change stable `api/v1` semantics to accommodate an experiment.

### P0 — freeze the experiment and validate observation

1. Add `docs/experiments/interaction-capability-v1.yaml` (**new**) containing cell
   IDs, input channels, clock mode, model identity, planning mode, synthesis
   settings, scoring windows, seeds, repeats, and availability reasons.
2. Add the study cases to the scenario framework with a separate versioned suite.
   Preserve the twelve-case suite and its historical identities unchanged.
3. Add a study trace schema to the benchmark evidence layer. Required fields:
   session/turn/pair/cell IDs, source interval, observation availability time,
   revision ID, model admission time, action, generated text, and playback mark.
4. Check source audio sample rates and actual played audio. Never infer successful
   overlap or content adaptation from event names alone.

**Acceptance:** one pair's two branches share an identical prefix; only the
authored feedback differs. The trace proves no future evidence was delivered.
Deliberately removing the feedback must make the adaptation scorer fail.

### P1 — causal evidence replay and matched input cells

Implement an annotated evidence source alongside the scenario replay layer.
Release each event at `available_at`, retain `source_start/end`, and allow explicit
delay injection. Keep unavailable input separate from observed silence.

Build A1–A3 first using one prompted bounded policy, same backbone and output
path. Use fixed 500 ms ticks initially. Add a 200/500/1000 ms diagnostic only
after the principal contrast works. Clock/event-trigger changes are separate
treatments: the two current micro-turn profiles change both evidence and schedule.

Support append, provisional revision, and committed-prefix events explicitly.
The first clean diagnostic can use committed events; real-ASR testing must retain
the cost of waiting for commitment. Replay never changes what the model knew at
a past tick. Cue labels derived from future audio are not admissible early.

**Acceptance:** A1/A2 have identical words and tasks but different model-visible
timing; A2/A3 have identical schedule and content but different declared acoustic
evidence. Transcript/cue arrival is causally checked at every model request.

### P2 — joint micro-turn policy

Create `sidecars/interaction_microturn_sidecar.py` (**new**) or an equivalently
isolated implementation sharing transport utilities with the current sidecar.
Do not relabel `OrchestratedModel` as a trained or native model.

The experimental session maintains timed observations, heard/cut/pending speech,
and a revisable content plan. At each tick, generate a bounded action plus text
continuation. Preserve history across ticks and allow new observations to affect
the next continuation. Use the model's supported causal schedule; do not claim
new-input admission during an immutable hosted generation call.

Prototype schema (debugging only; a released checkpoint uses its own grammar):

```json
{"act":"revise","text":"For Kyoto, I would start with…","replaces_pending":"segment-7"}
```

Acts: wait, speak, continue, backchannel, yield, revise. Validate schema and keep
control fields out of TTS. Initially cap content generation by a declared token
budget; report the resulting audio duration rather than treating token count as
time. Joint decisions and separate control/answer calls are distinct cells.

**Acceptance:** “I know that part” changes the next unspoken content, while “Please
explain that part” produces the opposite continuation. It is insufficient to stop
the old answer and acknowledge the user without fulfilling the changed request.
Paired no-feedback runs must not show the same arbitrary change.

### P3 — training, only after the protocol works

Preferred reproduction: an accessible trained micro-turn checkpoint with its
published input grammar, scheduling, and compatible tokenizer. If gated, record
unavailability; do not accept account terms or substitute weights under its name.

Independent adaptation: use the inspected Qwen3-8B backbone as the initial local
candidate and a versioned micro-turn schema. This is a new model experiment, not
a reproduction of DuplexCascade. Proposed starting budget: 5,000 licensed text
dialogue trajectories for a pilot, expanded only after held-out gains. Include
wait, continuation, pause, acknowledgement, interruption, content revision, and
contrasting outcomes after the same prefix. Include ordinary dialogue examples
to monitor language/task regression; do not train only on control labels.

Create `tools/interactionstudy/train/` (**new**) with dataset builder, split
manifest, training configuration, and evaluation entry point. Use parameter-
efficient adaptation; select BF16 LoRA or quantized adaptation after a memory
probe. Initial probe: 4k context, microbatch one, gradient checkpointing, a small
fixed number of training steps. Log peak allocated/reserved VRAM and seconds per
step. Token embeddings/output head must be trained if adding vocabulary; reusing
existing tokens avoids implying newly added tokens work without adaptation.

Stop inference services owned by this experiment before training. Do not assume
training and all reference models fit together. Freeze training/test templates,
speakers, and random seeds before the held-out campaign. Train matched A2/A3
variants with matched data/compute for a representation comparison.

**Acceptance:** learning curves, held-out act/content metrics, ordinary-dialogue
regression checks, complete data provenance, and a checkpoint manifest. No fixed
training-time estimate is promised until the probe is measured.

### P4 — planning and speech study

Add a speech-input scheduler and consumption trace to `tools/ttsprobe` or a
dedicated `tools/interactionstudy/` runner (**new**). Store which text was actually
visible to the model for each output region when the backend exposes it; otherwise
declare the conditioning horizon unobservable.

First run fixed-text delivery experiments on Kyutai in one continuing context.
Measure extra flushes/end markers: the current adapter notes that nonterminal
flush can still influence sentence-ending prosody. Match them where possible;
otherwise report the confound. Do not alter `second_stream_ahead` or delay values
and present an unvalidated checkpoint modification as a supported lookahead mode.

True lookahead experiments require a supported inference setting or separately
trained, matched variants. Keep that part unavailable until established. Fish is
a useful additional voice/model reference, but a Fish–Kyutai difference is not
causal evidence about lookahead.

Then cross fixed full planning, revisable full planning, and incremental planning
with the supported synthesis/delivery conditions. Keep speaking style instructions
and voice fixed. Include an offline blind listening package with full stereo
conversations and randomized pair order.

**Acceptance:** the report names the manipulated variable correctly, includes
latency and human ratings, and does not count more disfluency as greater quality.

### P5 — native references and held-out campaign

Use existing `duplex`/`upstream` integrations only after verifying the selected
model's real interface and input admission. Pin hosted model/date/settings and
record unavailable access. Closed services are optional external references;
they do not block the local study. Do not promise a large model will fit on the
96 GB host merely because an adapter exists.

Run the preregistered 24-pair pilot, then freeze prompts/scorers and run 80 new
pairs with three repeats per cell. Start with a small number of cells; estimate
total wall time and API cost from the pilot before scaling. Run one interaction
session at a time for the first latency-controlled campaign. Interleave cell
order, not concurrent GPU execution, to control host drift.

**Acceptance:** unique evidence bundles, complete failures/timeouts, per-family
metrics, paired uncertainty, blind listening results, and explicit confounds.
Existing FDB/FD-Bench subsets are supporting diagnostics, not replacements for
content-adaptation cases or full external benchmark campaigns.

## Existing GPU smoke run: executable today if the host is provisioned

These commands exercise the existing orchestrated baseline. They do **not** run
P1–P5 or train a model. Do not run them on the Mac. The host must already contain
the intended source revision, model snapshots, benchmark data, and Python
environments. This document does not install or download them silently.

Connect and inspect:

```bash
ssh rtx-pro
cd /home/ubuntu/OpenRealtime
git status --short
git rev-parse HEAD
nvidia-smi
uptime
test -x .runtime/qwen-asr/bin/python
test -x .runtime/duplex-plan/venvs/microturn/bin/python
test -x .runtime/duplex-plan/venvs/kyutai/bin/python
test -f deploy/duplex/manifest.json
```

Compare the remote revision with the intended local work before continuing.
Sync through the project's normal Git workflow; do not reset another checkout.
Inspect free GPU memory, CPU load, and listening ports 9100, 9101, 9125, and 9290
using `ss -ltnp`. An occupied port needs an ownership check, not a forced kill.
The service comments estimate roughly 22 + 20 + 5 GB for this stack, but those
are planning estimates, not admission guarantees. Existing vLLM scripts resolve
the local Hugging Face `refs/main`; record and compare the resulting immutable
snapshot hashes with the manifest before treating a run as pinned.

Build the existing server and inspect service ownership:

```bash
source scripts/go-toolchain.sh
ort_go_bin="$(openrealtime_go_bin)"
mkdir -p .runtime/duplex-plan/bin
"$ort_go_bin" build -o .runtime/duplex-plan/bin/openrealtime ./cmd/openrealtime
deploy/duplex/services/llm-qwen3-8b.sh status
deploy/duplex/services/asr-voxtral.sh status
deploy/duplex/services/kyutai-tts.sh status
```

A `status` exit code of one means the service is not ready. If a service is
already running, verify its model/configuration and avoid stopping someone
else's session. Start missing services sequentially, inspecting their logs if
startup fails:

```bash
deploy/duplex/services/llm-qwen3-8b.sh start
deploy/duplex/services/asr-voxtral.sh start
deploy/duplex/services/kyutai-tts.sh start
curl -fsS http://127.0.0.1:9100/health
curl -fsS http://127.0.0.1:9101/health
curl -fsS http://127.0.0.1:9125/health
```

The existing smoke driver overwrites results for the same profile and writes a
fixed trace filename. Preserve both on every invocation. Run the following in
the same shell, where the run directory variable remains available:

```bash
ort_run_dir="$(mktemp -d "$PWD/.runtime/duplex-plan/capability-smoke-XXXXXXXX")"
ort_profile=microturn-clock-only
mkdir -p "$ort_run_dir/prior"
if [ -d ".runtime/duplex-plan/results/e2e/$ort_profile" ]; then
  mv ".runtime/duplex-plan/results/e2e/$ort_profile" "$ort_run_dir/prior/e2e"
fi
if [ -f ".runtime/duplex-plan/traces/$ort_profile.jsonl" ]; then
  mv ".runtime/duplex-plan/traces/$ort_profile.jsonl" "$ort_run_dir/prior/trace.jsonl"
fi
git rev-parse HEAD > "$ort_run_dir/revision.txt"
git diff > "$ort_run_dir/worktree.patch"
git status --short > "$ort_run_dir/worktree-status.txt"
cp deploy/duplex/manifest.json "$ort_run_dir/model-manifest.json"
deploy/duplex/run-e2e.sh "$ort_profile" 2 0
cp -a ".runtime/duplex-plan/results/e2e/$ort_profile" "$ort_run_dir/e2e"
if [ -f ".runtime/duplex-plan/traces/$ort_profile.jsonl" ]; then
  cp ".runtime/duplex-plan/traces/$ort_profile.jsonl" "$ort_run_dir/trace.jsonl"
fi
printf 'Smoke artifacts: %s\n' "$ort_run_dir"
```

This requests two examples from each of four FDB categories and skips FD-Bench:
eight attempted cases. Required benchmark assets must be provisioned according
to the [benchmark guide](benchmarks.md). Inspect each benchmark JSON and log:
`run-e2e.sh` tolerates individual benchmark command failures, so `finished.json`
alone does not establish eight completed cases. A zero-output or missing-data
run is a failed smoke check. No capability claim follows from eight cases.

The source patch is supplementary provenance, not a portable source archive;
untracked source files must also be retained if used. For a reportable run,
prefer a committed source revision and an exact, immutable deployment bundle.

Stop only services this run started, and only after other users have not adopted
them. Their respective scripts expose `stop`. Do not run blanket process cleanup.

## New study runner: implementation contract, not an available command

Extend the existing benchmark machinery with an immutable study-run directory:

```text
artifacts/interaction-capability/<run-id>/
  experiment.yaml       # frozen cells and scoring policy
  environment.json      # code, models, host, load, memory, dependencies
  fixtures.json         # pair identity, split, audio/evidence hashes
  runs/<cell>/<pair>/<variant>/<repeat>/
    input.wav
    conversation.stereo.wav
    admitted-evidence.jsonl
    model-inputs.jsonl
    actions.jsonl
    playback.jsonl
    result.json
  summary.json
  listening/            # blinded package; mapping kept separately
  report.md
```

Resume only exact matching manifests; otherwise allocate a new ID. Record failed
attempts rather than replacing them. Keep prompt design on the pilot split.
Do not implement model-specific scenario shortcuts. Store API keys outside
artifacts, and retain only audio/outputs whose licenses and participant consent
permit the study. No hidden reasoning capture is required.

## Validation and completion

Each implementation stage runs focused tests for its new behavior: causal
release, event ordering, revision semantics, heard-state admission, withheld
future labels, paired-scorer discrimination, and output-context continuity.
Test the scorer against intentionally wrong content, always-silent output,
always-interrupt output, and generated-but-unheard text. Use the repository's
full `./scripts/check.sh` gate before submitting implementation changes; model
and GPU tests remain separately provisioned checks.

The first deliverable is P0–P2 plus a pilot report showing paired mid-speech
content adaptation. P3 establishes learned-policy behavior; P4 separates planning
from synthesis; P5 supplies broader external references. Publish conclusions
only at the strength supported by the completed stages, including unavailable
cells and negative results.
