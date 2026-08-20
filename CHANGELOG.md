# Changelog

## v0.1.0 — 2026-08-17

Pre-1.0: the component contract at `api/v1` is frozen in shape, but the
program is unreleased and the M8-M10 live and study milestones are still
open. The `api/v1` import path is the Go semantic import path, not a claim
that a 1.0 release has been cut.

- Freeze the provider-neutral component contract at `api/v1`.
- Cover every event in the pinned OpenAI GA Realtime, transcription,
  translation, and legacy beta profiles: 133 profile/direction definitions and
  66 unique wire names.
- Add strict schema, direction, profile, causal-trace, and stable-adapter
  conformance suites.
- Ship versioned reference adapters, compiled examples, M0–M6 benchmark
  reproduction, and the SHA-256-pinned benchmark release.
- Preserve explicit limitations: no native-provider, natural-language quality,
  live transport, or participant-study result is claimed.
