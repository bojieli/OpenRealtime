# ADR-0016: Preserve speech history when model freshness expires

- Status: Accepted for the opt-in scenario conversation profile; live and release
  acceptance require separate retained evidence
- Date: 2026-09-05
- Complements: ADR-0003, ADR-0004, ADR-0015

## Context

The composed scenario graph can stream speech while further user observations
commit. `interaction.ModelResultCommit` compares the model's original trajectory
version with the current version at completion. A matching final ASR transcript
can advance version 3 to 4 after a count starts from its partial. The complete
result is then rejected, even though `One.` reaches the audio sink. The playback
adapter cannot mark absent assistant items played, so later model requests omit
that count and a release receipt can remain unresolved.

The mounted reproduction retains the exact `version_conflict`, actual sink
speech, and missing canonical assistant item. Independent live evidence also
shows missing response terminals, absent assistant count history, and repeated
counts, although the mounted reproduction alone does not establish the cause
of every live failure.

Freshness rejection remains necessary for proposed actions. Erasing an already
observed speech effect does not undo that effect and makes future context less
accurate. Recording prepared speech must also remain distinct from claiming it
was heard: cancellation can leave some or all prepared words unplayed.

## Decision

The scenario profile explicitly enables `retain_rejected_speech` on
`interaction.ModelResultCommit`. The default remains strict compare-and-append.

1. Committed-context model invocations carry their verified immutable prefix
   identity into the completed result. Unbound legacy contexts do not acquire
   this history-retention capability.
2. The ordinary result first attempts its original version-checked transaction.
3. Only an exact version-conflict reply for that pending run can start a
   separate history transaction. The latter contains the original instruction
   and sanitized voice-authorized assistant text, with new canonical item IDs.
   Reasoning, tool proposals, calls, and opaque provider state are excluded,
   including when the original output mixed speech and a proposed tool action.
4. `state.TrajectoryStore` verifies the original prefix digest while holding
   the append lock. Later observations may extend that prefix; they never
   become a substituted source for the old output. Canonical commit time moves
   to the insertion boundary. The complete batch validates atomically.
5. The commit consumer verifies the original prefix, context tail, exact item
   contents and IDs, and deterministic timestamp normalization. Missing,
   altered, cross-session, duplicate, or unrelated receipts cannot attest it.
6. The terminal outcome is `speech_retained`, not `committed`. It grants no
   authority to an old proposal. Assistant items remain `prepared` until the
   existing exact playback receipts establish their played or cancelled state.
7. A prefix failure or any other rejection remains terminal negative evidence.
   History insertion does not retry provider calls or infer the intended words
   from recognized audio or the expected benchmark answer.

The provider-neutral graph owns both transactions. The session adapter continues
its existing playback visibility projection; it does not fabricate missing
assistant content from a synthesis plan or the wire transcript. The public
Realtime protocol and stable `api/v1` interfaces are unchanged.

## Consequences and limits

Later continuations can learn what was actually played even when the full
provider result lost freshness. A stale result still cannot supply an executable
proposal through this history transaction. Models that did not have voice
authority and legacy results without a verified prefix retain the strict path.

This adds an explicit provenance-only append operation. Verifying an old prefix
proves its identity, not that it is current or that a new effect is authorized.
Callers of that operation must preserve this distinction. Existing append and
compare-and-append operations keep their previous timestamp and freshness rules.

The new descriptor/runtime revisions and profile values change frozen identities.
Existing sealed recordings retain their original executables and outcomes.
Mounted tests and synthetic mutation checks do not establish live benchmark
quality, recognizer accuracy, complete cancellation coverage, or release readiness.

## Alternatives considered

- Dropping compare-and-append for every model result would admit stale proposals.
- Re-running the model would not record the words the listener already heard.
- Replacing the result's source with the current prefix would invent provenance.
- Making the adapter reconstruct content from wire text would bypass the graph's
  sanitized result and canonical commit contract.
- Treating all retained words as played would invent speech across interruption.
