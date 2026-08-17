# M1 reference artifacts

These files are deterministic outputs of `./scripts/reproduce_m1.sh`:

| Artifact | SHA-256 | Purpose |
| --- | --- | --- |
| `reference/report.json` | `9487ea89377219640bde1f6f4d88e6494b63be1ab7aff8ae0540be5ff2e459ce` | 30 raw trials and summarized stage distributions |
| `reference/trial-0000.jsonl` | `f75ad941ace568ce512d6474b8100f4c96b7b2f72b0ebb64a9724a0e817c3bfb` | Representative 66-record causal OpenAI Realtime trace |
| `reference/timeline-trial-0000.html` | `4c3083aa57430281aa624e2acdc384336f692e5731b2c700038182ad42b52996` | Self-contained trace visualization |

The source fixture and manifest are project-authored. No provider response,
model output, human voice, personal data, or imported recording is present.
These generated research artifacts are released under CC0-1.0. See
`docs/m1-baseline.md` for interpretation limits.
