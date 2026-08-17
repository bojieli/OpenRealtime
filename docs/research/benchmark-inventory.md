# Benchmark and workload inventory

Retrieved 2026-08-17. “Adapter policy” states the safe initial integration; it
is not legal advice and must be rechecked at the pinned revision.

| Resource | Coverage | Public artifact state | License observation | Adapter policy |
| --- | --- | --- | --- | --- |
| Project controlled timing fixtures | pauses, endpoints, predictable/ambiguous continuations | M0 tone exists; M1 speech pending | original fixtures CC0 | vendored with hashes |
| Full-Duplex-Bench v1/v1.5 | pause, backchannel, turn-taking, interruption, overlap | official code/data repository | repository license CC BY-NC 4.0 | external opt-in; no vendoring |
| Full-Duplex-Bench v2 | dynamic multi-turn examiner | official repository, actively evolving | same repository license; inspect sub-artifacts | experimental external adapter |
| Full-Duplex-Bench v3 | disfluent speech and tool use | code public; data separately downloaded | inspect every sub-artifact | defer until M3/M6 |
| SimulEval | simultaneous text/speech translation quality and latency | official repository archived 2025-09-18 | CC BY-SA 4.0 stated by repository | protocol adapter or clean metric implementation |
| Moshi | native full-duplex open baseline | official code and weights public | component-specific terms require inventory | optional N1 adapter; never core dependency |
| Rapid audio games | reaction, timing, rule adherence | project workload pending | original scripts/fixtures target CC0 | build in M5 |
| Difficult questions | foreground acknowledgement plus tool/deliberation | project workload pending | prompts and expected answers must be redistributable | build in M4 |
| Paralinguistic challenge | emotion, sarcasm, laughter, sighs, non-speech | source not selected | voice/performer rights are critical | do not collect until ethics checklist |

## Selection gates

A benchmark enters a release only when its exact revision, task taxonomy,
metric implementation, license, data origin, speaker rights, redistribution
policy, required models, expected compute, and known limitations are recorded.
Access alone does not imply redistribution permission.
