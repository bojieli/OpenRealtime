# MiniCPM-o gated campaign (in progress)

First full campaign for `native-minicpm-o`: official duplex MiniCPM-o 4.5,
audio only. Gated replay (`wait_configured: true`), binary `openrealtime-d0225ace`.
Started 2026-09-24 at 08:02 UTC. Earlier MiniCPM-o evidence was a
two-recordings-per-category smoke run.

| Category | Applicable passed | N/A | Task errors | |
| --- | --- | --- | --- | --- |
| user_interruption | 14/184 | 16 | 0 | yield p50 466 ms |
| user_backchannel | 91/91 | 7 | 1 | |
| background_speech | 83/83 | 17 | 1 | |
| talking_to_other | 88/88 | 12 | 0 | |

All four FDB categories are complete. MiniCPM-o holds through every
applicable non-interruption, but yields to only 14 of 184 real interruptions.
The FD-Bench condition was still running when this was retained.
