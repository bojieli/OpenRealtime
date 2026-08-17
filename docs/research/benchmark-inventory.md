# Benchmark and workload inventory

Retrieved 2026-08-17. “Adapter policy” states the safe initial integration; it
is not legal advice and must be rechecked at the pinned revision.

| Resource | Coverage | Public artifact state | License observation | Adapter policy |
| --- | --- | --- | --- | --- |
| Project controlled timing fixtures | pauses, endpoints, predictable/ambiguous continuations | M0 tone exists; M1 speech pending | original fixtures CC0 | vendored with hashes |
| Full-Duplex-Bench v1/v1.5 | pause, backchannel, turn-taking, interruption, overlap | official code/data repository | repository license CC BY-NC 4.0 | external opt-in; no vendoring |
| Full-Duplex-Bench v2 | dynamic multi-turn examiner | official repository, actively evolving | same repository license; inspect sub-artifacts | experimental external adapter |
| Full-Duplex-Bench v3 | disfluent speech and tool use | code public; data separately downloaded | inspect every sub-artifact | pinned and inventoried; require complete external orchestration before reporting a score |
| FD-Bench | full-duplex response time, interruption handling, WER/BLEU, and subjective quality | code and external audio dataset public | NTUitive license; external assets require review | comparison/inventory only: official pipeline requires Python, PyTorch, and CUDA; selected FDB v1.5 has the more direct commercial overlap matrix |
| LiveKit eot-bench | causal end-of-turn decisions and latency/false-cutoff Pareto frontiers in 14 languages | code, dataset, and reference predictions public | Apache-2.0 stated for code/data | useful component benchmark, not an end-to-end live-system score; official harness is Python |
| TOBench | 100 closed-loop omni-modal tasks, 27 MCP servers, 324 tools | official MIT repository and external task bundle | per-task services and asset terms require inventory | comparison/inventory only: upstream harness requires Python and is not a realtime duplex voice benchmark |
| FT-Bench (TREX) | 10 autonomous LLM fine-tuning tasks | paper and task dataset public | task datasets and training assets require per-task review | out of scope: it evaluates training-agent research workflows, not live speech or realtime protocols |
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

TOBench is relevant to general tool-using agent quality, but it does not isolate
realtime voice turn-taking or provider audio protocols. Its official harness
also requires Python 3.12 plus a large mixed MCP/Node environment. The current
live-provider experiment therefore uses Full-Duplex-Bench v1.5 as the direct
duplex benchmark and records Full-Duplex-Bench v3 as the future tool-use bridge;
it does not relabel a partial TOBench port as a compatible result.

The similarly named 2026 FT-Bench belongs to TREX and measures autonomous LLM
fine-tuning over ten training tasks. It is not a voice-agent benchmark. If
“FTbench” is intended to mean Full-Duplex-Bench/FD-Bench, the live experiment
reported here uses the pinned Full-Duplex-Bench v1.5 corpus directly.
