# Sub-turn interaction benchmark study

Status: 2026-09-04. This note records the retained twelve-case checkpoint and
the paired Full-Duplex-Bench (FDB) diagnostic that followed it. The FDB run is
an intentionally focused diagnostic (five recordings per category), not a
release result: the complete FDB v1.5 population is 498 recordings and every
limited cell below is marked non-reportable by the runner.

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

The code and shipped Deepgram profile used for the run are at commit
`8e7065b99623` (`interaction: classify active transcript overlap`). The profile
keeps the narrow local Qwen classifier on ordinary active-output transcript
events: directed speech yields `stop-speaking`, while a listener backchannel or
side speech yields `keep-speaking`. A protected same-stream continuation still
uses the general event policy so correction/resume behavior is not lost.

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

The practical next step is a preregistered full FDB pair using the already frozen
profiles, followed by selected Meeting/FD-Bench cases. The focused v4 result is
useful for diagnosing policy and audio behavior, but it is not evidence that a
sub-turn policy improves every suite or that τ-Voice should be changed.

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
