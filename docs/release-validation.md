# Release validation matrix

`scripts/release-matrix.json` is the versioned, machine-readable inventory of
release tests and benchmark evidence. `scripts/release-validate.sh` validates
and runs it. The matrix is intentionally separate from `scripts/check.sh`:
`check.sh` remains the convenient offline developer check and, outside its
release mode, explicitly tolerates unavailable browser, SDK, Swift, and Python
dependencies. A release record must not inherit those tolerated skips.

Validate the checked schema and print its exact required gate IDs:

```sh
./scripts/release-validate.sh -mode validate -report -
```

Inspect every prerequisite without running a command:

```sh
./scripts/release-validate.sh \
  -mode plan -scope all \
  -report /tmp/openrealtime-release-plan.json
```

The plan uses `ready`, `blocked`, and `not_run`; it never uses `passed`.
Provisioned inputs such as a signed application, native macOS driver, pinned
tau2 environment, benchmark datasets, graph evidence, and live model endpoint
are reported individually. An unavailable prerequisite is `blocked`, even
when its gate was not selected. This makes the difference between “not run”
and “could not run here” visible in the retained JSON.

Run the default local matrix into a new evidence directory:

```sh
evidence=.runtime/release-validation/local-$(date -u +%Y%m%dT%H%M%SZ)
./scripts/release-validate.sh \
  -mode run -scope local \
  -artifacts "$evidence" \
  -report "$evidence/report.json"
```

The directory must not already exist. Each command receives separate stdout
and stderr logs. The report contains the checked command template, start and
finish times, semantic matrix SHA-256, exit status, timeout status,
prerequisite outcomes, observed Go test skips, and postcondition result.
Reports are create-only so a later run cannot silently overwrite the evidence
for an earlier one.

The local presentation gates are deliberately split. One runs every locked
browser profile in real Chromium; the other drives the browser and the exact
macOS distribution profile sequentially through one unchanged server and
asserts that the clean gateway acquired no UI route. Both refuse browser or
Node skips in release mode. The second gate is not a substitute for the
provisioned signed-macOS gate: its native half is a manifest-derived protocol
probe, while an actual signed `.app` launch remains Darwin-only evidence.

`selected_outcome: passed` means only that every gate selected by that
invocation passed. `release_complete` is stricter: it is true only when every
required ID in the entire matrix passed in that invocation. A local-only run
therefore says `release_complete: false` and lists the unrun or blocked
performance and provisioned gates under `missing_required_gates`. This is
deliberate; local CI success is not model, dataset, or signed-native evidence.

## Checked performance protocol

The direct audiovisual review-bundle benchmark is opt-in because it performs
the complete four-case Meeting retention path and the exact-sixteen
Realtime-CU retention path, including receipt verification. Run just that gate
with:

```sh
./scripts/release-validate.sh \
  -gate performance.review-bundles \
  -artifacts .runtime/release-validation/review-perf-001 \
  -report .runtime/release-validation/review-perf-001/report.json
```

The matrix invokes the repository-owned Meeting and Realtime-CU benchmarks
directly at their complete review populations. The Meeting package also runs
its checked allocation threshold. The gate requires both benchmark names in
stdout; timing, bytes, and allocations remain visible for review. This local
performance evidence does not substitute for either live behavioral suite.

## Direct candidate benchmark protocol

The checked matrix executes only the new graph-native implementation. The old
implementation is reference material, and the benchmark owner's recorded
numbers are quality targets for the case-by-case review; neither is a
production arm, command, registry, or runtime dependency.

The required direct candidates are FDB v1.5, FDB v3, FD-Bench, Meeting
Assistant cascade, Realtime-CU, the eleven-scenario profile, tau control, and
tau regular. Each gate must run its complete declared population against the
shared Realtime API with an exact execution requirement and authenticated live
graph inspection. Diagnostic subsets remain useful for iteration but cannot
satisfy a release gate.

Every new attempt must retain its deterministic result and the media needed to
review what happened. Audio cases retain playable audio; visual cases retain
the exact submitted images; computer-use and other audiovisual cases retain
synchronized video plus audio. Source manifests and external receipts are
published create-only after the complete population closes. Advisory review
uses the exact `google/gemini-3.7-flash` plug-in and never rewrites the
deterministic scorer.

The final review reports new totals, per-case outcomes, safety and deadline
failures, and latency distributions beside the trusted historical numbers. It
does not fabricate historical attempts or require historical media. A material
regression remains a blocker: retain the failed new run, diagnose it with the
new graph/runtime evidence, rerun the affected diagnostic slice, and then rerun
the complete candidate population.

The graph-native scenario candidate also owns a create-only human/media review
directory and an external source receipt. The source manifest is published
only after all 165 checklist rows, exact scorer results, stereo WAVs, submitted
visual inputs, candidate review indexes, and the finished architecture result are
closed and cross-bound. The release gate requires both the committed source
manifest and the portable receipt outside that directory; `CHECKLIST.md` or an
ordinary result JSON by itself is not retained-review evidence. Advisory model
reviews remain separate evaluations of that receipt and cannot change the
deterministic pass/fail result.

For operator review, run `openrealtime review scenario` against that sealed
directory and external receipt. The production registration is pinned to
`google.gemini-3.7-flash`; the evaluator receives only the canonical public
attempt context and digest-verified WAV/image bytes. It emits one create-only
evaluation bundle and sibling portable receipt per attempt, then a reproducible
case-by-case `REVIEW.md`, aggregate manifest, and aggregate external receipt.
These artifacts are secondary review evidence: they expose disagreement and
media-quality findings but never rewrite the checklist outcome. They must not
be represented as a passing live release gate unless the complete provisioned
population and all retained receipts were actually produced and verified.
The required `external.model.scenario-review` gate consumes the candidate
scenario source in the same fresh release artifact directory and asserts the
first and last per-attempt WAVs and receipts as well as the aggregate review;
the command's receipt verifier binds all 165 attempts between those endpoints.
`openrealtime review verify-scenario` provides the credential-free reopening
path for a copied source/evaluation pair and both external receipts.

## Provisioned gates

Use the all-scope plan as the authoritative prerequisite list. The principal
variables are:

- `OPENREALTIME_BENCH_ENDPOINT`, `OPENREALTIME_BENCH_EXECUTION`, and
  `OPENREALTIME_BENCH_INSPECTION_GRAPH` for dataset and owned benchmark runs;
- `OPENREALTIME_MEETING_CASCADE_ENDPOINT` and
  `OPENREALTIME_MEETING_OMNI_ENDPOINT` for the two Meeting Assistant clients;
- `OPENREALTIME_SPEECH_ENDPOINT`,
  `OPENREALTIME_SCENARIO_ARCHITECTURE_MANIFEST`, and
  `OPENREALTIME_SCENARIO_ARCHITECTURE_CELL` for all 11 scenarios at 15
  repetitions;
- `OPENREALTIME_TAU_USER_MODEL_ENDPOINT` and
  `OPENREALTIME_TAU_SYNTHESIS_ENDPOINT` for both complete 278-task tau2 speech
  conditions;
- `OPENREALTIME_PRESENTATION_LIVE_ENDPOINT` for the descriptor-locked,
  real-Chromium browser composition test;
- `OPENREALTIME_SIGNED_APP` and `OPENREALTIME_MACOS_E2E_RUNNER` on Darwin for
  the authority-signed native application gate.

URLs must be absolute and credential-free. Authentication stays in the
benchmark and server credential environment rather than being copied into the
matrix report. File, directory, and executable variables must also contain
absolute paths; they are reported by variable name, not by their potentially
sensitive value.

Run a provisioned gate by exact ID, or use `-scope all` only on a job that has
all prerequisites:

```sh
./scripts/release-validate.sh \
  -gate external.tau.upstream \
  -artifacts .runtime/release-validation/tau-upstream-001 \
  -report .runtime/release-validation/tau-upstream-001/report.json
```

The tau upstream gate performs `uv sync --all-extras`, `tau2 check-data`, Ruff
lint and formatting, `make test-all`, and the audio-native provider suite. It
is provisioned rather than local because dependency resolution and the pinned
external checkout are not offline repository inputs.

## Fail-closed rules

- Every gate is required. Diagnostics and smoke subsets do not belong in this
  matrix.
- Default local gates may record skips in the broad root/module sweeps because
  the skipped claims have dedicated gates. Dedicated release gates forbid any
  `--- SKIP:` result.
- Benchmark commands have explicit population postconditions. A CLI process
  that exits zero after printing `NOT REPORTABLE` does not pass the matrix.
- FDB v1.5 requires 498 completed recordings, FDB v3 requires 100, FD-Bench
  requires all 6,147 across the checked 21 partitions, Realtime-CU requires 16,
  Meeting Assistant requires four per foreground, tau2 requires 278 in each of
  control and regular, DynaCU requires 150, and the owned scenario run requires
  165/165 attempts plus its externally anchored, reopenable source/media
  receipt.
- Tests discover every `Fuzz*` function and every non-runtime Go module and
  compare them with matrix coverage. Adding one without a normal/race/vet or
  exact fuzz gate breaks `go test ./internal/releasevalidation`.
- Exit 0 means the selected job passed (or a plan is fully ready). It does not
  override the report's `release_complete` field. Exit 1 means a selected gate
  failed or was blocked. Exit 2 is a malformed invocation, matrix, or evidence
  destination.
