# OpenRealtime benchmark release v0.1.0

This directory is the machine-readable M0–M6 reference release.

- `study.json` separates comparability groups, publishes paired effects,
  evidence-scoped claims, counterexamples, missing comparisons, and limitations.
- `preregistration.json` freezes the prospective human-study plan and explicitly
  records that it has not been run.
- `manifest.json` records byte size and SHA-256 for every released source,
  fixture, report, representative trace, schema, and research document.

Verify it without Python or a provider account:

```bash
go run ./cmd/openrealtime release verify \
  --root . benchmarks/releases/v0.1.0/manifest.json
```
