# Contributing to OpenRealtime

OpenRealtime is a research project. A change is reviewable only when its origin,
behavior, and effect on the evidence are inspectable.

## Development setup

Install Go 1.25 or newer, then run the gate:

```bash
./scripts/check.sh
```

That is the whole gate and the same one CI runs: formatting, `go vet` and
`go test -race` across every module, the protocol conformance suite, the
prompt-injection release gate, the examples, and the shell scripts. It runs
offline in a few minutes, and it runs every stage before reporting, so a run
tells you everything that is broken rather than the first thing.

The scripts resolve the toolchain from `OPENREALTIME_GO_BIN`,
`/usr/local/go/bin/go`, or `PATH`, in that order. Use the same helper for
ad-hoc commands when an older system `go` comes first on `PATH`:

```bash
source scripts/go-toolchain.sh
go_bin="$(openrealtime_go_bin)"
"$go_bin" test -race ./eventloop/...
```

What needs a model, a dataset, or a GPU is deliberately not in the gate. The
measurement suites (`openrealtime bench`, `scripts/prepare-*.sh`) are reported
rather than gated, because a gate that cannot run offline eventually fails for
reasons that have nothing to do with the change in front of it.

The engine, the protocol, and the tools are Go, and protocol code is generated
by `cmd/specsync` rather than by hand. Python appears in exactly two places, in
both cases because the thing it talks to is Python: the sidecar library and
reference sidecars under `sidecars/`, and the τ-Voice runner. Neither is on the
server's runtime path — a sidecar is a separate process the engine speaks a
documented protocol to.

## Contribution requirements

Every pull request must:

1. Explain the research or engineering question it addresses.
2. Declare the origin and license of imported code, data, prompts, model
   weights, and generated assets. Write “none” when there are none.
3. Add tests for state, ordering, revision, cancellation, and time invariants
   affected by the change.
4. Record exact model, prompt, provider, fixture, schema, and configuration
   versions for performance evidence.
5. Report negative or ambiguous measurements alongside positive results.
6. Avoid committing credentials, private audio, or provider output whose terms
   prohibit redistribution.
7. Keep the OpenAI Realtime wire shape compatible. Project timing, causality,
   and experiment metadata belongs in the separate trace envelope, never in a
   client or server event sent on the wire.
8. Treat `api/v1` as stable. A breaking interface or semantic change requires a
   new `api/v2` import path, migration documentation, and a major release.
9. Treat canonical trajectory and reasoning data as internal. Raw reasoning
   retention must be opt-in, provider-permitted, consented where applicable,
   and unnecessary for ordinary wire compatibility or timing evidence.

Contributors certify that they have the right to submit their contribution and
license it under the repository’s applicable license. Substantial architecture
or protocol changes should start with an ADR in `docs/adr/`.

## Benchmarks

Performance pull requests must include the command, raw trace, hardware and
network metadata, repeated-trial distribution, and comparison baseline. A
single best run is not evidence of an improvement.
