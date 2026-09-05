# Graph-native interaction-scenario launch contract

This package is the scenario suite's fail-closed composition boundary. It does
not contain a server, model, provider, credential, UI, or binding-name switch.
It decorates an explicitly selected `graph/launch` adapter plugin, checks the
adapter against the exact immutable plan, and returns the prepared
`NativeBinding` for a host to compose as its `server.SessionProvider`.

The complete contract covers the twelve repository-owned interaction cases.
The ordinary path requires:

- session settings (`gateway.input.update`);
- continuous realtime PCM input (`gateway.input.audio`);
- turn, activity, transcript, speech, and failure outputs; and
- audio input/output, transcription, turn generation, and concurrent I/O in
  the adapter's frozen capability projection.

Two cases add protocol seams without adding a launcher species:

- `a recorded menu` requires the function-call, tool-result, and
  `response.create` round trip;
- `telling them what it saw` requires a typed text/message input carrying the
  still image, an ordered explicit `response.create` after each durable image
  item, plus visual-input and text-injection capability evidence.

`BuildContract` derives and fingerprints those requirements from the reviewed
suite. `GuardAdapter` is the composable plugin decorator. `GuardLaunch` applies
it to an exact selected `graph/launch` config for direct composition through
`server.NewGraphBundle`. `New` is the provider-only convenience constructor.
Both delegate topology, lock, values, deployment, secret-catalog, assembly,
adapter-artifact, and mount-dependency validation to `graph/launch`.

For the normal descriptor-locked server path, `NewApplicationPlugin` wraps an
installed graph application as another application plugin. Its strict
configuration records the complete ordered scenario case list, this contract's
fingerprint, and the delegate application's exact registration, runtime
artifact, SessionProvider artifact, and plugin-owned configuration. There is no
application-kind or binding switch. `FreezeLaunchProfile` obtains the Plan and
adapter identities by actually preparing that selected resource-free launch
configuration, then freezes the ordinary `graph/launch/profile` document. The
host still exact-matches the installed application, provider, and gateway
artifacts before it can open a listener or start a session. Presentation hosts
remain clients of the same Realtime and management APIs; this package installs
no HTTP or UI route.

`TestProfiledGraphNativeWebSocketExercisesExactTwelveScenarioContract` is the
credential-free protocol checkpoint. It starts twelve independent sessions
through that generic profile registry and exact graph-native server path. Every
case crosses session configuration, realtime PCM input, concurrent input/output,
and a complete response lifecycle. The recorded-menu case additionally crosses
function call, result, and `response.create`; the visual case sends the checked
PNG as an official `input_image` message and then explicitly creates its
response. A separate deterministic failure
event covers the adapter's typed failure boundary. This catches profile, mount,
gateway, and wire regressions, but deliberately does not score the scripted
interaction behavior.

The checked release matrix runs that test as the required default local gate
`local.scenario.profiled-websocket`. Its skip policy is `forbid`, and it has no
credential, browser, network, or provider prerequisite; a release cannot turn
an unavailable live benchmark into a local pass by skipping this checkpoint.

The checked full-suite contract fingerprint is in
`testdata/full-suite-contract.sha256`. Changing a case's external composition
surface requires an explicit artifact review.

Static launch compatibility is not behavioral evidence. In particular, it
does not prove that the graph waits through requested silence, interrupts at
the right moment, distinguishes nearby speakers, handles acknowledgements,
chooses the correct tool, understands the image, or meets a latency bound.
Those claims still require the preregistered live run of all twelve scenarios
at the required repetitions with authenticated runtime evidence. Provider
credentials and live results are intentionally outside this package.

The repository-owned scenario-conversation graph now composes the reusable
`interaction.PostCommitSilence` element at an explicit 15,000 ms. Virtual-clock
tests prove that only a successful durable audio/message commit arms or resets
the deadline and that expiry emits a response invocation bound to the latest
committed prefix. That establishes the case-7 mechanism, not a live behavioral
pass: the model still has to interpret the user's request and produce the
requested check-in in the retained 15-repeat run.

The provisioned candidate remains wired as
`external.benchmark.scenario` in `scripts/release-matrix.json`. It runs the
existing public scenario client against the graph-native endpoint with the
reviewed architecture cell, authenticated inspection graph, sealed review
bundle, speech service, and fifteen repetitions per case:

```sh
go run ./cmd/openrealtime scenario \
  -url "$OPENREALTIME_BENCH_ENDPOINT" \
  -speech-url "$OPENREALTIME_SPEECH_ENDPOINT" \
  -transcribe-url "$OPENREALTIME_SCENARIO_TRANSCRIBE_ENDPOINT" \
  -transcribe-model "$OPENREALTIME_SCENARIO_TRANSCRIBE_MODEL" \
  -repeat 15 \
  -architecture-manifest "$OPENREALTIME_SCENARIO_ARCHITECTURE_MANIFEST" \
  -architecture-cell "$OPENREALTIME_SCENARIO_ARCHITECTURE_CELL" \
  -launch-profile "$OPENREALTIME_SCENARIO_LAUNCH_PROFILE" \
  -inspection-graph "$OPENREALTIME_BENCH_INSPECTION_GRAPH" \
  -review-dir results/candidate-scenario-review \
  -review-receipt results/candidate-scenario-review.receipt.json \
  -record results/candidate-scenario.json
```

For a graph-native architecture cell the command fails before credentials or
protocol work unless the launch profile, cell graph requirement, adapter name,
and the cell's exact runtime adapter-profile fingerprint form one valid frozen
checklist selection. By default the graph-native path runs the complete
canonical suite through `NewLiveExecutor` and `RunChecklist`.

For a focused diagnostic, use `scenario -list` to obtain exact case names,
then pass the same repeated `-case` flags to `profile scenario` and `scenario`.
For example, select `-case 'an acknowledgement is not an interruption'` when
freezing the profile and when running its reviewed architecture cell. The
profile freezes the selected contract; an omitted, additional, duplicate,
unknown, or whitespace-altered selection cannot silently run a different
population. Names are ordered by the authored suite regardless of flag order.
The removed substring-based `-only` flag remains unsupported.

Subset runs retain the same source/media and review receipts as full runs.
Their case ordinals and media filenames start at one within the selected
population, with the exact case names and contract fingerprint retained beside
them. The command labels them `DIAGNOSTIC SUBSET`; `checklist.json` records
`full_suite: false`, `reportable: false`, and `passed: false`, even with fifteen
or more repetitions and individually passing cases. Architecture reportability
describes its declared population; the checklist additionally enforces the
complete-suite behavioral gate. A subset cannot replace any required campaign.

The selected `-review-dir` plug-in writes create-only per-attempt stereo WAVs,
the exact submitted visual bytes, independently verified media manifests,
per-attempt checklist rows, `checklist.json`, and a case-by-case
`CHECKLIST.md`. The existing sanitized `REVIEW.md` and review manifest remain
alongside those graph-native artifacts. After the final architecture result is
finished and retained, the plug-in closes every writable review handle,
snapshots the exact flat evidence tree, and publishes `source-manifest.json`
last. That source manifest cross-binds every checklist row to its exact scorer
result, media manifest, stereo WAV, submitted visual bytes, final
`architecture-result.json`, and complete file-set digest. The create-only
portable receipt selected by `-review-receipt` is written outside the review
directory and is required to reopen the source bundle; its default is
`<review-dir>.receipt.json`.

A checklist is reportable only when all 180 attempts have exact live graph
evidence and verifier-backed media; a smaller diagnostic run is labeled
non-reportable even when its individual behavior checks pass. Behavioral
failures are still published case by case with their recordings. The
deterministic checklist and source receipt are the authority; any multimodal
model review is a separate, secondary evaluation bound to that immutable
source receipt and cannot convert a failed checklist row into a pass.

Run that secondary pass only after retaining the external source receipt. The
reviewer is an offline plug-in client; it does not start or alter a Realtime
session:

```sh
GEMINI_API_KEY="$GEMINI_API_KEY" go run ./cmd/openrealtime review scenario \
  -source-dir results/candidate-scenario-review \
  -source-receipt results/candidate-scenario-review.receipt.json \
  -out results/candidate-scenario-evaluations \
  -provider google.gemini-3.7-flash \
  -parallel 4
```

The command verifies the complete source population before opening the selected
provider. Each attempt gets a create-only evaluation directory, its own
external sibling receipt, and a link from the case-by-case `REVIEW.md`. The
aggregate `manifest.json` and portable receipt are published only after every
attempt bundle reopens against its receipt, the source re-verifies unchanged,
and the provider closes successfully. A provider failure leaves no aggregate
marker; any completed per-attempt bundles remain diagnostic evidence for that
failed invocation. Evaluation provenance describes a provider-verified
in-process exchange and local digest retention, not independent remote-service
attestation.

Reopen a copied bundle later without a provider credential or network access:

```sh
go run ./cmd/openrealtime review verify-scenario \
  -source-dir results/candidate-scenario-review \
  -source-receipt results/candidate-scenario-review.receipt.json \
  -evaluation-dir results/candidate-scenario-evaluations \
  -evaluation-receipt results/candidate-scenario-evaluations.receipt.json
```

Verification requires both external receipt levels and reopens all source and
evaluation bytes; a local manifest by itself is not authority.

Missing endpoints, provider credentials, reviewed manifests, inspection
authority, or sealed review publication are an unavailable provisioned gate—not a
passing synthetic result. The credential-free WebSocket test above must never
be substituted for the required 180 live candidate attempts or the documented
comparison with the benchmark owner's trusted historical result.
