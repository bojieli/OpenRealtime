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
