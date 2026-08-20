# Proposal: `api/v2`

Status: proposal. Nothing here is implemented, and `api/v1` remains the
supported contract. This exists because M9 lists an `api/v2` proposal as an
open deliverable and because the M10 pilot produced concrete evidence about
what `v1` cannot express.

A new semantic import path is the only way to make a breaking change to the
component contract, so the bar is evidence that the current contract cannot
carry something real. This proposes three changes and deliberately declines a
fourth.

## The evidence

Across the three complete pre-freeze pilot populations and the first complete
frozen canonical airline cell (328 simulations), 34 runs ended in
`too_many_errors` -- the tau2 environment-error budget, `max_errors = 10`.
Every one is the agent failing to resolve a spoken alphanumeric identifier.
This is post-hoc mechanism evidence, not a causal comparison between the pilot
and frozen runtimes. Classified by
`scripts/classify-tau-voice-failures.py`:

| Recorded outcome/mechanism | Count |
| --- | --- |
| `too_many_errors`: `identifier_variant_search` | 31 |
| `too_many_errors`: `spelled_token_not_reassembled` | 3 |
| `infrastructure_error` (separate; excluded from scoring) | 18 |

The agent asked the user to spell the identifier letter by letter in 33 of the
34 error-terminated runs, so this is not a missing repair strategy. Repair runs
and does not converge. In one retail run the agent received the spelled letters
and called
`find_user_id_by_name_zip{last_name: "C I", first_name: "M", zip: "K O V A C S"}`.
It held the correct characters and could not join them into a token,
distributing them across fields instead. The reference was `Mei Kovacs / 28236`.

## 1. Perception must be able to say how a token was spoken

`PerceptionRevision` carries `StableText`, `UnstableText`, `Delta`, and
`Final`. Every one is flat text. When a speaker says "K, O, V, A, C, S", the
only faithful rendering into that struct is the literal string `K O V A C S`,
and every consumer downstream must re-infer that those six characters are one
token. The evidence above is what that re-inference failing looks like.

The v1 struct cannot be extended compatibly here: consumers already treat
`StableText` as the text to use, so any new field is advisory and a consumer
that ignores it stays wrong.

```go
// v2
type SpokenForm string

const (
    SpokenProse     SpokenForm = "prose"      // ordinary speech
    SpokenSpelled   SpokenForm = "spelled"    // "K, O, V, A, C, S"
    SpokenDigits    SpokenForm = "digits"     // "seven, three, four, zero"
    SpokenPhonetic  SpokenForm = "phonetic"   // "kilo, oscar, victor"
)

type Span struct {
    Form        SpokenForm `json:"form"`
    Text        string     `json:"text"`        // as heard: "K O V A C S"
    Reassembled string     `json:"reassembled"` // as intended: "KOVACS"
    Alternates  []string   `json:"alternates"`  // ranked, may be empty
}

type PerceptionRevision struct {
    // ... v1 fields ...
    Spans []Span `json:"spans"`
}
```

`Reassembled` is the provider's own claim, not a guess made downstream by
whoever happens to consume the text. A provider that cannot reassemble leaves
it empty, which is an honest absence a consumer can branch on -- unlike a
silently wrong join.

`Alternates` addresses the other 31 cases. The agent searched identifier
variants one failed tool call at a time, spending the whole error budget to
enumerate what a recognizer could have offered at once.

## 2. Repair must be a typed action, not a sentence

`FastAction` is `listen | acknowledge | answer | defer | yield`. Asking the
user to repeat or spell something is an `answer` whose text happens to be a
question. The runtime therefore cannot count repair attempts, cannot bound
them, and cannot tell a repair loop from progress.

This is why `classify-tau-voice-failures.py` detects repair by searching the
agent's speech for the word "spell". That is a workaround for a contract gap,
and it would silently under-count any agent that phrases the request
differently.

```go
// v2
const FastRepair FastAction = "repair"

type RepairRequest struct {
    Target  string     `json:"target"`  // the field being repaired
    Form    SpokenForm `json:"form"`    // the form being requested
    Attempt int        `json:"attempt"` // 1-based
}
```

A typed repair action makes the loop observable and boundable: a runtime can
refuse a fourth consecutive repair on the same target and hand off, instead of
spending an error budget.

## 3. Capabilities must cover the new surface

```go
const (
    CapabilitySpokenForms      Capability = "spoken_forms"
    CapabilityAlternates       Capability = "alternates"
    CapabilityTypedRepair      Capability = "typed_repair"
)
```

Existing capability negotiation already carries this; the constants are the
only addition. A v1 provider adapted into a v2 runtime declares none of them
and behaves exactly as it does today.

## No v2 change: making the stable partial canonical

M9 also lists canonical stable-partial effects. That is a commit-policy change
inside the engine -- whether a declared stable partial may take tool effects
before endpoint -- and `docs/canonical-trajectory.md` already states it must
preserve the same causal and authority invariants. It needs no contract
change, so it does not belong in `v2`. The gateway now implements that policy
post-freeze as the opt-in `stable-partial` observation mode. Eligibility comes
only from provider-typed `StableText`; later revisions carry typed supersession
provenance, and only committed slow calls execute. The frozen M8–M10 executable
remains endpoint-only, so this implementation is unmeasured and does not alter
the evidence motivating the v2 spoken-form proposal.

## What this proposal does not claim

None of this is measured. The pilot shows what the current contract cannot
express; it does not show that expressing it fixes the failures. The
reassembly defect in particular sits in the consumer, and a provider that
populates `Reassembled` correctly still depends on a consumer that reads it.

Adopting `v2` would require, in order: an implementation behind the new import
path, a v1-to-v2 adapter with the compatibility test, a migration guide, and a
declared study condition measuring the change. Applying any of it to the
running study would alter the system under test, so it is post-study work.
