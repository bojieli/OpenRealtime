# Stable component API v1

Import `github.com/bojieli/OpenRealtime/api/v1`. The semantic import path and
`v1.Version == "0.1.0"` freeze the component contract independently from the
experimental engine packages.

Plan version 0.2 includes an implemented experimental canonical-trajectory
continuation design, but it does not change this API. The existing fast-decision
and deliberation roles remain the supported M4 boundary. Replacing them requires
a separate `api/v2` contract and migration guide after live validation. This is
a Go import-path version for OpenRealtime components; it does not imply an
OpenAI Realtime protocol revision.

`docs/api-v2-proposal.md` records what the M10 pilot showed this contract
cannot express. It is a proposal only; nothing in it is implemented.

## Compatibility promise

The project may add optional capabilities, new concrete adapters, helper
functions, or fields whose zero value preserves existing behavior. It will not
remove or rename a v1 interface method, constant, or required field; change its
meaning; or add a method to an existing v1 interface. Such a change requires
`api/v2` and a migration guide. Security fixes may reject inputs that were
always invalid under the documented invariants.

The unversioned `engine`, scheduler, experiment, and internal packages remain
research surfaces and do not carry this promise. Provider implementations
should depend on `api/v1`, not the experimental packages.

## Provider roles

`ProviderSet` contains perception, cognition, speech, fast-decision, and
deliberation roles. Each exposes a nonempty name/version descriptor and explicit
capabilities. Streaming is callback-based: returning from the callback applies
backpressure, and a callback error stops production. Providers must honor a
cancelled context before mutating externally visible state. Implementations
that advertise streaming output must implement `StreamingSpeechProvider`.

Perception revisions are strictly increasing and may not retract the prior
stable prefix. A final revision is terminal. Response candidates name their
source revision and validity. Deliberation updates use contiguous sequence
numbers, exact goal/revision identity, and exactly one final or failed terminal
update. Progress text is observable state, not permission to fabricate work.

## Audio and ownership

Audio is mono PCM16 little-endian unless an adapter explicitly documents a
different provider-side conversion. The OpenAI reference path uses 24 kHz.
`SampleOffset` is absolute within a stream; chunks and frames are contiguous.
Time is monotonic nanoseconds. `EndSample` and `DurationNS` validate zero rates,
empty/odd PCM, and integer overflow.

Callers retain ownership of input byte slices and must not mutate them while a
provider call is active. Providers must not retain or mutate caller memory after
return unless their adapter documentation explicitly introduces an ownership
transfer. The shipped v1 reference adapters clone across the stable/experimental
boundary.

Provider instances are serial by default. An adapter may document stronger
concurrency guarantees, but consumers cannot assume them from v1 alone. A
consumer can run different provider instances concurrently.

## Conformance

Run the complete stable suite:

```bash
go run ./cmd/openrealtime conformance all \
  --fixture tests/fixtures/m0-tone.wav \
  --manifest tests/fixtures/m1-reference-manifest.json \
  --workload tests/fixtures/m4-difficult-workload.json
```

The protocol half checks all 133 pinned profile/direction definitions against
their schema type, registry lookup, and compiled 178-definition schema closure.
It reports 66 unique wire names across GA Realtime, transcription, translation,
and beta profiles and exercises strict profile/direction/required-field faults.

The provider half checks descriptors, required capabilities, cancellation,
frame and revision ordering, stable-prefix monotonicity, candidate identity,
PCM chunk continuity/finality, fast decision identity, and deliberation stream
closure. Use `conformance.RunProviders` with an adapter-specific deterministic
probe before publishing compatibility.
