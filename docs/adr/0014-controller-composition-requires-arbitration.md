# ADR-0014: Controller composition requires explicit arbitration

**Status:** accepted.

## Context

OpenRealtime already separates available capabilities from selected ownership
and attests the exact evidence given to the interaction owner. That is
necessary but not sufficient when several interaction mechanisms are installed.

The component runtime can contain acoustic predicates and a learned
enumerated-act policy at the same time. Earlier definitions called that whole
deployment `text-policy`, but a launch flag decided whether the model replaced
the floor. Immediate predicate barge-in remained active even when the model did
replace it. Two launches could therefore carry one architecture fingerprint
while selecting different controllers for the most consequential decisions.

Treating the combination as another binding name would reproduce the original
mistake: every predicate/model/native combination would need another species
and code branch. Treating all installed policies as active is worse, because
two independent writers can issue contradictory acts for one live moment.

## Decision

The interaction boundary now carries two additional pieces of selected state:

- a composable controller vector: predicates, external text policy, native
  interaction, and remote interaction;
- one arbitration rule which reduces the selected mechanisms to one
  authoritative act.

One selected controller uses `single` arbitration. The first composed rule is
`predicate-floor`, selecting exactly predicates plus the external text policy.
Predicates retain endpoint and overlap/barge-in decisions. The text policy owns
durable semantic instructions, narrated or direct visual decisions, quiet
clock acts, and silent-tool acts. Both see only their declared evidence.

Pure external T is now genuinely different: the enumerated-act model owns both
endpoint and overlap decisions. Bare acoustic onset has no linguistic evidence
and therefore produces the inertial `keep-speaking` act. Once transcript
evidence arrives, the policy selects `keep-speaking` or `stop-speaking` through
the same act vocabulary it uses elsewhere.

The full-T floor and overlap adapters inherit the policy model's attested
decision deadline. They may not silently impose a shorter local timeout while
the cell records only the longer model timeout. A deployment that needs a
different deadline must select and attest it as part of the policy deployment.

Safety and authority checks are not additional controllers. Liveness bounds,
stale-moment refusal, duplicate-act suppression, tool authorization, and
execution fences may reject or bound an act, but they do not propose a competing
conversational act.

Catalog definitions, live status, authored cells, per-task observations, and
comparison artifacts all retain the controller vector and arbitration. A
definition authored before this field existed remains runnable and inspectable
with its immutable fingerprint, but it cannot author a current controlled cell.

F52 consequently has four treatment labels:

- P: one predicate controller;
- T: one external text-policy controller;
- C: predicate and text-policy controllers under an explicit arbitration rule;
- N: one native interaction controller.

C is a composition, not a model species. `cascade.text-policy@3` and
`cascade.composed-policy@1` use the same component topology, while
`cascade.text-policy-visual-speaker@2` and
`cascade.composed-policy-visual-speaker@1` hold enriched evidence and producer
identity fixed for the first T/C experiment.

## Consequences

Architecture selection derives full versus composed controller wiring from
catalog data. An explicitly contradictory `-interaction-floor` setting is
refused. New supported controller combinations can be expressed in an external
catalog without adding an architecture-ID branch, provided the named generic
arbitration rule is implemented by the topology.

The first complete T/C diagnostic showed why the arbitration contract must
continue to evolve as selected data. `predicate-floor` recovered two
deadline-sensitive menu/waiter cases but lost semantic correction-overlap and
one explicit long-silence case relative to full T; it was not a monotonic union
of their strengths. A later revision should split endpoint, take-floor,
yield-floor, response-veto, concurrent-speech, and silent-action jurisdiction,
or specify an equivalent priority/veto relation. That is an evolution of the
generic controller composition surface, not evidence for another binding
species.

The sidecar topology currently implements single predicate, single external
text, and single native selection. It honestly refuses `predicate-floor`
composition until typed sidecar acts expose enough jurisdiction to implement
that arbitration. Unsupported composition is an unavailable architecture, not
a translated approximation.

T/C is an architecture-only comparison only when foreground, models, exact
evidence, producers, non-treatment policies, observers, authority, fixture,
hardware, and provenance match. P/C and C/N change more than one treatment
dimension and remain system comparisons unless a later experimental design
defines and validates a narrower adjacency.

## Alternatives considered

**Infer composition from policy names.** Rejected because a report saying both
`immediate` and `model:qwen` does not say which one may end or interrupt a turn.

**Let every controller vote.** Rejected because voting still needs a specified
conflict rule, deadlines, abstention semantics, and one writer. Leaving those
implicit makes the runtime scheduling order the policy.

**Make predicate plus model another binding.** Rejected because the combination
uses the existing component topology and would turn a selected capability
vector back into a model taxonomy.

**Call engine safety bounds predicates.** Rejected because a fence can refuse an
unsafe or stale act but never selects what conversational act should happen.
Conflating policy and enforcement would make removing a learned controller
appear to remove safety.
