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
  still image, plus visual-input and text-injection capability evidence.

`BuildContract` derives and fingerprints those requirements from the reviewed
suite. `GuardAdapter` is the composable plugin decorator. `GuardLaunch` applies
it to an exact selected `graph/launch` config for direct composition through
`server.NewGraphBundle`. `New` is the provider-only convenience constructor.
Both delegate topology, lock, values, deployment, secret-catalog, assembly,
adapter-artifact, and mount-dependency validation to `graph/launch`.

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
