# Sub-turn interaction benchmark study

Status: 2026-09-05. This note records the retained twelve-case checkpoint and
the paired Full-Duplex-Bench (FDB) and FD-Bench diagnostics that followed it.
These are intentionally focused diagnostics, not release results: the complete
FDB v1.5 population is 498 recordings, each selected FD-Bench condition has 293
conversations, and every limited cell below is marked non-reportable by the
runner.

## Runtime under test

The tested cascade is:

- Deepgram Nova-3, `en-US`, streaming partials at a 100 ms cadence;
- the local Qwen `qwen-fast` interaction policy (`qwen3-vl-30b-a3b`), which
  chooses the interaction act for transcript events; and
- Gemini `gemini-3.5-flash` for spoken cognition and background reasoning,
  with effort/reasoning budget 512, followed by Fish Speech synthesis.

The word-timing endpoint is the local faster-Whisper service. The benchmark
client reached Google through the standard `HTTPS_PROXY` endpoint
`http://23.135.236.242:3128`; local services and `api.deepgram.com` were kept
in `NO_PROXY`. No credential value is part of any retained artifact.

The twelve-case checkpoint and FDB study use the code and shipped Deepgram
profile at commit `8e7065b99623` (`interaction: classify active transcript
overlap`). The profile keeps the narrow local Qwen classifier on ordinary
active-output transcript events: directed speech yields `stop-speaking`, while
a listener backchannel or side speech yields `keep-speaking`. A protected
same-stream continuation still uses the general event policy so
correction/resume behavior is not lost. The later FD-Bench v7 study uses commit
`01c82bdd8363` and the same runtime providers after extending the runtime-note
quarantine to the strict graph-native cognition stream.

## Twelve-case retained checkpoint

The complete graph-native scenario checkpoint is retained under
`.runtime/deepgram-scenario-media-v28`:

- 12 canonical 24 kHz stereo PCM16 WAVs, one for each case, in
  `diagnostic-12x1-20260904-01/`;
- deterministic behavior: **12/12 passed**;
- Gemini multimodal advisory review: **12/12 evaluated and passed**, with
  usable media and agreement on every case;
- source receipt:
  `sha256:6be3a0ee4b405af7d543cfa3ff5910ed888481ef6a182bae3487ab62da50bf7e`;
- advisory review receipt:
  `sha256:5d8fbd014096e50d56fafe81059f891a5a7ee00be2ae31cc5333de3c73381e26`.

Case 12, **“picking up where it was cut off,”** is present in the source
manifest as ordinal 12, trial 1, with behavior `passed`. Its retained WAV is
`12-picking-up-where-it-was-cut-off-trial-01.stereo.wav` and has digest
`sha256:19a9458cf80883850fa342eb664323e97605ac0e6672eefaab7ae630732495b7`.
The per-case Gemini evaluation is also retained and reports `pass`.

The review directory contains a second, content-addressed copy of each WAV for
the reviewer. Thus a recursive `*.wav` count is 24, while the canonical source
record is exactly the 12 stereo WAVs above. The source and review bundles were
reopened with `review verify-scenario` after case 12 was added; all 12 source
and 12 evaluation records verified against their receipts.

The retained bundle is immutable evidence for the profile run that produced
it. It predates deterministic scenario scorer version 2, which rejects word
fragments and requires every appointment detail in the translation case. Its
12/12 score remains the original checkpoint's result; it has not been rescored
or promoted under those stronger checks. Version 3 additionally requires
recorded acoustic continuation across both acknowledgements; the separate
waveform audit below exposes a failure in that historical recording.

A later live diagnostic found that a model could copy the reserved
`[runtime: ...]` note used to describe prepared-but-unheard speech into its
answer. The legacy continuation runner now removes that annotation before
building `RunResult.AssistantText` or committing assistant trajectory content.
The strict graph-native `cognition.TextModel` also applies a bounded streaming
filter before publishing prepared text: it handles markers split across
provider chunks without buffering the ordinary response, and the same
sanitized bytes form its completed result. Continuation, graph-native cognition,
and cascade regressions cover both boundaries. These repairs do not rewrite or
re-label the sealed v28 recordings, and scratch reruns remain non-reportable
unless they acquire a new graph-native source receipt.

The canonical source media index is:

| # | Case | WAV | SHA-256 |
| ---: | --- | --- | --- |
| 1 | count-as-they-go | `01-count-as-they-go-trial-01.stereo.wav` | `f11735b91ff5a9c195835c33ac8bb99f33052a297e819ba6a884b9190331ec45` |
| 2 | asked not to be interrupted | `02-asked-not-to-be-interrupted-trial-01.stereo.wav` | `e7b717b7edeb4125ea6c11eac7d60a98bf19d7a3f615b777cb86b8a62cb7840c` |
| 3 | a recorded menu | `03-a-recorded-menu-trial-01.stereo.wav` | `b9f691e8ef26cf8ac4f397e5bb2c4226685d9b5fd1219f41cb8d73a8e837e063` |
| 4 | cutting in on something wrong | `04-cutting-in-on-something-wrong-trial-01.stereo.wav` | `93cf6dfcd62f88cd49141167f29d3e843ae01684322955fc002e5e5ad1b9fa8e` |
| 5 | ordering from a waiter | `05-ordering-from-a-waiter-trial-01.stereo.wav` | `d0224999e2119569180b1a05b0737625988ae73a9aaa55336821447cdaf09954` |
| 6 | translating as they speak | `06-translating-as-they-speak-trial-01.stereo.wav` | `4b159a3f685ad87868ccdef4b525ce1dacab304bc8084e6a6540c10882ac533e` |
| 7 | waiting out a silence they asked for | `07-waiting-out-a-silence-they-asked-for-trial-01.stereo.wav` | `d919a68abf4c771375092c85ccc821a1704e4a8b5f29e24d57b68304a3ae136f` |
| 8 | somebody else's conversation | `08-somebody-else-s-conversation-trial-01.stereo.wav` | `b3672b08784af782eb61c7bbcea12ad3ac5871b56cb9f24e6d582da18a70d41d` |
| 9 | an acknowledgement is not an interruption | `09-an-acknowledgement-is-not-an-interruption-trial-01.stereo.wav` | `e8c5b653c74f744024ca32bc6e9a385e35fb50064ec48f43a1bb9acc031662a5` |
| 10 | telling them what it saw | `10-telling-them-what-it-saw-trial-01.stereo.wav` | `d13d125a7186bd26b32692bb16c9d35dd4ad7d2e03a8629d22e12b8e1a748b18` |
| 11 | an ordinary question | `11-an-ordinary-question-trial-01.stereo.wav` | `18b207e19d476e78ab10fb0340d3ce882909205063156c382f59b30e7701a56b` |
| 12 | picking up where it was cut off | `12-picking-up-where-it-was-cut-off-trial-01.stereo.wav` | `19a9458cf80883850fa342eb664323e97605ac0e6672eefaab7ae630732495b7` |

## Acknowledgement waveform audit

The 2026-09-05 audit applied scorer version 3's new `held-across` checks to
the retained case-9 agent channel. It verified the original stereo WAV digest
`sha256:e8c5b653c74f744024ca32bc6e9a385e35fb50064ec48f43a1bb9acc031662a5`.
Line starts are 9,000 and 11,500 ms from the unchanged authored script; their
ends, 9,696 and 12,800 ms, come from the source result's retained latency
anchors. Neither line was displaced by composition's 600 ms minimum breath
after the preceding line.

| Acknowledgement | Active before | Active during | Active after | Longest interior pause | Version-3 check |
| --- | ---: | ---: | ---: | ---: | --- |
| Mhm | 1,000 ms | 696 ms | 740 ms | 260 ms | Pass |
| Right, yeah | 1,000 ms | 200 ms | 0 ms | 280 ms | Fail: no continuation afterwards |

The old scorer passed this recording because it required earlier speech and
the absence of a few restart phrases, without requiring continued speech.
The waveform does not demonstrate continuation through the second
acknowledgement. This observation alone does not establish whether the agent
yielded or naturally finished its explanation; either way, the required
continuation was not demonstrated.

This is a new analysis of one historical recording, not a new execution or a
version-3 score for the full suite. The original 12/12 checkpoint and its
receipts remain unchanged. The diagnostic command, input hashes, exact scorer
revision, and output are retained separately under
`artifacts/scenario-acknowledgement-hold-v3-20260905`.

## Twelve-case diagnostic on the 2026-09-05 tree (v29)

A second single-trial checkpoint, `.runtime/deepgram-scenario-media-v29`, was
frozen from clean revision `d70cd903a300ab9fb5c3e7b1034201f9e7a2dc26`
(executable `sha256:9dd088496342d4ae27e21bb04bc6cfe54970ed9af07b779ca2b6f75826c1ac6a`)
with the same Deepgram Nova-3, local Qwen policy, Gemini 3.5 Flash, Fish, and
faster-Whisper selections as v28 and the transcript rules the tree shipped at
that revision. Those rules are longer than v28's: rule 1 of both the partial
and final rules carries the floor-taking, vocative, and acknowledgement
priority decisions added on 2026-09-04. The graph fingerprint moved to
`sha256:0d5b4c4b511508b7ef0752e2f74b75723cbbd3288dd48e88e6d947d681ec8379`
because the cognition stream gained its runtime-note filter after v28.
`bench architecture inspect` still refuses the sparse status a launch-profile
server emits, so the cell carries the v28 architecture declaration with the
live profile digest and the instruction-revision pin replaced;
`config/authoring-limitations.json` records that, and the per-attempt
authenticated inspection binds what executed.

Two runs were made. The first, `diagnostic-12x1-20260905-01`, was scored
against a cell that still named the v28 profile digest and is therefore
non-reportable as a cell; it passed 11/12, failing *ordering from a waiter*:
the agent did not speak through when the sea bass was named at 25.5 s and
answered only at 29.9 s, after the waiter's next line, whereas v28 spoke
through at 25.4 s. The second, `diagnostic-12x1-20260905-02`, against the
correctly authored cell, is a reportable architecture cell and passed
**12/12**, including the waiter. The single-trial split between the two runs
is the variance the fifteen-repeat design exists for; the waiter case was 8/15
in the accepted eleven-case baseline.

The Gemini 3.7 Flash advisory review of the second run evaluated 12/12 with
usable media and agreed on 11. It observed a failure in *count-as-they-go*:
the agent counted both animals on time but first said "I will count the
animals as you mention them," breaching "and say nothing else" and overlapping
the user's next line. The scorer at revision `d70cd90` did not check that
constraint, so the deterministic pass stands for that revision; the later
scorer-v7 content checks are the place such a rule belongs.

- source receipt `sha256:91869c6295333b4e346897f6cd742d0125aef80c47d476833237c9eae4ca650d`
- advisory review receipt `sha256:06eeff42c52779a05a158476d557d7f96e65e481220468566555f2d4fcc7045a`

Both source and evaluation bundles reopen with `review verify-scenario`
(12/12 verified). This is a one-attempt diagnostic of that revision, not the
fifteen-repeat campaign, and it adds no final-candidate credit.

## FDB v1.5 paired diagnostic

The frozen study is `.runtime/fdb-subturn-study-v4` (binary digest
`sha256:7f46c8ba7050d6a8bbcb61ba4b6fdf63835eeeb082adefd14923ff1c669fdb78`).
Both arms use the same Deepgram, Gemini, TTS, word-timing, graph, and dataset
artifacts. The only intended factor difference is the transcript-event policy:

| Arm | Cell / F8 | Profile fingerprint |
| --- | --- | --- |
| Baseline | `fdb-en-turn-boundary-focused-v4` / `turn-boundary` | `sha256:a665e411fd3806010c30a07edc6b894c3b5131a6465f3bff8fe1d9d72a65d2dc` |
| Treatment | `fdb-en-subturn-qwen-focused-v4` / `subturn-qwen` | `sha256:e65c31b0567329a55ea46ea99ae1979578d704836e8325bbadf8e2e03696e5f4` |

After removing the policy-act list, the normalized profile values have the
same digest `sha256:0d2948cec621b0570128d240ecb7c61009d14921c1cc94bba17a3dcc28c9c7bc`.
Each arm retained 20 WAVs (five per category), and each of its four Gemini
review bundles evaluated all five recordings.

### Results

The deterministic scorer is authoritative. “Gemini observed” is the retained
multimodal review and is advisory; it can identify audible truncation that is
outside FDB’s one-second mechanical hold window.

| Category (dataset population) | Baseline deterministic | Treatment deterministic | Baseline Gemini observed | Treatment Gemini observed | Relevant latency (baseline → treatment) |
| --- | ---: | ---: | ---: | ---: | --- |
| `user_interruption` (200) | 1/5 | 1/5 | 1/5 pass | 1/5 pass | yield p50 1,351 → 1,297 ms; mean 1,246 → 1,251 ms |
| `user_backchannel` (98) | 5/5 | 5/5 | 5/5 pass | 4/5 pass | hold mean 929 → 929 ms |
| `background_speech` (100; 4 applicable) | 4/4 applicable | 4/4 applicable | 5/5 pass | 5/5 pass | hold mean 975 → 790 ms |
| `talking_to_other` (100) | 5/5 | 5/5 | 5/5 pass | 5/5 pass | hold mean 985 → 966 ms |

The small paired slice therefore shows no deterministic score change from the
sub-turn policy. The treatment’s one advisory disagreement was
`user_backchannel/5`, where Gemini heard a false barge-in and mid-word cutoff;
the baseline recording of the same fixture also ended abruptly, but the
advisory reviewer classified it as a pass while noting speech truncation. Both
arms’ interruption tails are above the 1,000 ms FDB allowance, so this slice
points to endpoint/model/TTS latency work as well as semantic classification.
Neither arm should be advertised as a complete FDB pass from this diagnostic.

The failed first attempt at the treatment interruption run,
`20260904-01-user-interruption.source`, is preserved as non-evidence. It was
created with a display name containing hyphens; the actual category identifier
is `user_interruption`. It is not included in any result or receipt above.

### Receipt index

These are the create-only source and aggregate-review receipts for the eight
focused cells. They provide the evidence chain without embedding media or
credentials in this repository:

| Arm / category | Source receipt | Gemini aggregate receipt |
| --- | --- | --- |
| treatment / interruption | `sha256:0236dfd4c12706d10262f440c1a4636df0dc2f7333ff43288d4c4ab3d9b3eded` | `sha256:cc63a2e74404b3061afdde800c3854b1dd162bb49bfa057946a055bccb9ed70b` |
| treatment / backchannel | `sha256:9a0bd9145f0888e45e678e5528154f869bc23c367cab32a65c19c9dfac06ddc4` | `sha256:45980f1c06507ff95672fb925597dc218dcc55bb776f166deb2dcbb84bcb3518` |
| treatment / background | `sha256:d71cd0ee338846022c0b94bb4e46b0a548647aa93751fd42b14311e9b8dd5914` | `sha256:54f3acff85f2088710fa00d8e4ff7c28b2f4872001a22d5eccf17e5a5834b15c` |
| treatment / other | `sha256:109e580a63e25ecc53936fb05df8c734426d7ad720a407d33ad693b30f95609a` | `sha256:a0adb056ec87e8f734e00fe2b330791a41381975feb72ed3b5c1d5b627f58ae0` |
| baseline / interruption | `sha256:691f35aacb1fdf558ce4ca54c8b1e7aa9542aacfe665e17a94b1ca203bffc4bc` | `sha256:9ca6e687f73b02f34970765e40cb2fe0aa356d3a82063a974a53af1bbc246abe` |
| baseline / backchannel | `sha256:270c4f06621bac8ef05c68d6a768baa6f8bac646066a1a7bcbeeffe94d8dbb5e` | `sha256:e292ee996ae8810adbd08cf01c6c44c975256c98f863dca342a568183e34cb02` |
| baseline / background | `sha256:bbb116ca2927c07552c64ce1a51cfadc62deb91d04b3936b9113293fc86c09ff` | `sha256:dfc39c1fb4ac4a0cfddfe25737c4bc0aefbe42e237fbeb70c8bf453701f52459` |
| baseline / other | `sha256:f099274e6df5ee6a4ffd38d9eb37ee177e1556178fafc88a32a37c86c20c6c84` | `sha256:d125dad26f26fadb3562a6bd35a29810fe7ba38f8ba3c32d068fd05920cf0cd2` |

## Focused acknowledgement diagnostic with scorer version 3

The 2026-09-05 focused run is retained in
`artifacts/scenario-focused-acknowledgement-20260905-02`. It executed one case
through the public graph-native CLI at revision
`86f171235597afb7c4abbd2958efacb43b2600da`, with executable digest
`sha256:440119b9a18d3b6d5c8099a7ac966ae35e0c05aede0ed3554f5131b52fdd6ead`.
The result is **1/1 completed, 0/1 passed**, with authenticated execution
evidence, one canonical 25.078-second stereo recording, and one independent
Gemini 3.7 Flash advisory evaluation. The reviewer found usable media, reported
`fail`, and agreed with the deterministic result. Both source and evaluation
were independently reopened by `review verify-scenario`.

- Source receipt: `sha256:52bbb2486a0f0c002dd7da709a5c943048df06fa3d28a412d77ef55956d7019c`.
- Advisory receipt: `sha256:0a4335cc0bf4bcf835639889285437d8934499d3f7cc8e0bd14069dff5527dbd`.
- Source WAV: `source/01-an-acknowledgement-is-not-an-interruption-trial-01.stereo.wav`,
  digest `sha256:9d5ad3393d457fc53b90ac8da4da56181403c544ede5327e1e1e701a18cc0604`.

| Trigger | Before activity | During activity | After activity | Longest interior pause | Check |
| --- | ---: | ---: | ---: | ---: | --- |
| Mhm, 9,000–9,510 ms | 720 ms | 510 ms | 1,000 ms | 0 ms | Pass |
| Right, yeah, 11,500–13,078 ms | 1,000 ms | 480 ms | **0 ms** | 180 ms | Fail |

An independent Python calculation from the retained WAV's agent channel
reproduced every activity and pause measurement exactly; its program and output
are retained in `.support/audit.py` and `waveform-audit.json`.

This failure also exposes an authoring limitation. The script asks for a long
refund explanation without supplying a refund policy. The first agent text was
“I do not have any information about the refund process in my current context.”
Its response completed at 12,318 ms. A second short confirmation started after
the acknowledgements. The absence of acoustic continuation is established;
whether the first response naturally finished or was interrupted is not
established by these records. The advisory review's causal interpretation does
not settle that question. The next scenario repair should provide concrete
source content for sustained speech and distinguish normal completion from a
policy-driven stop before attributing the failure to interruption handling.

The profile reused the v28 provider selections and instructions with current
code, plus explicitly hashed cached participant PCM. Those PCM durations differ
from the v28 retained checkpoint, so this is not a paired performance
comparison. Historical architecture declarations are metadata only: the normal
`bench architecture inspect` authoring command still refuses sparse graph-native
status. A protocol helper captured actual graph/binding/adapter status; the new
Graph IR, execution requirement, and per-attempt authenticated inspection bind
what executed. `authoring-limitations.json` records this distinction.

The preceding `...-01` directory retains both that authoring refusal and a
pre-media executor refusal: subset ordinal 1 was incorrectly checked against
full-suite ordinal 9. The live executor now freezes the selected contract,
rejects changed fixtures and mismatched ordinals, and retains media under the
subset's own case order. CLI/profile selection, retention, direct executor,
negative mutation, and full repository gates passed before the new run.
No historical result was rewritten, no fifteen-repeat case gate closed, and
the full-suite/final-candidate ledger remains unchanged.

## Grounded acknowledgement diagnostic with scorer version 4

The next three-trial diagnostic is retained in
`artifacts/scenario-grounded-acknowledgement-20260905-01`. It used clean revision
`51623d0a2db965058944a50a5f5fae646d1c94b5` and executable digest
`sha256:6ea01910cbf5ded2333bbd96b8c9c6c4ce3bb993d6496762f2b88d1fd81d4e29`.
The scenario now supplies a fictional five-step refund policy and requires
the order number, return label, and original payment method in the answer.
The spoken prompt, acknowledgement cues, acoustic thresholds, and cached
participant PCM are unchanged from the preceding focused diagnostic. This is
a changed-fixture diagnostic, not a paired runtime performance comparison.

All **3/3 attempts completed and 0/3 passed**. All three produced the required
content details; each failed the first acknowledgement's lookback because
the agent had not started speaking before 9,000 ms. Independent Gemini 3.7
Flash review reported usable media and an agreeing failure for every trial.
All three source recordings and evaluations reopened against their external
receipts:

- Source receipt: `sha256:3d13d9aa2060c4e6e6063a033b48e4d43570ad2ec7d8192c2b5ec4a3a8f02ee0`.
- Advisory receipt: `sha256:7113889c4c21d4a96195ca1f6d5a21c17edee4e22c4fc8069b2c1f9ed781d12c`.

| Trial | First audio playout | First response cancelled | Mhm activity before/during/after | Right, yeah activity before/during/after |
| --- | ---: | ---: | --- | --- |
| 1 | 9,030.638 ms | 12,975.662 ms | 0 / 410 / 580 ms | 780 / 1,320 / 660 ms |
| 2 | 9,110.785 ms | 13,047.777 ms | 0 / 290 / 540 ms | 1,000 / 1,418 / 522 ms |
| 3 | 9,205.636 ms | 12,989.534 ms | 0 / 190 / 600 ms | 1,000 / 1,160 / 538 ms |

Version 4 now preserves response IDs, serialized audio playout positions,
terminal status, and terminal reason. The initial response in every trial
reported `cancelled` with reason `turn_detected` during the second
acknowledgement. The isolated server also logged the transcript-event policy's
explicit `stop-speaking` decision at the corresponding time. This is stronger
evidence than inferring cancellation from silence. Subsequent responses
continued later parts of the explanation; `completed` statuses on those
responses cannot erase the first response's cancellation. The decision
provider's exact request/reply was not retained, so its internal classification
cause is still unproven.

Two follow-ups remain. First, the fixed 9,000 ms cue did not encounter an
already-speaking agent in any of these trials. The first hold therefore
measures startup timing as well as acknowledgement handling; an additional
case anchored to observed speech would separate those claims. Second, audio
from subsequent responses satisfied the second acoustic hold even though
the first response was cancelled. A positive turn-preservation claim needs
an additional cancellation/continuation invariant; waveform continuity alone
does not establish it. Version 4 exposes the response evidence without
changing the acoustic check into a semantic or causal judge. No passing case
or acceptance credit is inferred from those individual acoustic windows.

Independent Python analysis of each retained WAV exactly reproduced all six
hold measurements. The full repository gate passed, and mutations dropping
terminal status, shifting playout, joining on arrival time, borrowing another
response's terminal status, and aliasing the caller's review measurements each
failed. The new binary also reopened the historical twelve-case checkpoint
and all 498 FDB source/evaluation records without changing their receipts.
The profile, runtime status, graph execution evidence, commands, WAV hashes,
and metadata-authoring limitation are retained with the diagnostic. The
fifteen-repeat acknowledgement gate and final-candidate ledger remain open.

## Acknowledgement cancellation audit with scorer version 5

`artifacts/scenario-acknowledgement-hold-v5-20260905` retains an offline
reinterpretation of all three grounded v4 recordings. Scorer revision
`4a16480f9366d0b30b2adba97c2093b19f372c23` independently reopened the original
source and advisory receipts above and verified every original outer checksum.
All 93 source-artifact files remain unchanged. This is zero new live attempts
and carries no full-suite or final-candidate credit.

All three still fail the first hold for zero activity before the fixed cue.
Each now also fails the second hold for the initial response's recorded
cancellation at 12,975.662, 13,047.777, and 12,989.534 ms respectively. The
second hold ends at 14,078 ms. Later audio cannot erase that interruption.
Every waveform measurement and response-to-playout join is unchanged; only
the deterministic interpretation advances from version 4 to version 5.

Version 5 requires recognized, unambiguous response outcomes and attribution
of active PCM to recorded response audio intervals. An overlapping response
that reports cancelled, failed, or incomplete at or before the hold ends
cannot earn credit, including when prefetched or replacement audio fills the
acoustic window. An abort after the window does not retroactively fail the
hold. Completed speech segments may follow one another naturally. This is a
bounded protocol-continuity check, not a claim about the cancellation cause or
complete semantic fidelity. The runtime repair and a stimulus anchored to
observed speech remain open.

The retained audit includes the exact executable digests, three rescored
results and cue timelines, reproduction scripts, source-verification output,
and checksums. The new regression first reproduced the v4 false passes.
Five mutations were caught, including dropping the verdict at the actual
WebSocket-to-WAV-to-review boundary. Restored scenario packages and the full
repository gate passed; the optional official SDK checks were explicitly
skipped because that dependency was absent in the isolated worktree. No new
advisory evaluation, live pass, fifteen-repeat case gate, or release credit is
inferred from this offline audit.

## Speech-anchored acknowledgement diagnostic with scorer version 6

`artifacts/scenario-speech-anchored-acknowledgement-20260905-01` retains three
new trials of the optional `acknowledgements during observed speech` case,
from clean revision `bbf6c7003ca47d4231b2af36e4eb0be202bc83db`. All three
completed and none passed. Independent Gemini 3.7 Flash reviews found usable
media and agreed with every failure. Both source and advisory bundles reopened:

- Source receipt: `sha256:d74c20a760e430f70ac441a94b87aef843fd184c7e2eabac93a1db9bc5043a31`.
- Advisory receipt: `sha256:42785593889eaef28934439aa546a86bf0030919986d10e7bdb44ee90cccd7bb`.

The new driver sends pre-synthesized cues only after at least 600 ms of audible
agent PCM in the preceding second, with activity in the last 100 ms. The first
cue started at 9,900, 11,100, and 9,800 ms; the second at 11,500, 12,300, and
11,500 ms. All six cues were fully sent at independently verified opportunities.
The separate Python WAV audit reproduced their activity, exact input hashes,
sample counts, and actual hold anchors. The original startup-timing failure is
therefore separated from overlap behavior. Trial 3 still had only 31 ms of
activity during its first cue; all three had first-response cancellation during
the second hold, at 13,068, 13,799, and 13,116 ms respectively.

The existing policy client capture hook retained 72 request/final-choice
exchanges, including timing and errors, with no headers or hidden reasoning.
In every trial the overlap classifier proposed `listener_backchannel` for
`Right?`, the binary validator returned `not_backchannel`, and fallback chose
`directed_speech`. The server then recorded explicit transcript-policy
`stop-speaking`, followed by response cancellation with reason `turn_detected`.
This identifies a repeatable decision path; a controlled policy comparison is
still needed to attribute the rejection specifically to ASR punctuation.

Scorer v6 validates cue policy/identity, successful transmission, recorded room
PCM, and independently recomputed agent activity before resolving checks to
actual positions. Missed opportunities and partial sends fail. The default
contract remains twelve cases and 180 attempts; the optional extension is
never full-suite reportable, including at fifteen passing repetitions. Full
gates and six mutations passed, with the absent optional official SDK reported
as a skip. The temporary server was stopped. All failures remain retained;
the runtime repair, repeated-case requirements, and final ledger remain open.

## Backchannel validator repair and repeated speech-anchored diagnostic

The live baseline's exact `Right?` validator requests motivated a controlled
comparison retained in `artifacts/scenario-backchannel-validator-study-20260905`.
The preregistered diagnostic contains the three retained contexts plus nine
pure-continuer and fourteen non-backchannel word variants, repeated twice.
The original prompt produced 42/52 expected outcomes; the revised prompt
produced 52/52. Independent replay through the shipped overlap classifier and
provider adapter reproduced the same 42/52 versus 52/52. The original failed
both repetitions of the three retained requests, `Right?`, and `Okay?`; all
fourteen negative variants remained non-backchannels after the change. These
are small provider diagnostics, not a replacement benchmark population.

Revision `15a51f7619e8a5e298b5ef0487e530b3e052c312` clarifies that ASR punctuation
alone cannot turn a pure continuer into a question. Actual lexical/contextual
requests, disagreement, and floor-taking remain excluded. It changes the
validator prompt, not canonical transcripts or a runtime word whitelist.
The full repository gate passed, with the optional official SDK explicitly
skipped because its dependency was absent.

Three new live trials are retained in
`artifacts/scenario-speech-anchored-acknowledgement-20260905-02`, using executable
`sha256:7d27dfa8961e8bd9a3a6bb55576bca2c0ab310a998708062c3f6fe6c74dc201e`.
All three completed and **2/3 passed**, versus the baseline's **0/3**. Independent
Gemini 3.7 Flash review reported usable media and agreed with each outcome.
The source and evaluations independently reopened:

- Source receipt: `sha256:3d12efc7666edcc249664fd22092c01d0c1e641e90f0c72d8c2539eb0a0efc59`.
- Advisory receipt: `sha256:5fd79c827b09838d15d9a9963d5c851da35da3e58de3b63bee9f6b635ccc261b`.

| Trial | First cue | Second cue | First hold before/during/after | Second hold before/during/after | Result |
| --- | ---: | ---: | --- | --- | --- |
| 1 | 11,300 ms | 13,500 ms | 680 / 511 / 449 ms | 640 / 1,099 / 1,000 ms | Pass |
| 2 | 11,000 ms | 12,200 ms | 600 / 511 / 829 ms | 1,000 / 1,219 / 1,000 ms | Pass |
| 3 | 9,700 ms | 11,500 ms | 660 / 20 / 991 ms | 1,000 / 1,340 / 859 ms | Fail: first-cue overlap activity |

All six cue opportunities and exact input PCM hashes independently reproduce
from the WAVs. Every response overlapping the six holds reports `completed`,
and the validator accepts `Right?` in all three trials. The repeated baseline
cancellation is absent. Trial 3 still pauses after `First,` before the next
phrase; its 20 ms overlap activity fails the unchanged threshold even though
its longest interior pause is 340 ms. The failure and agreeing review remain
retained; the pause is not labeled cancellation.

The 70 retained policy exchanges include one context-cancelled request for the
advancing initial prompt fragment `In as much detail`. Its later directed
classification concerns that prompt, not an acknowledgement. The artifact
retains the exception, policy choices, comparison plan, commands, exact
runtime/profile identity, and metadata-authoring limitation. Participant PCM,
scenario content, cue rules, scorer, and provider settings are unchanged across
the two three-trial campaigns, but shared services and sequential execution
limit statistical claims. No threshold was relaxed and no failed trial was
filtered. The temporary server was stopped; no case-repeat, full-suite, or
final-candidate gate closes. The inter-phrase pause is the next behavioral gap.

## Clause segmentation repair and remaining within-phrase pause

The preceding third trial sent `First,` to synthesis as its own segment.
The scenario graph's `minimum_runes: 1` allowed that cut even though the
reusable splitter's default floor of twelve was designed to avoid short
introductions running out before the next synthesis arrived.
Revision `d5bce69f1e33aadf7ca478ae93694ace27b69aea` adds a separate optional
`minimum_clause_runes` floor and sets it to twelve for this graph. Sentence
ends retain the existing minimum of one, preserving short complete answers
and counting sentences; source completion still flushes immediately. Other
profiles that omit the setting retain their existing behavior.

`artifacts/scenario-speech-anchored-acknowledgement-20260905-03` retains three
new trials from executable
`sha256:1c6ed16ed2ba6099bd2d58a810fbfb33d7d924756e6e78cf459e39e9fcf5741d`.
All three completed and **2/3 passed**, the same overall result as the preceding
campaign. The isolated introduction is absent in every new recording. All
first holds contain the full 511 ms of cue-overlap activity. Trial 3 instead
fails the second hold's unchanged 500 ms gap limit with a **540 ms** pause.
Gemini 3.7 Flash found usable media and agreed with every outcome. All source
and review bundles independently reopened:

- Source receipt: `sha256:e79e9b2b216a9b3ec3415fcc5d3151ceb2c52220c7d11fc7dedbd80c34e53f2d`.
- Advisory receipt: `sha256:26ee146084a391ecc0f3b2babf713675312a9c74001ae000ddf85e96e492d976`.

| Trial | First cue | Second cue | First hold before/during/after | Second hold before/during/after | Result |
| --- | ---: | ---: | --- | --- | --- |
| 1 | 10,500 ms | 11,700 ms | 600 / 511 / 849 ms | 1,000 / 1,039 / 1,000 ms | Pass |
| 2 | 10,400 ms | 11,600 ms | 660 / 511 / 929 ms | 1,000 / 1,259 / 600 ms | Pass |
| 3 | 10,500 ms | 11,700 ms | 600 / 511 / 1,000 ms | 1,000 / 1,039 / 1,000 ms | Fail: second-hold gap |

All six input identities, cue opportunities, and waveform holds independently
reproduce; overlapping responses all report `completed`. Trial 3's pause spans
12,640–13,180 ms inside the first spoken segment. Audio chunks cover 533.96 ms
of that interval with a maximum delivery gap of 3.11 ms. The next text segment
does not begin until 16,160.313 ms. This is a low-activity interval within
near-continuously delivered audio, not a between-segment or 540 ms transport
stall; its provider-internal cause remains unestablished.

Initial prompt-end-to-audio latencies were 3,986 / 3,757 / 3,985 ms, versus
3,958 / 3,815 / 3,572 ms previously. These small sequential campaigns on shared
services do not establish a latency or overall success-rate improvement.
The emitted first phrase also retains the earlier `purchase Check` omission
where the supplied policy mentions the purchase email. Its origin is unknown;
keyword checks and advisory agreement do not prove complete content fidelity.

The artifact retains 79 policy exchanges, exact commands/configuration,
comparison plan, waveform and delivery audits, and the existing metadata
authoring limitation. The full repository gate passed; optional official SDK
tests explicitly skipped for a missing dependency. Splitter, mounted-element,
and real scenario endpoint regressions each reject an independent restoration
of the old behavior. No thresholds changed or failed trials were filtered.
The temporary server was stopped. Within-phrase audio continuity, content
fidelity, fifteen-repeat cases, the full suite, and final acceptance remain open.

## Provider-boundary attribution and purchase-confirmation coverage

`artifacts/scenario-provider-boundary-trace-20260905-01` retains one new
instrumented diagnostic from clean revision
`15e1c3ee8aeb3b9863db424e3c1b88b72c8f9c14`, using the same executable digest
`sha256:1c6ed16ed2ba6099bd2d58a810fbfb33d7d924756e6e78cf459e39e9fcf5741d`.
Local model/TTS proxies retained non-thought response text, projected requests,
original body digests, native provider audio, and receipt timing. Credentials,
headers, model reasoning, and signatures were excluded. The proxies change
transport and timing; this is attribution evidence for that one recording,
not an unchanged-deployment or performance comparison.

The trial completed and **1/1 passed** under v6. Gemini 3.7 Flash found usable
media and agreed. The first cue began at 10,000 ms, with 620/511/909 ms of
before/during/after activity; the second began at 11,500 ms, with
920/1,319/720 ms. Their longest hold gaps were 0 and 320 ms. Both overlapping
responses completed. Input identities, cue opportunities, and holds reproduce
from the WAV. The source and advisory bundles independently reopened:

- Source receipt: `sha256:dc3e6edbdd53c7a3740ac5f5bc05c4f00aa9c8c5c12e0b843085f90231c75d31`.
- Advisory receipt: `sha256:791a7a72ae84dc1bd13cccf1e3fd3d74495598efc40d09b959c070f0ba66e6cb`.

The initial partial-prompt model request disconnected after emitting text
ending at `purchase`. The succeeding complete response included `purchase
email`. That complete response equals the ten synthesis requests and delivered
text segments after joining segment boundaries with spaces. Offline replay of
all ten native Fish audio responses through the shipped adapter reproduces
every one of the **1,075,548 recorded PCM16 samples**, byte for byte. Five
within-request low-activity intervals last 300, 320, 420, 360, and 340 ms.
Those pauses are present in the provider audio. This trace contains no runtime
sample loss and does not reproduce the older omission or 540 ms failure.
The isolated endpoint and both proxies were stopped; shared services remained.

The omission still justifies an evaluator repair: the earlier three content
checks could all pass when `purchase email` was absent, because the response
mentioned an order number, return label, and original payment method. Scorer
version 7 now independently requires one of `purchase email`, `confirmation
email`, `email confirmation`, or `order confirmation` in both acknowledgement
cases. An unrelated return-label email cannot supply that detail. This remains
a bounded phrase requirement, not proof of complete semantics or audible-word
fidelity; negation and other policy omissions still need further evaluation.

`artifacts/scenario-purchase-confirmation-v7-20260905` reopens all ten source
recordings and first reproduces every original v6 pass/failure and acoustic
hold from the retained WAV, transcript, and digest-checked participant PCM.
It then evaluates the additional requirement. Original files, scores,
receipts, and model reviews remain unchanged:

| Campaign | Attempts | Original v6 passes | Retrospective v7 passes |
| --- | ---: | ---: | ---: |
| Speech-cue baseline | 3 | 0 | 0 |
| Validator repair | 3 | 2 | 0 |
| Clause segmentation | 3 | 2 | 1 |
| Provider-boundary trace | 1 | 1 | 1 |

Seven recordings omit the purchase-confirmation phrase; three of them had
passed v6. All earlier cancellation and acoustic failures remain. The
regression rejects the retained omission despite continuous audio, completed
response evidence, and an unrelated email mention, while accepting all four
declared alternatives. Removing the added check makes it fail. The full
repository gate and explicit optional SDK skips are retained with the audit.
No new v7 live campaign or advisory rereview is claimed. Within-phrase audio
continuity, the earlier omission's cause, broader content fidelity, complete
repeated scenarios, and final acceptance remain open.

## FD-Bench paired diagnostic

The post-repair study is retained under
`.runtime/fdbench-subturn-study-v7`. Its binary is built from pushed commit
`01c82bdd8363` and has digest
`sha256:47cb7c71be3d81d721009c20c0bae960fcc178b0f2c5298b80e337c0ef60b55f`.
Both arms use the same five lexicographically selected conversations
(`conversation_1`, `conversation_10`, `conversation_100`, `conversation_101`,
and `conversation_102`) in each of four conditions. The arms ran sequentially
against the same local and remote services to avoid shared-load confounding.

The graph-native launch profiles are:

| Arm | F8 | Partial acts | Profile fingerprint |
| --- | --- | --- | --- |
| Baseline | `turn-boundary` | `listen`, `keep-speaking` | `sha256:145c66aa9e40b605529570a9a5ca8ae6dbfb91e907487bba36161c8048d368c3` |
| Treatment | `subturn-qwen` | `listen`, `speak-through`, `interrupt`, `act-silently`, `keep-speaking`, `stop-speaking` | `sha256:d3112d62c0edf73a3e6cb2aac0adcd7fb64000ad30ac46ff800771e6f9e3f856` |

The profile diff contains only the name, partial-act list, and fingerprints or
digests derived from those values. In both arms, every Deepgram partial is
presented to the local Qwen interaction model; the baseline constrains the
decision to passive acts, while the treatment permits all six sub-turn acts.
Gemini 3.5 Flash with budget 512 remains the response-content model in both
arms.

The study retained **40 canonical source WAVs** (20 per arm), all PCM16,
24 kHz, and stereo. It also retained a content-addressed reviewer copy of each
recording. Gemini 3.7 Flash evaluated all **40/40** recordings; all were usable,
all advisory outcomes were `fail`, and all agreed with the deterministic
scorer. Every cell completed five attempts with no infrastructure failure, but
every cell is **non-reportable** because 288 of its 293 conversations were not
attempted.

### Deterministic results

Values are baseline → treatment means over the same five conversation IDs.
“Latency” is response-onset latency only for turns that produced a measurable
answer, so its sample changes when one arm answers fewer turns and should not
be compared alone.

| Condition | Clean | Answered | Missed turns | Overrun turns | Premature turns | Overlap (ms) | Latency (ms) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `f5tts-single-round-combine-easy` | 0/5 → 0/5 | 0.4 → 0.8 | 3.8 → 3.4 | 2.6 → 2.4 | 0.0 → 0.0 | 1,830 → 1,839 | 1,945 → 1,960 |
| `f5tts-single-round-combine-hard` | 0/5 → 0/5 | 0.8 → 0.6 | 3.4 → 3.6 | 3.0 → 3.2 | 0.0 → 0.0 | 3,353 → 2,916 | 1,865 → 1,837 |
| `f5tts-single-round-combine-easy-noisy-bg-0dB` | 0/5 → 0/5 | 0.0 → 0.0 | 4.2 → 4.2 | 0.0 → 0.0 | 0.0 → 0.0 | 0 → 0 | — → — |
| `cosyvoice2-single-round-combine-easy-noisy-gap-0dB` | 0/5 → 0/5 | 0.6 → 0.2 | 3.6 → 4.0 | 0.2 → 0.0 | 0.2 → 0.2 | 241 → 37 | 703 → 1,942 |

This slice does not support a claim that the expanded partial policy improves
FD-Bench overall. It cut hard-condition mean overlap by 437 ms and reduced
noisy-gap overlap by 204 ms, but clean-easy overlap was unchanged and the
noisy-gap arm answered fewer turns. No condition produced a clean conversation.
The lower noisy-gap overlap is therefore partly silence from missed responses,
not an unqualified interaction win.

### Failure attribution and repair evidence

The retained timelines and Gemini reviews separate four mechanisms:

- Clean F5-TTS failures are chiefly late playback cancellation and response/TTS
  duration. Both arms start measurable answers near the two-second budget and
  then continue into later user speech. Permitting `stop-speaking` on partials
  can reduce some overlap, but does not make cancellation immediate or shorten
  generated speech.
- At 0 dB continuous background noise, both arms have identical zero-answer
  and 4.2-missed-turn means. Deepgram/endpointing commonly joins several user
  turns and intervening noise into one late transcript, so the interaction
  model does not receive clean per-turn evidence to repair.
- In noisy-gap audio, the treatment reduces acoustic overlap but more often
  remains silent or batches multiple questions into one late answer. This is a
  mixed endpointing/ASR and activation-latency result, not evidence that the
  Qwen act choice alone solved the condition.
- Gemini also hears response-generation artifacts in selected recordings:
  clause restarts and repetitions, prompt-like phrases, and, in two noisy
  recordings, scratchpad or `<thought>`-like prose. These are cognition/output
  hygiene failures distinct from partial interaction classification and remain
  visible in the failed advisory records.

The first post-twelve-case FD-Bench attempt, retained separately under
`.runtime/fdbench-subturn-study-v6`, proved that the earlier legacy-runner
sanitizer did not cover strict graph-native streaming: Gemini heard the
reserved `[runtime: ... Prepared but never spoken ...]` projection in
`f5tts-single-round-combine-easy/conversation_10`. Commit `01c82bdd8363`
added the missing graph-native boundary. In v7, a scan of all 40 canonical
`agent_text` timelines finds no reserved runtime marker, and none of the 40
Gemini reviews reports that projection leak. The v6 failure remains immutable
diagnostic evidence; no successful retry overwrote it.

### FD-Bench receipt index

The values below are the receipts' own `receipt_sha256` identities, not hashes
of the receipt JSON container files.

| Condition | Baseline source | Baseline Gemini aggregate | Treatment source | Treatment Gemini aggregate |
| --- | --- | --- | --- | --- |
| F5-TTS easy | `sha256:c88cfa9896ba31b061bc6f39b0fdd352c306c9ced789987244562c26a9ee7e67` | `sha256:147ea8748577c4e7f809af2ed9421e5483fdb1748eed67d1f160bd6a782c7579` | `sha256:5e90ad53a6bc8b0b7b0fc974a09d5e5d97c2c72249ec84dc6a71d37d48bbcbd5` | `sha256:4922f170b2b7aff287030b51e14cca6c1589a492db8f7fc748d745c1ed7d5572` |
| F5-TTS hard | `sha256:43e6f150d31e1c62a50fbc62d051f83bb51940c5a96c90de74bf93e0f1ab179d` | `sha256:ca6519717127db1293ac047e2c0bd4b00785882f82d51cd0e43ed3d30bf6bedc` | `sha256:7c3768df5bbe2d0993e85a9a61ae9261ceb59d845734f805a7221288a8c93c6d` | `sha256:dbe5bd1cce9870e285da9f7d4ad0b4d936c24abcfdf61abe75e8886faaf83b66` |
| F5-TTS easy, noisy background 0 dB | `sha256:f3247d97b514cdbb0c7aef96a08ecefaaf0b51d5b7eb6454acaa053002e7cc8b` | `sha256:614c7c63dc7982b3249a2cd838ae116130f7cd3fe517d11c41e51053ad04cf71` | `sha256:7b110ac8ec042cc3f4f8793b060094f0313153858604d4a26b936ebb0dd6ee87` | `sha256:eb0dfe30625aa9a7124c2ec82e904b31297f5cf6fc23106fa6333faaa56c46ad` |
| CosyVoice2 easy, noisy gap 0 dB | `sha256:7b23c47b65c40d93359bf18275e56f604660f7ade1f02b0ce9432b4b28ab513c` | `sha256:9a02d240186692c5bfed79d0be40495ff4aa006ea70cd875fe48ea88dcac0def` | `sha256:17d4e396651d63eeb145ff6322cb619aab3c50805e94a6e8a9fc0aab7f7c8a62` | `sha256:875e0d7a04467e9eaeec4596d80cde1771e8abf6e82794468818b4adeff3b45f` |

## Which benchmark suites benefit

Sub-turn classification is an interaction/timing intervention, not a universal
model upgrade. The recommended scope is:

| Suite | Applicability | Why / boundary |
| --- | --- | --- |
| FDB v1.5 | **Strongest fit** | Every recording injects an overlap event while the agent is speaking. Partial acts directly test yield versus hold; run the full 498-task population before making a release claim. |
| Meeting Assistant | **Strong** | Concurrent listening, speaking, corrections, and background speech have the same live floor problem. Apply the partial policy to the speech lane while retaining the separate visual/background authority. |
| FD-Bench | **Secondary but useful** | Its primary questions are endpointing, premature speech, missed turns, and response latency. Partial acts can reduce premature/late responses, but must be reported as a timing treatment rather than folded into the suite’s task definition. |
| FDB v3 | **Selected cases** | Disfluent requests and spelled identifiers can benefit from waiting for a semantically complete sub-turn before a tool proposal. The classifier must gate admission; it must not grant tool authority or rewrite the released arguments. |
| Realtime-CU | **Targeted** | Audio-triggered actions and barge-ins can use early semantic acts, but visual grounding, authorization, and settlement remain independent. Only selected audio-overlap tasks should vary this factor. |
| DynaCU-Bench | **Low broad applicability** | The suite is primarily a screenshot/action loop. Introducing a word-level floor policy changes action timing without testing the suite’s dynamic-page question, so leave it out of the general condition. |
| τ-Voice | **Leave unchanged** | tau2-bench owns the task simulator, databases, speech conditions, and reward. Its control/regular comparison and interaction metrics are the published contract; changing the profile would create a new, non-comparable system treatment. Keep the existing τ-Voice runner and add a separately identified experiment only if a benchmark owner requests one. |

The practical next step is a preregistered full FDB pair, followed by a larger
FD-Bench population only after the ASR/endpointing and response-duration
failures above are repaired. The focused v4 and v7 results are useful for
diagnosing policy and audio behavior, but they are not evidence that a sub-turn
policy improves every suite or that τ-Voice should be changed.

## Reopening the retained evidence

The scenario chain was reopened without a provider credential:

```sh
.runtime/deepgram-scenario-media-v28/bin/openrealtime review verify-scenario \
  -source-dir .runtime/deepgram-scenario-media-v28/diagnostic-12x1-20260904-01 \
  -source-receipt .runtime/deepgram-scenario-media-v28/diagnostic-12x1-20260904-01.receipt.json \
  -evaluation-dir .runtime/deepgram-scenario-media-v28/advisory-gemini37-12x1-20260904-01 \
  -evaluation-receipt .runtime/deepgram-scenario-media-v28/advisory-gemini37-12x1-20260904-01.receipt.json
```

Each FDB review chain was likewise reopened with:

```sh
.runtime/fdb-subturn-study-v4/bin/openrealtime bench verify-candidate-review \
  -review-prefix .runtime/fdb-subturn-study-v4/<arm>/evidence/20260904-02-<category>
```

`<arm>` is `subturn-qwen` or `turn-boundary`; `<category>` uses the dataset
identifiers `user-interruption`, `user-backchannel`, `background-speech`, or
`talking-to-other` in the artifact prefix (the source category itself remains
underscore-separated). All eight commands returned 5/5 verified.

The eight FD-Bench v7 chains were reopened with the same candidate verifier:

```sh
for arm in turn-boundary subturn-qwen; do
  for cell in \
    f5tts-easy \
    f5tts-hard \
    f5tts-easy-noisy-bg-0db \
    cosyvoice2-easy-noisy-gap-0db
  do
    .runtime/fdbench-subturn-study-v7/bin/openrealtime \
      bench verify-candidate-review \
      -review-prefix ".runtime/fdbench-subturn-study-v7/$arm/evidence/20260905-01-$cell"
  done
done
```

All eight commands returned 5/5 verified, for 40/40 independently reopened
recordings and reviews.


## Scenario score replay from retained evidence

New scenario results retain `replay.version: 1` alongside scorer version 7.
The verifier reconstructs authored input timing, exact source PCM, serialized
agent chunks, menu transitions, requested recognizer windows, acoustic checks,
content checks, and latencies. It then compares the complete result and its
architecture-task projection. Source-integrity verification remains available
for historical results; behavioral campaign acceptance now requires replay.
The public offline command is `openrealtime review replay-scenario`.

Two focused campaigns ran from clean source
`b7d188daabc4ea13d24d7862cfcfe22a2cd1d6e5`, before integration with the colleague's
later commits. Both used the identical executable SHA-256
`cb011753c5792c8da0b709c9da0fb3202010af4b176975b2ba13445c87447f6e`.
Deepgram Nova-3 handled conversational recognition, Gemini 3.5 Flash generated
responses, Qwen supplied the local policy, and Fish supplied speech. The
counting scorer used the separate SenseVoice endpoint. Exact profiles, graph,
values, execution receipts, model settings, commands, and media are retained.
These are focused diagnostics; they add no final-candidate population credit.

| Recording | Current deterministic outcome | Offline replay | Advisory review |
| --- | --- | --- | --- |
| Observed-speech acknowledgement, trial 1 | Pass | Exact match | Agrees |
| Observed-speech acknowledgement, trial 2 | Fail: omitted purchase confirmation | Exact match | Agrees |
| Observed-speech acknowledgement, trial 3 | Pass | Exact match | Agrees |
| Recorded menu | Pass | Exact match | Agrees |
| Interrupted count | Pass under current boundary check | Exact match | Agrees |

The second acknowledgement again said “order number and purchase Check that”
without completing the required confirmation phrase. Its original failure,
media, acoustic measurements, and agreeing review are retained. Replay proves
that this failure follows from the retained inputs; it does not explain the
provider boundary that caused the omission.

The interrupted-count pass has a narrower meaning than sustained counting:
SenseVoice heard `1.` in `[0, 15000)` ms and `2.` in `[27925, 39925)` ms. Their
PCM digests are respectively
`sha256:38143ca8afcc65f84a1f842a1cab468e9150548d22cd3f51204cb924fdcae378`
and
`sha256:2ee56e7fd5ac4e0e06aabbaa8197d6c3fb87ee891b816cb09f6d3862f9292e9e`.
The reviewer heard the same progression. The version-7 check required no minimum
number of audible counts on either side, so this evidence does not establish
continuous counting toward forty or a sufficiently long pre-interruption
prefix to distinguish restarting. That scenario's full acceptance box remains
open. The subsequent sustained-counting diagnostic below implements this
coverage extension and retains the remaining behavioral and evaluator failures.

Retained directories:

- `artifacts/scenario-score-replay-20260905-01`: acknowledgement source receipt
  `sha256:d5cfe358a8889a1444cb0ccd38d7bfc597750b88660659dfd4d8f549868b6017`,
  advisory receipt
  `sha256:7e6217ad06fc98b2d09e9c9889670f67dccb0f98011683a33e0c7af9bf8b8cf5`.
- `artifacts/scenario-score-replay-menu-counting-20260905-01`: menu/count source
  receipt
  `sha256:e696fa0d448b68a6a9d6e890f3c64482266dc2474481474318f866098372b433`,
  advisory receipt
  `sha256:7da8e5d35dbcc7d2ba641434691c9bdba653c9409fb5ea9af6fded18fd196c71`.

Both source trees and every advisory evaluation were independently reopened;
all five outcomes and metrics replayed exactly without provider calls.
Regression coverage includes all twelve canonical scenarios, production
recording, sent speech cues, absent historical inputs, tampered outcomes,
acoustic measurements, latencies, room/output audio, counting windows, menu
results, and freshly resealed architecture metrics. Mutations bypassing result
comparison or metric comparison were caught and restored.

The initial full gate also exposed an older endpoint test that stopped at the
first response's completion and assumed every segment shared that response ID.
A ten-repetition race run reproduced the scheduling-dependent false failure.
The repaired test waits for all expected audio and the final response's
completion, checks every segment and successful terminal status, and verifies
balanced response lifecycles. Ten race-enabled repetitions passed; restoring
`minimum_clause_runes: 1` still failed the segmentation assertion. Original
failure logs and mutation evidence are retained beside the integration gate.

Replay treats recognizer text and transcript events as retained observations.
It does not independently establish ASR accuracy, provider/runtime authorship,
or a canonical run specification. Missing or partly transmitted dynamic cues
still lack their complete source PCM and refuse this verifier. Universal
all-suite replay, trusted thresholds, the independently controlled release
store, and the final 7,501-attempt campaign remain open.

## Sustained counting and the audible stopping boundary

The earlier one-number interrupted-count pass exposed both a scenario coverage
gap and a production instruction problem. A provider-boundary recording from
clean `0f210b297c0383dbba21dafb5c65812d2e4bbbc8` retained the complete initial
request producing only `One.` and the resume request producing only `Two.`.
Both model streams reached upstream EOF with `STOP`; synthesis received exactly
those texts and completed. This locates the immediate omission in model output
for that recording, before synthesis or playback. It does not explain unrelated
purchase-confirmation or event-count omissions.

The new instruction explicitly requests the complete remaining finite range in
one response and delegates pacing/interruption to speech playback. It separately
requires exactly `<wait>` when an event-driven count has no new occurrence.
Fifteen controlled model requests (three repetitions of initial range, resumed
range, no-new-animal, first-animal, and one-number-only controls) passed 6/15 with
the old instruction and 15/15 with the candidate. The exploratory first revision
and its inadequate silence criterion remain retained separately; they are not
included in that comparison. Controlled text output is evidence about this
instruction, not proof of live conversational behavior.

Scorer v8 added an authored minimum of three observed numbers on both sides,
range/order/start checks, rejection of already-completed counts, and captured
speech in the two seconds before interruption. It rejected both retained v7
one-number passes when separately rescored. Fresh v8 recordings from clean
`ce0ad2bab2b2c9a426881e71c84b9b3f2c3f6843` then exposed two evaluator problems:
SenseVoice merged adjacent spoken numbers into `145` or `1314`, and the old
recognition window ended when the user began interrupting, excluding words the
agent audibly finished while stopping. Neither problem justifies rewriting
recognizer output from the runtime's proposed text.

Scorer v9 extends the recognized prefix through the authored stopping deadline
and independently requires captured silence from that deadline until the resume
request. In these recordings the prefix is `[0, 19496)` ms, the quiet interval
is `[19496, 25000)` ms, and the resumed observation is `[27925, 39925)` ms. The
same 20 ms / RMS 128 waveform activity rule and 120 ms tolerance used by the
other acoustic checks apply. Results and reviews retain counts, window bounds,
recent activity, and quiet activity. Regression mutations removing the count
minimum, interruption opportunity, stopping-prefix extension, or paused-speech
check each fail their targeted test.

| Evidence population | Original deterministic outcome | Interpretation |
| --- | --- | --- |
| v7 provider trace, interrupted count, one trial | 1/1 | One number on each side; false positive for sustained counting, now rejected |
| v8 event count, three trials | 2/3 | Trial 3 omitted the first animal count despite recorded admission decisions |
| v8 interrupted count, three trials | 1/3 | Two failures contain merged recognizer digits; original scores and observations remain intact |
| Separate v9/Whisper rescoring of those three v8 recordings | 3/3 | New attributed recognition/windows on existing media; zero new live trials |
| v9 interrupted count, three fresh trials | 1/3 | A real failure to stay stopped and a disputed recognizer sequence remain retained |

The fresh v9 recordings used clean
`c18d781de4f56872ef280b239f34cf2fb72dbb21`, including the colleague's intervening
commits through `8276a03`. Trial 1 has 3,164 ms of waveform activity in the
required quiet interval; the existing transported-audio check reports 4,180 ms
in its nearly identical window. Speech continues after recorded cancellations.
This is an observed cancellation escape; the responsible lifecycle boundary
has not yet been established. Trial 2 passes with prefix 1–4, continuation 5–10,
and no quiet-window activity. Trial 3 also has no quiet-window activity but its
recognizer reports `6,7,8,9,10,11,12,13,11,12,13`; the advisory reviewer disputes
that sequence against the audio. The disputed original remains a failed result.

Every new source and advisory receipt reopens, and all ten original live
outcomes and metrics replay exactly using their corresponding frozen executable.
The v9 retrospective results also retain their new recognition observations and
replay exactly. This proves reconstruction from observations, not recognition
accuracy. Gemini 3.7 Flash reports agreement with every original result, but
that binary field overstates agreement on cause: v8 reviewers attribute failures
to not reaching forty, and the v9 trial-3 reviewer disputes the scorer's sequence
while again judging failure to reach forty. The authored v9 note explicitly
limits this recording to stopping and resumed progression. Eventual completion
needs its own sufficient horizon and check; advisory reasoning outside the
declared contract cannot validate a deterministic failure.

The v9 trial-3 hearing audit reconstructs the exact original window and checks
its PCM digest before making fresh recognizer calls. Whisper reproduces its
repeated suffix; SenseVoice instead merges the last numbers into `123`.
Extending the window by two seconds produces different erroneous suffixes from
both recognizers. Neither service is therefore a demonstrated oracle for this
recording, and changing services or window lengths alone does not resolve the
coverage gap. These audit observations do not replace the sealed judgment.

The three diagnostic directories retain exact source/executable/profile/graph
identities, executed commands, participant PCM identities, media, observations,
reviews, and support scripts:

| Directory under `artifacts/` | Source receipt | Advisory receipt |
| --- | --- | --- |
| `scenario-counting-boundary-trace-20260905-01` | `sha256:3f6709c74f5a3226ef6bbe89c866a18d2f93a9af2394fc60dc27d76791f729c1` | `sha256:a2b4afc88a157e50a8abf24d2a98e081b6105ac8959bf6b0b99174084b44b50f` |
| `scenario-counting-v8-repair-20260905-01` | `sha256:0c8a627214c94466482cb131c951b7309432b55ccc7be4e86d861e0a9b11b990` | `sha256:e12e921337b3a682c5690e8c34411fd788dc71a2e9d36db2b18c4adfca7a3dcc` |
| `scenario-counting-v9-stop-window-20260905-01` | `sha256:b49f6a7644410e29c1c90a31e97c496b5c1b339d12c5c60426138a926f4220bd` | `sha256:dee30ed110fdd97137e649839e31422148133cf59f9697951ac4d7de8ecb6e2a` |

The v8 and final combined-code v9 developer gates passed. Both explicitly
skipped the official SDK WebSocket/WebRTC tests because that SDK was not
installed; those integrations are not verified by these runs. Logs and exact
validation identities are retained in their corresponding directories.
No shared services were stopped. These focused recordings
close neither the per-case fifteen-repeat requirements nor the twelve-case
180-attempt campaign, and add no credit to the final 7,501-attempt ledger.

## Queued speech after completed preparation

The preceding v9 recording exposed speech continuing after cancellation. A
mounted regression now reproduces a concrete gap in the overlap controller:
once segmentation was terminal, cancellation no longer addressed it, although
its prepared sentences could still be waiting for synthesis or playback. The
controller also retired a completed model/segmentation run whenever no utterance
was currently active, including between segments or before their independent
status lanes arrived. This lost the run address needed to revoke queued speech.

The repair retains the segmentation outcome's emitted count until exact distinct
speech terminals arrive. Prepared speech remains visible as queued work, and
both a semantic stop and an acoustic interruption address the entire segmented
stream after preparation completes. Cancellation before synthesis closes an
utterance without requiring a nonexistent playback receipt. Per-run terminal
membership also prevents a delayed speech/status lane from reviving a finished
segment after the bounded global terminal cache evicts its entry.

The regression fails on the old code for speech-before-completion,
completion-before-speech, and release-before-completion. The production-graph
test prepares eight numbers, emits the first number's audio, processes a spoken
stop, and checks cancellation of all queued sentences, no subsequent canceled
audio at the sink, and successful speech for a new request. Ten race-enabled
repetitions pass. Restoring active-only segmentation cancellation fails that
mounted test; dropping the emitted population and ignoring per-run terminal
membership each fail their corresponding ordering regression. These prove the
repaired mechanism, rather than establishing every historical failure's cause.

Six live recordings used clean `bc909ad` with overlap implementation 10 and
unchanged scenario scorer 9. The executable SHA-256 is recorded in
`artifacts/scenario-queued-speech-cancel-20260905-01/identity.json`.

| Case | Original deterministic result | Retained observations |
| --- | --- | --- |
| Event count, trial 1 | Fail | First animal count omitted; only “2” emitted for the second animal |
| Event count, trial 2 | Fail | First animal count omitted again |
| Event count, trial 3 | Pass | Both requested counts emitted |
| Interrupted count, trial 1 | Pass | Prefix 1–6, resumed 7–14, zero quiet-window activity |
| Interrupted count, trial 2 | Fail | Whisper reports prefix 1–10 and resumption at 7; zero quiet-window activity |
| Interrupted count, trial 3 | Pass | Prefix 1–7, resumption at 7 through 15, zero quiet-window activity |

All three interrupted-count recordings have zero measured active audio in
`[19496, 25000)` ms, compared with the earlier trial's 3,164 ms. This is a
three-trial diagnostic improvement, not full case or suite acceptance. Event
counting remains unreliable at 1/3 in this sample; it cannot be described as
repaired. Every original outcome and metric replays, and all six source/review
records reopen. The isolated server stopped after recording.

The second count recording's exact-window recognizer audit reproduces the
discrepancy: SenseVoice hears 1–6, while Whisper hears 1–10; both recognize the
resumed 7–15. Only two approximately 93 ms audio chunks from the interrupted
“Seven” response occur after “Six”; the next queued text is canceled. The
advisory reviewer also describes counting to six and resuming at seven, yet
reports binary agreement with the failure because the agent did not reach forty.
That rationale again exceeds the bounded stopping/resumption contract. The
original failure and all contradictory observations remain retained; no result
is replaced by runtime text or a preferred recognizer.

The first repair's full developer gate passed. After rebasing onto the
colleague's evidence-formatting gate change, the separate cache-eviction repair
advanced overlap to implementation 11. Its exact validation identity, focused
race checks, graph lock, mutation, and final full-gate log are retained alongside
the earlier campaign; it does not retroactively change that campaign's binary.

The six-trial source receipt is
`sha256:aec156fbbcb4863a3f591e8ccd63eb5d3e45f74e649dac0f1c12788cbf21992f`;
its advisory receipt is
`sha256:7ad9fe83ca70f48f8587333cf88fd2e56828a23ecb8fa2db653f1501ccacd202`.

## Event-count omission before the provider boundary

A separate three-trial event-count campaign from clean
`92ec459c9c9c5ab61d429f71226767d4b2c3fc90` uses overlap implementation 11 and
transparent local model/TTS tracing proxies. It again passes only 1/3. The two
failures omit the first animal count; the third emits both counts. All three
recordings, exact provider projections, advisory evaluations, receipts, and
deterministic replay are retained in
`artifacts/scenario-event-count-boundary-trace-20260905-01`.

| Trial | Model HTTP requests observed | Synthesis requests observed | Original outcome |
| --- | --- | --- | --- |
| 1 | One, returning `2` with upstream EOF / STOP | `2`, completed upstream | Fail: first count omitted |
| 2 | One, returning `2` with upstream EOF / STOP | `2`, completed upstream | Fail: first count omitted |
| 3 | Two, returning `1` then `2`, both upstream EOF / STOP | `1` then `2`, completed upstream | Pass |

The missing first counts therefore disappear before the traced model HTTP
boundary. They are not cases where a returned `1` failed to reach synthesis.
This does not yet distinguish non-admission, stale invocation rejection, or
cancellation before the HTTP request. The policy projection retains
`speak-through`, trigger `yes`, final `condition-met`, and final `answer` for
the first animal in both failures. Such model choices are not themselves
proof that the downstream invocation was accepted.

Both failing trials receive a complete partial transcript for the capybara
sentence before the identical final; the passing trial proceeds from a shorter
partial to the final. This is a concrete scheduling correlation for the next
mounted reproduction, not a demonstrated root cause. Preserve the failed
sources while tracing semantic admission through committed invocation and the
model's admission/cancellation boundary.

The exact executable SHA-256 is
`fd32dd973753f5a5bebb965c41daa5940fb59347a49b106e703591d29c051e08`.
Source receipt:
`sha256:644533336815a13ad9c6a4561a5f5556e2de14e1db8aa94bf28a6b715d8ebb27`.
Advisory receipt:
`sha256:882e3feb32a4368567cb7d5f61dcb9a7727c484a38767fb17667bbbb92a8ba4d`.
The server and both run-owned proxies stopped after recording; no shared
service was stopped. Traces retain safe text/audio projections and hashes,
without authentication headers, hidden reasoning, or thought signatures.
This campaign supplies no full-suite or final-candidate credit.

## First-count confidence ordering

The retained first-count failures have an additional decisive observation:
the policy client measures the chosen first token's probability. Both failed
finals selected `answer` at approximately 0.6511, below the configured 0.7
threshold, while the independent activation guard selected `condition-met` at
approximately 0.9989. Their complete partials selected `speak-through` at
0.6789. The passing trial's final selected `answer` at 0.9889. These values
are reconstructible from the retained, projected token log probabilities;
the complete-partial scheduling correlation alone was not the diagnosis.

A deterministic production-graph regression reproduces the failure with the
same complete-partial-to-identical-final ordering. It first installs the
standing instruction without speech, suppresses the uncertain partial, and
observes the final become `listen` at `confidence_guard`, with zero provider
calls despite activation confidence 0.999. Changing only the primary choice
to `listen` or a confident `answer` produces the requested count. This exposes
an ordering inconsistency: the existing standing-trigger recovery handled
explicit `listen`, but not the uncertain `answer` that the later confidence
guard would turn into `listen`.

SemanticAdmission implementation 12 lets the same independently verified,
pre-existing standing condition recover an uncertain final answer before that
guard. The recovered decision names `voice_activation` and its actual
activation confidence. The unchanged threshold still suppresses uncertain
partials. Mounted controls also reject unmet and uncertain conditions; passing
cases deliver one count through the real graph's model, TTS, and audio sink.
The original regression fails before the repair and all five cases pass after
it. Logs are retained under
`artifacts/scenario-count-admission-repair-20260905-01`. Restoring the old
admission code through a test overlay fails at the same confidence guard.
Focused race tests and the complete developer gate pass. The official SDK
WebSocket/WebRTC checks explicitly skip because the SDK is not installed.

Six live recordings from clean `0424eaf6064fda4d7e93e62260024b8003e1126f`
pass: three event counts and three ordinary-question controls. Their executable
SHA-256 is `e2a1940959a4089b456626dd1903387a6b6d7119737782ea5cd658bcfd42c68a`.
All six exact-model advisory reviews agree, all sources/evaluations verify,
and all outcomes and metrics replay. Nine model requests and nine synthesis
requests complete upstream. Each count session returns `1` followed by `2`;
each control answers the capital-of-France question. Count trial 3 reproduces
the old 0.6511 final `answer` and 0.9989 `condition-met`, but now makes the
first-count model request and receives `1`. The other two trials act on more
confident partials, so they do not independently exercise the repaired branch.

The trial-3 reviewer retains an unusual pronunciation of `1` as a minor
observation, despite accepting the count. This acoustic observation is not
erased by the deterministic pass. Source receipt:
`sha256:8881937b2b9c5dd35badf3590463d7ae43001a1f50deefd19632d14240fb9159`.
Advisory receipt:
`sha256:8bac374919efc0d85b4ac01904806dd961c8adc63aee42f12e13bf5d1c21ad0c`.
The bundle includes exact tracked source, binary, configuration, provider
projections, commands, regression/mutation logs, and the developer gate.
The isolated server and tracing proxies stopped without touching shared
services. The complete fifteen-trial case and final-candidate acceptance remain
open; the earlier failed recordings and scores remain unchanged.

## Fifteen event counts and audible-content disagreement

A separate fifteen-trial event-count diagnostic used the same clean `0424eaf`
source and byte-identical executable as the six-trial repair/control campaign.
All fifteen deterministic scorer-9 outcomes pass. All fifteen source/review
records verify, all outcomes and metrics replay, and every review has usable
media. The campaign is retained under
`artifacts/scenario-count-admission-15x-20260905-01` with its own exact source,
binary, profile, provider traces, commands, and receipts. It is a diagnostic
population; no attempt enters the shared final-candidate ledger.

Every session makes two model requests returning `1` and `2`, followed by the
matching two synthesis requests. All thirty model and thirty synthesis streams
finish upstream. Trials 2, 5, 7, 9, 11, 14, and 15 reproduce the old final
`answer` confidence of 0.6511 with strongly verified `condition-met`; each now
invokes the model for the first count. This strengthens the admission-repair
evidence without establishing that every emitted digit was spoken correctly.

The advisory reviews agree with 14/15 pass labels. Trial 12 is judged a failure
because its first count contains unwanted speech resembling “This is Pierce…”
instead of only the number. Trial 5 retains a significant garbled second-count
finding despite its reviewer marking agreement with the deterministic pass.
Other reviews retain distorted first counts as minor observations. Their
labels and explanations remain unchanged; binary agreement cannot erase an
audible-content finding or resolve inconsistent severity judgments.

The exact-window audit verifies each source WAV against its media manifest,
extracts only the agent channel around the response's recorded playout, and
makes fresh, separately retained recognition requests. Both recognizers hear
non-count speech on the affected windows. The same audit against the exact
upstream 44.1 kHz synthesis WAVs locates that speech before playback:

| Synthesis exchange | Input text | SenseVoice on upstream WAV | Whisper on upstream WAV |
| --- | --- | --- | --- |
| Trial 3, first count | `1` | “It's serve.” | “If swerve.” |
| Trial 5, second count | `2` | “Ever as mom.” | “Ever. And mom.” |
| Trial 12, first count | `1` | “This is Pierce, Da K.” | “This is Pierce. Daekwon?” |

These observations support a synthesis defect for those recordings. The
reviewer's proposed reference-prompt-leak explanation is not established by
the trace alone. The retained request names only the digit, `reference_id:
default`, no inline references, normalization enabled, and temperature 0.8.
The services also disagree on short control windows: the supposedly clear
trial-5 first count becomes “Yeah.” or empty, and trial-1's second count becomes
“22.” or “to one two”. Neither recognizer is a demonstrated oracle for all
single-digit speech, and choosing a preferred recognition does not resolve the
evaluation contract.

Scorer 9's event-count `CheckSaid` checks reported agent text and associated
timing; its replay inputs have no independent event-count hearings. Consequently
correct `1`/`2` text can pass alongside garbled or extraneous synthesized speech.
The next repair must make audible count content and “say nothing else”
independently observable, preserve ambiguous recognition, diagnose the upstream
short-number synthesis, and rerun the repeated case under the strengthened
scorer. The fifteen-repeat recording milestone therefore does not close the
event-count acceptance box.

Source receipt:
`sha256:984ca3b5325f6a0982779d2b32da54eb35c0e8825ca875d558d6c456b69f4983`.
Advisory receipt:
`sha256:c4a4b2e95fb385a174df940b5d055c882411b88d36b0638416c56c3f4015ab5c`.
`count-summary.json` joins each trial with provider observations, confidences,
reviews, and exact result digests. The hearing audit's initial response-variable
serialization error and corrected retry are retained separately; existing
observations were reused without replacement. The server and proxies on
18977–18979 stopped, and private intermediate policy capture was removed after
safe projection. Shared services and colleague processes were preserved.
