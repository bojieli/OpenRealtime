# Portable client performance evidence

This package records bounded, payload-free client performance measurements.
It implements the Go portion of the
[`presentation.client.performance_evidence` v1 service](../../docs/composable-presentation.md#81-performance-evidence-service-contract).
Use it when adding a client probe; it does not record conversation content.

## Provider and recorder lifecycle

The client composition runtime mounts a `Provider` through a `MountScope` and
injects a source-scoped `Recorder` into each selected probe plugin. Identities,
source bindings, configuration, and mount generation are frozen before a
recorder exists. Recorder calls accept only metric/dimension enums and unsigned
integers. There is deliberately no field or method for arbitrary labels,
payloads, URLs, host names, session/response/item IDs, paths, device names, or
diagnostic strings.

## Duration buckets

Durations use these fixed inclusive, non-cumulative bucket upper bounds (ns):

```
100000, 250000, 500000, 1000000, 2000000, 5000000, 10000000,
20000000, 50000000, 100000000, 250000000, 500000000, 1000000000,
2000000000, 5000000000, 10000000000, 30000000000, 60000000000,
300000000000, 3600000000000, 86400000000000
```

## Sampling and snapshots

Sampling hashes the schema identity, seed, generation, canonical source index,
metric, canonical dimensions, and zero-based attempt ordinal with SHA-256. The
first big-endian 64 bits modulo the denominator are compared with the
numerator. This specifies deterministic behavior for a future shared corpus;
this Go slice does **not** claim JavaScript or Swift parity.

Snapshots are payload-free, recursively cloned, fingerprinted over compact
canonical JSON, and bounded by both the selected configuration and the absolute
256-KiB ceiling. `DecodeSnapshot` accepts only the exact bytes emitted by
`MarshalSnapshot`, including its single trailing newline.
