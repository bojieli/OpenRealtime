# Draft fixture audit, 2026-09-22

The 24 pairs in `bench/capability/fixtures.go` are development candidates. They
cover all eight planned families, but coverage alone does not validate the
pilot. Draft revision 2 changes unused addressing stimuli, supplies missing
task facts, removes intent conclusions from acoustic annotations, and replaces
several truncated lexical stems with explicit word forms. Existing run bundles
retain their original fixtures and scores; they are not rescored silently.

| Family / pairs | Current evidence | Acceptance still needed |
| --- | --- | --- |
| Semantic correction, sc-01–03 | Authored opposite/neutral feedback and shared prefixes | Prepared recordings; actual overlap; correct revised content and neutral continuation; semantic review of lexical false positives |
| Steering, st-01–03 | st-01 has shared recorded input, conservative phrase-end transcript release, live feedback and withheld controls, independent output transcription | Successful opposite audible continuation; broaden to the other topics; validate scoring windows and content, beyond single keywords |
| Prosodic acknowledgement, pr-01–03 | Identical words and times; draft 2 cues describe pitch/duration without supplying agreement or doubt labels | Distinct recordings with measured cues; independent listener agreement; retain ambiguous cases; do not treat the synthetic labels as perception evidence |
| Silence/hesitation, si-01–03 | Same eventual transcript with an inserted pause; one branch scores the unfinished phrase and the other scores completion | Verify actual audio gap and word availability; control removal currently retains the later finishing phrase; validate both waiting and subsequent task fulfillment, not only one silent window |
| Addressing/overlap, ad-01–03 | Draft 2 matches words, speaker and times; only directional cue differs; calendar and map facts supplied | Validate directional recordings and addressee agreement; distinguish inferred direction from known intent; semantic assessment must reject acknowledgements that do not update the task |
| Proactive semantic action, pa-01–03 | Error versus explicitly permitted fictional/deliberate error | Validate timely intervention and false intervention separately; pa-03 needs review that distinguishes identifying an exercise from endorsing its unsafe instruction |
| Concurrent task, co-01–03 | Counting, language switch and running-total changes authored | Playback-aligned number order and incremental response scoring; co-02 Spanish synthesis is outside the validated English/French TTS support; do not count end-of-phrase recognition as incremental-task success |
| Output revision, ov-01–03 | Identical constraint at two authored times | Prove whether relevant words were actually heard at the constraint; a fixed early/late timestamp does not establish that distinction. Validate options/durations/flight facts and repairs against what played |

The current whole-word lexical scorer cannot recognize arbitrary inflections,
negation scope, correct numerical sequences, or whether an option satisfies a
constraint. Explicit terms fix some missed matches without making it a semantic
scorer. For example, the recorded st-01 skip sentence about retrieving website
content contains “request” and passes its lexical screen, but that alone does
not demonstrate an explanation of establishing the connection.

The A1/A2/A3 runner uses a shared nominal 500 ms clock and declared channels.
Actual request admission times can diverge when policy calls overrun that clock.
A1 also sees ordered action history and segment identities, which may provide
indirect timing information. The running development comparison does not prove
the clean P1 timing contrast. Its causal replay audit checks declared-channel
observation rendering; it explicitly excludes the history suffix and audio
annotation validity.

Next execution priority is recorded semantic/steering pairs and their controls,
while completing protocol validation. Prosody, addressing, concurrent timing,
and heard-versus-pending revision retain their original place in the 24-pair
pilot; they cannot be promoted to validated families because easier pairs run.

A nominal native delivery audit of all 24 draft pairs (its evidence bundle was
lost with the previous checkout on 2026-09-23; re-derive before relying on it)
found the following. At one word per 500 ms tick, 15 of 48 branches
finish prefix admission at or after feedback starts; at two words per tick,
none do in the authored annotations. This does not establish audible opportunity:
recorded phrase lengths and conservative phrase-end release change timing, as
already seen in recorded sc01/st02/st03. Before freezing pilot timing, recompute
against prepared recordings and verify substantive played speech when required.
Do not shift already-collected scoring windows to compensate for late output.
