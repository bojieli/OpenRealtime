# M3 reference artifacts

`./scripts/reproduce_m3.sh` regenerates 30 trials for each of four scenarios.
The full raw metrics are retained in `reference/report.json`; one complete
OpenAI-compatible trace and timeline per scenario is checked in.

| Artifact | SHA-256 |
| --- | --- |
| `reference/report.json` | `056a80b85eb67d589a81454c28cdb0851f2c8bc54cb655e6177f9b01bf0f3894` |
| `reference/directed_interruption-trial-0000.jsonl` | `2b117883c153c2851aa71d04cc057d08bb950a42115f2e130328973351d33056` |
| `reference/listener_backchannel-trial-0000.jsonl` | `d9b9d7b6ac3ce31f2d2ee240fa9b28d7f76a14c72c310b46cfad7faf49a487ee` |
| `reference/side_speech-trial-0000.jsonl` | `406c2b6fc2fc2372f71233134307b3da863fabbd3cce6463888f0a38a6759a09` |
| `reference/invalidation_repair-trial-0000.jsonl` | `81389e671ff4a9e377199fe70869ca953e0267f02adad39ab79bdab914d2d469` |
| `reference/timeline-directed_interruption-trial-0000.html` | `94078e76449b0dd4c0485e2ed0ef3c72a8311f6940cbb03e3dc4af40d94851d7` |
| `reference/timeline-listener_backchannel-trial-0000.html` | `3a46b5cb19512feaf16eecc75633ac5684ae9bc2f83e9e79ed4fb937413e3fc3` |
| `reference/timeline-side_speech-trial-0000.html` | `63267ad5b0d1a1968032ecd5b456d1c9d2da445618bef244a56730c39b808442` |
| `reference/timeline-invalidation_repair-trial-0000.html` | `a6ac1e3f50890e89e64bda39bf6ac0b7dd5a25c7610bce835179d5c229443dfa` |

All content is project-authored CC0-1.0 deterministic instrumentation. There
is no human voice, third-party benchmark data, or provider output.
