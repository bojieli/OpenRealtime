# ADR-0002: Separate licenses and require artifact provenance

- Status: accepted; the code license is superseded by
  [ADR-0017](0017-mit-license-for-code.md), which replaces Apache-2.0 with MIT.
  The separation of code, documentation, and fixture licensing, and the
  provenance requirement below, remain accepted as written.
- Date: 2026-08-17

## Context

The project needs permissive, patent-aware code reuse while research data and
documentation have different reuse expectations. Voice data also carries
privacy, publicity, consent, and provider-term concerns not resolved by a code
license.

## Decision

License code, schemas, configuration, and scripts under Apache-2.0;
documentation under CC BY 4.0; and original project fixtures and golden traces
under CC0-1.0. Do not redistribute third-party benchmark data by default.

Every nontrivial artifact must record origin, applicable license, consent or
other lawful basis, transformations, and a cryptographic hash. Generated audio
must additionally identify the generator/model, terms, voice right, prompt,
and generation date. Contributions declare that provenance explicitly.

## Consequences

Software has an express patent grant. Documentation remains widely reusable
with attribution. The open reference condition uses original or clearly
redistributable fixtures. Adapters for noncommercial benchmarks may exist, but
their data stays external and their results must state the usage restriction.
