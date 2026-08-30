# Graph-native interaction-scenario launch contract

This package is the scenario suite's fail-closed composition boundary. It does
not contain a server, model, provider, credential, UI, or binding-name switch.
It decorates an explicitly selected `graph/launch` adapter plugin, checks the
adapter against the exact immutable plan, and returns the prepared
`NativeBinding` for a host to compose as its `server.SessionProvider`.

The complete contract covers the eleven repository-owned interaction cases.
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

`TestProfiledGraphNativeWebSocketExercisesExactElevenScenarioContract` is the
credential-free protocol checkpoint. It starts eleven independent sessions
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
Those claims still require the preregistered live run of all eleven scenarios
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
reviewed architecture cell, authenticated inspection graph, migration
registration, immutable evidence store, speech service, and fifteen
repetitions per case:

```sh
go run ./cmd/openrealtime scenario \
  -url "$OPENREALTIME_BENCH_ENDPOINT" \
  -speech-url "$OPENREALTIME_SPEECH_ENDPOINT" \
  -repeat 15 \
  -architecture-manifest "$OPENREALTIME_SCENARIO_ARCHITECTURE_MANIFEST" \
  -architecture-cell "$OPENREALTIME_SCENARIO_ARCHITECTURE_CELL" \
  -inspection-graph "$OPENREALTIME_BENCH_INSPECTION_GRAPH" \
  -migration-store "$OPENREALTIME_MIGRATION_STORE" \
  -migration-registration "$OPENREALTIME_MIGRATION_REGISTRATION" \
  -migration-registration-sha256 "$OPENREALTIME_MIGRATION_REGISTRATION_SHA256" \
  -migration-arm candidate \
  -record results/candidate-scenario.json
```

Missing endpoints, provider credentials, reviewed manifests, inspection
authority, or migration registration are an unavailable provisioned gate—not a
passing synthetic result. The credential-free WebSocket test above must never
be substituted for the required 165 live candidate attempts or their matched
legacy baseline.
