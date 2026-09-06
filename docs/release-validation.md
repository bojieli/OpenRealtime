# Release validation matrix

Use this page to distinguish ordinary development checks, provisioned release
checks, and optional performance experiments. The matrix reports what ran,
what passed, what failed, and what could not run in the available environment.

| Goal | Command or section |
| --- | --- |
| Check a change during development | `./scripts/check.sh`; see [Contributing](../CONTRIBUTING.md) |
| Validate the matrix without executing gates | `./scripts/release-validate.sh -mode validate -report -` |
| Inspect prerequisites | `-mode plan -scope all` as shown below |
| Run provisioned checks | [Provisioned gates](#provisioned-gates) |
| Understand optional campaigns | [Complete-campaign comparison](#optional-complete-campaign-comparison) |
| Understand failure and skip handling | [Fail-closed rules](#fail-closed-rules) |

## Matrix and basic workflow

`scripts/release-matrix.json` is the versioned, machine-readable inventory of
release tests and optional diagnostic tools. `scripts/release-validate.sh` validates
and runs it. The matrix is intentionally separate from `scripts/check.sh`:
`check.sh` remains the convenient offline developer check and, outside its
release mode, explicitly tolerates unavailable browser, SDK, Swift, and Python
dependencies. A release record must not inherit those tolerated skips.

Benchmarks are debugging tools. Full campaigns, fresh benchmark archives,
external model reviews, score reporting, and the measurement program are not
requirements for project completion or publication. Historical results and
focused affected-case checks are sufficient inputs for finding and verifying
repairs. Every `external.benchmark.*` entry, the benchmark review and upstream
tau checks, `performance.review-bundles`, and `local.efficiency` are optional
(`required: false`, `selection: opt_in`). They do not enter
`missing_required_gates` or prevent `release_complete`. When explicitly selected,
a tool still checks its declared contract and reports failure honestly.

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
The provisioned Darwin gate builds the server executable from the checked
source, generates a private nonce-bound browser-then-native contract, and
accepts only a strict create-only receipt from the exact hashed runner. Its
retained contract and receipt bind signed app/code-directory identity, the
frozen launch profile, expected server Graph/profile and one process/run,
ordered distinct sessions, separate audio/camera/screen and native permission
paths, interruption-to-tool continuation, tools, inspection, population, and
cleanup. Portable receipt-verifier tests do not satisfy this gate.

`selected_outcome: passed` means only that every gate selected by that
invocation passed. `release_complete` is stricter: it is true only when every
required ID in the entire matrix passed in that invocation. A local-only run
therefore says `release_complete: false` and lists the unrun or blocked
required provisioned integration gates under `missing_required_gates`.
Optional diagnostic campaigns do not affect this field.

## Optional efficiency diagnostic

`local.efficiency` runs `openrealtime efficiency` for ten seconds of simulated
1080p video at 3 fps and retains the JSON report beside the run. It is a
local opt-in diagnostic: it needs no model, dataset, or network, and checks
that the report is written and reaches the audio gate. It does not compare
against machine-dependent timing thresholds or require published efficiency
numbers to be refreshed before release.

## Optional performance protocol

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

## Optional complete-campaign comparison

For bug fixes, start with existing failed cases and run only the behavior needed
to validate a repair. No frozen candidate, full-population rerun, sealed closure,
paid reviewer, or published score is needed for that workflow.

The existing complete-campaign tools remain available for an explicitly chosen
quantitative comparison. Their declared populations are FDB v1.5 (498), FDB v3
(100), FD-Bench (6,147), Meeting Assistant cascade (4), Realtime-CU (16), the
twelve-scenario profile (180), and tau control and regular (278 each). These
7,501 attempts describe the optional comparison contract, not a release quota.
Meeting omni and DynaCU are also independently optional.

The optional `external.benchmark.validation.behavioral` command consumes eight
campaign closures, a frozen-candidate declaration, and the checked target
registry. It verifies population, candidate identity, score reconstruction,
registered targets, and repair lineage. Those requirements apply when invoking
that comparison tool. An incomplete historical run can still diagnose bugs even
when it cannot satisfy this stricter comparison contract. Missing target data
blocks only the optional comparison, never project publication.

The following control-artifact reference documents that optional tool. Its
artifact names and `behavioral acceptance` output are retained for compatibility;
they do not impose a project acceptance program.

### Behavioral acceptance control artifacts

`scripts/behavioral-acceptance-targets.json` is the only comparison authority.
Candidate files are observed behavior, never a source from which a target is
inferred. Historical result JSON is deliberately not an input. Every suite has
an exact result kind, suite name, final population, case-key rule, and five
independent registrations: aggregate, per-case, safety, deadline, and latency.
Each registration is one of:

- `registered`, with a bounded source citation and explicit checked thresholds;
- `unavailable`, with a reason, which makes acceptance `blocked`; or
- `not_applicable`, only for an evidence domain and with a rationale.

A registered aggregate supplies `minimum_passed`. Registered per-case targets
must enumerate and account for the complete population. Safety rules are
zero-tolerance, at-most-zero failure bounds with one sample for every task.
Latency requires both a median and a tail bound; every observed `*_ms` metric
must be targeted or explicitly excluded with a rationale. Deadline-named
metrics receive the same inventory check. Missing required samples are
failures, not zeroes. A candidate therefore cannot replace a stronger checked
target with a generic 80% floor or use an aggregate pass to hide a severe
case-level, safety, deadline, or latency regression.

FDB v1.5 additionally requires an explicit positive aggregate
`minimum_applicable` and a `minimum_applicable` for every case. The registered
floor is 287 passes over at least 430 applicable recordings, with all 498
recordings completed. It translates the retained campaign's 355 nominal
passes by removing its 68 inapplicable recordings; the original artifacts and
receipts remain unchanged. Every previously applicable case, including a
behavioral failure, retains an applicability minimum of one. Historically
inapplicable cases retain completion requirements and zero applicability and
pass minima. Aggregate and case applicability floors are enforced separately,
so silencing a failing case cannot improve acceptance. New final FDB outcomes
must carry typed applicability; legacy nominal scores cannot certify a final
candidate. All execution, safety, deadline, latency, and media requirements
still apply to inapplicable recordings.

The second control artifact is supplied through the absolute path in
`OPENREALTIME_BEHAVIORAL_CANDIDATE`. It must exist before the final campaigns
start and is strict JSON of this shape (abbreviated to one suite here):

```json
{
  "format_version": 2,
  "candidate_id": "final-2026-09-02-01",
  "revision": "0123456789abcdef0123456789abcdef01234567",
  "executable_sha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "machine": {
    "cpu": "pinned CPU identity",
    "cores": 32,
    "gpu": "pinned GPU identity",
    "os": "linux",
    "arch": "amd64",
    "go_version": "go1.25.0",
    "hostname": "release-runner-01"
  },
  "suites": [
    {
      "id": "fdb-v1.5",
      "execution_requirement_sha256": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "run_spec_sha256": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "task_inventory_sha256": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "scorer_sha256": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
      "source_receipts": [
        {
          "kind": "deterministic-source",
          "artifact_format": "openrealtime.candidate-source-receipt"
        }
      ],
      "lineage": [
        {
          "campaign_id": "fdb15-failed-full-01",
          "kind": "failed_full",
          "population": 498,
          "artifact_sha256": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
        },
        {
          "campaign_id": "fdb15-focused-fix-01",
          "kind": "focused_diagnostic",
          "population": 24,
          "artifact_sha256": "sha256:9999999999999999999999999999999999999999999999999999999999999999"
        },
        {
          "campaign_id": "fdb15-final-full-02",
          "kind": "final_full",
          "population": 498
        }
      ]
    }
  ]
}
```

The real declaration must contain all eight uniquely sorted suite IDs. Its one
global revision, executable digest, and exact machine identity must match every
closure and result. Each suite pins the SHA-256 of the canonical bytes emitted
by `bench.MarshalExecutionRequirement`, which binds Graph IR, values,
deployment, and live element/runtime identities; it is not a digest of an
arbitrarily formatted source file. It also pins three pre-run canonical
artifacts: the complete behavior-affecting command/environment/endpoint run
specification, the exact sorted task inventory, and the deterministic scorer
implementation plus immutable scorer inputs. The source-receipt requirements
freeze semantic kinds and schemas before the receipts can exist.

Lineage is chronological. A suite with no observed regression has only its
predeclared final `final_full` row. Every `failed_full` row retains the raw
canonical digest of its campaign closure and requires a later retained
`focused_diagnostic` closure; the last row must then be a new complete
`final_full` campaign. A focused population must remain smaller than the suite
population, and every row labelled as a full run must have the exact complete
population. The final closure does not exist when the candidate is frozen, so
the last declaration row intentionally has no artifact digest. Its closure
must name that exact campaign and directly bind every earlier closure in
chronological order. A focused pass without the subsequent complete
affected-suite rerun is rejected.

### Campaign closure contract

All artifacts referenced by a closure use canonical flat filenames in the
closure's directory. They may not use absolute paths, parent traversal, nested
directories, or symlinks. The static run specification, task inventory, and
scorer manifest are canonical compact JSON terminated by one newline. Generate
and review them before freezing the candidate; the candidate pins their raw
SHA-256 values. Endpoint entries retain only secret-free normalized endpoint
digests, and the public environment list must never contain credentials.

For example, the referenced `fdb15.run-spec.json` includes the explicit working
directory as well as the positional argument vector; an omitted working
directory or executable is invalid, while an intentionally empty non-leading
argument remains representable:

```json
{"format":"openrealtime.behavioral-run-spec","format_version":1,"suite_id":"fdb-v1.5","campaign_id":"fdb15-final-full-02","working_directory":"repository-root","arguments":["openrealtime","bench","fdb","-endpoint",""],"environment":[{"name":"OPENREALTIME_RELEASE_MODE","value":"true"}],"endpoints":[{"name":"realtime","sha256":"sha256:..."}]}
```

After a campaign, wrap each externally retained suite source receipt in an
`openrealtime.behavioral-artifact-receipt`. The wrapper records its semantic
kind, the underlying receipt's format, flat filename, exact raw digest, the
underlying receipt's path-independent `receipt_sha256`, and a wrapper
self-digest. The closure then binds the wrappers, result, exact sorted task
inventory, a digest of all complete task rows, and every predecessor closure.
Its draft has this abbreviated shape:

```json
{"format":"openrealtime.behavioral-campaign-closure","format_version":1,"suite_id":"fdb-v1.5","campaign_id":"fdb15-final-full-02","candidate_id":"final-2026-09-02-01","candidate_sha256":"sha256:...","revision":"0123456789abcdef0123456789abcdef01234567","executable_sha256":"sha256:...","machine":{"cores":32,"os":"linux","arch":"amd64","go_version":"go1.25.0"},"execution_requirement_sha256":"sha256:...","run_spec":{"format":"openrealtime.behavioral-run-spec","format_version":1,"path":"fdb15.run-spec.json","artifact_sha256":"sha256:..."},"inventory":{"format":"openrealtime.behavioral-task-inventory","format_version":1,"path":"fdb15.inventory.json","artifact_sha256":"sha256:..."},"source_receipts":[{"format":"openrealtime.behavioral-artifact-receipt","format_version":1,"path":"fdb15.source.wrapper.json","artifact_sha256":"sha256:..."}],"result":{"format":"bench_result","format_version":1,"path":"candidate-fdb15.json","artifact_sha256":"sha256:..."},"scorer":{"format":"openrealtime.behavioral-scorer","format_version":1,"path":"fdb15.scorer.json","artifact_sha256":"sha256:..."},"expected_population":498,"task_population_sha256":"sha256:...","predecessors":[],"closure_sha256":""}
```

Repair predecessors need not—and normally cannot—share the final candidate's
revision or executable. A behavior-affecting repair creates a new candidate.
Every predecessor closure is independently reopened and must belong to the
same suite, while the frozen final-candidate declaration binds its exact digest,
campaign identity, population, and chronological role. Only the eight accepted
final closures must all name the one final frozen candidate and executable.
Requiring old failed closures to claim that final identity would make a real
code-repair lineage impossible.

Publish that draft only through the verifier. It first reopens and hashes every
artifact, decodes the typed manifests with unknown-field rejection, confirms
the result IDs exactly equal the pre-run inventory, recursively verifies the
predecessors, calculates `closure_sha256`, writes with create-only semantics,
and reopens the published closure:

```sh
go run ./internal/releasevalidation/cmd/campaignclosure \
  -draft .runtime/release/final/fdb15.closure.draft.json \
  -out .runtime/release/final/candidate-fdb15.closure.json
```

The closure verifier accepts only repository-owned receipt schemas. It invokes
the generic candidate-source verifier for FDB v1.5, FDB v3, FD-Bench, and
τ-Voice; the scenario source verifier; the Meeting source verifier; or the
Realtime-CU source verifier. Each implementation reopens the complete retained
tree against its external receipt, and the deterministic result digest returned
by that verifier must equal the closure's result digest. An unknown receipt
schema fails; a byte-valid wrapper cannot nominate its own trusted verifier.

The closure is still a content-integrity receipt, not a signature or proof that
an untrusted author really ran a model. The generic candidate source verifier
reconstructs and cross-checks retained attempts, completions, transcripts,
media, and deterministic rows. Recovered FDB v1.5, FDB v3, and FD-Bench
attempts are suite-rescored before they may seal; recovered τ-Voice attempts
currently fail closed because the retained trace cannot independently replay
the authoritative tau2 score. Acceptance does not claim to rerun every suite's
scorer from raw media. An operator who needs authenticated comparison evidence can retain the closure in
an independently controlled artifact store; fabricating a coherent replacement
universe is outside what an unkeyed SHA-256 receipt can detect. A result JSON
alone, a self-declared `final_full` label, or a closure whose bound artifacts
drift cannot pass.

Run the acceptance command directly only after all eight final closures exist:

```sh
go run ./internal/releasevalidation/cmd/behavioracceptance \
  -targets scripts/behavioral-acceptance-targets.json \
  -candidate "$OPENREALTIME_BEHAVIORAL_CANDIDATE" \
  -closure fd-bench=.runtime/release/final/candidate-fdbench6147.closure.json \
  -closure fdb-v1.5=.runtime/release/final/candidate-fdb15.closure.json \
  -closure fdb-v3=.runtime/release/final/candidate-fdb3.closure.json \
  -closure meeting-cascade=.runtime/release/final/candidate-meeting-cascade.closure.json \
  -closure realtime-cu=.runtime/release/final/candidate-realtime-cu.closure.json \
  -closure scenario=.runtime/release/final/candidate-scenario.closure.json \
  -closure tau-control=.runtime/release/final/candidate-tau-control.closure.json \
  -closure tau-regular=.runtime/release/final/candidate-tau-regular.closure.json \
  -report .runtime/release/final/behavioral-acceptance.json
```

The report is create-only and has three outcomes. `passed` means every suite
has the exact population and identity and clears every registered target;
`failed` means candidate evidence or an observed value violates the contract;
`blocked` means required trusted target data is honestly unavailable. Both
`failed` and `blocked` exit 1. Malformed control artifacts, invocation errors,
the deprecated unsealed `-result` flag, or an existing report path exit 2.
Passing unit and integration tests merely verify this machinery; a `passed`
report requires the declared campaigns and independently verified closures.
This is an optional comparison result, not a publication prerequisite.

The graph-native scenario candidate also owns a create-only human/media review
directory and an external source receipt. The source manifest is published
only after all 180 checklist rows, exact scorer results, stereo WAVs, submitted
visual inputs, candidate review indexes, and the finished architecture result are
closed and cross-bound. The optional campaign check requires both the committed source
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
be represented as a passing optional live review check unless the complete provisioned
population and all retained receipts were actually produced and verified.
The optional `external.model.scenario-review` gate consumes the candidate
scenario source in the same fresh release artifact directory and asserts the
first and last per-attempt WAVs and receipts as well as the aggregate review;
the command's receipt verifier binds all 180 attempts between those endpoints.
`openrealtime review verify-scenario` provides the credential-free reopening
path for a copied source/evaluation pair and both external receipts.

## Provisioned gates

Use the all-scope plan as the authoritative prerequisite list. The principal
variables are:

- `OPENREALTIME_BENCH_ENDPOINT`, `OPENREALTIME_BENCH_EXECUTION`, and
  `OPENREALTIME_BENCH_INSPECTION_GRAPH` for dataset and owned benchmark runs;
- `OPENREALTIME_MEETING_CASCADE_ENDPOINT` for the optional Meeting Assistant
  reference and `OPENREALTIME_MEETING_OMNI_ENDPOINT` only when selecting its
  optional native-audio validation;
- `OPENREALTIME_SPEECH_ENDPOINT`, `OPENREALTIME_SCENARIO_TRANSCRIBE_ENDPOINT`,
  `OPENREALTIME_SCENARIO_TRANSCRIBE_MODEL`,
  `OPENREALTIME_SCENARIO_ARCHITECTURE_MANIFEST`, and
  `OPENREALTIME_SCENARIO_ARCHITECTURE_CELL` for all 12 scenarios at 15
  repetitions;
- `OPENREALTIME_TAU_USER_MODEL_ENDPOINT` and
  `OPENREALTIME_TAU_SYNTHESIS_ENDPOINT` for both complete 278-task tau2 speech
  conditions;
- `OPENREALTIME_BEHAVIORAL_CANDIDATE` for the absolute path to the strict
  pre-run candidate declaration shared by all final behavioral suites;
- `OPENREALTIME_PRESENTATION_LIVE_ENDPOINT` for the descriptor-locked,
  real-Chromium browser composition test;
- `OPENREALTIME_SIGNED_APP`, `OPENREALTIME_MACOS_E2E_RUNNER`, the exact
  `OPENREALTIME_MACOS_E2E_LAUNCH_PROFILE`, and expected
  `OPENREALTIME_MACOS_E2E_GRAPH_FINGERPRINT` and
  `OPENREALTIME_MACOS_E2E_SERVER_PROFILE_FINGERPRINT` on Darwin for the
  authority-signed native application gate.

URLs must be absolute and credential-free. Authentication stays in the
benchmark and server credential environment rather than being copied into the
matrix report. File, directory, and executable variables must also contain
absolute paths; they are reported by variable name, not by their potentially
sensitive value.

Run an optional provisioned check by exact ID on a job with its prerequisites:

```sh
./scripts/release-validate.sh \
  -gate external.tau.upstream \
  -artifacts .runtime/release-validation/tau-upstream-001 \
  -report .runtime/release-validation/tau-upstream-001/report.json
```

The optional tau upstream check performs `uv sync --all-extras`, `tau2 check-data`, Ruff
lint and formatting, `make test-all`, and the audio-native provider suite. It
is provisioned rather than local because dependency resolution and the pinned
external checkout are not offline repository inputs.

## Continuous integration

The `release-matrix` job in `.github/workflows/ci.yml` runs the complete
local scope of this matrix on every change, with skips forbidden, and keeps
the create-only report and per-gate logs as a build artifact. It provisions
what the local gates need and nothing more: a browser, Node and the official
SDK, Python with the sidecar test requirements, ffmpeg and bubblewrap, the
Opus development libraries, and the pinned Swift image. Provisioned gates
stay blocked there, which the report records. `check.sh` remains the
developer gate; this job also covers the dedicated Opus, sidecar-conformance,
and fuzz checks. The optional efficiency diagnostic runs only when selected.

### A fuzz-engine behaviour the cancellation gates work around

Under `go test -fuzz` on go1.25.0 and go1.25.14, a goroutine blocked on a
context's `Done` channel can be handed `context.Canceled` from a context whose
`context.Cause` reads as the caller's cause a microsecond later, in the same
process, on the same context object, with a single worker. A twenty-line
target that only blocks and reads the cause reproduces it within a second;
the same target passes two thousand times when built with the fuzz
instrumentation but run without the engine, and the runtime's own seeds prove
the cause survives every wait path in the ordinary sweep. The three
cancellation fuzz gates therefore accept the bare cancellation as the same
outcome only while the fuzz engine is driving them, detected from the
`-test.fuzz` flag; the cause assertion is unchanged everywhere else.

## Fail-closed rules

- Every gate is either a required release claim or an explicitly opt-in,
  `required: false` independent validation. Diagnostic and smoke subsets do
  not belong in this matrix. Optional validation never enters
  `release_complete`; the complete-campaign comparison is itself optional.
- Default local gates may record skips in the broad root/module sweeps because
  the skipped claims have dedicated gates. Dedicated release gates forbid any
  `--- SKIP:` result.
- Benchmark commands have explicit population postconditions. A CLI process
  that exits zero after printing `NOT REPORTABLE` does not pass the matrix.
- FDB v1.5 requires 498 completed recordings, FDB v3 requires 100, FD-Bench
  requires all 6,147 across the checked 21 partitions, Realtime-CU requires 16,
  the cascade Meeting Assistant reference requires four, tau2 requires 278 in
  each of control and regular, and the owned scenario run requires 180/180
  attempts plus its externally anchored, reopenable source/media receipt. The
  optional omni Meeting run still requires all four cases when selected, and
  the optional DynaCU run still requires all 150 tasks when selected; neither
  enters the optional eight-suite comparison population. None of these
  campaign populations is required for publication.
- Tests discover every `Fuzz*` function and every non-runtime Go module and
  compare them with matrix coverage. Adding one without a normal/race/vet or
  exact fuzz gate breaks `go test ./internal/releasevalidation`.
- Exit 0 means the selected job passed (or a plan is fully ready). It does not
  override the report's `release_complete` field. Exit 1 means a selected gate
  failed or was blocked. Exit 2 is a malformed invocation, matrix, or evidence
  destination.
