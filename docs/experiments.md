# Experimental protocol and prospective hypotheses

This document freezes the initial direction of the M0/M1 research program
before engine optimization. Changes after benchmark inspection must be dated
and labeled exploratory.

## Conditions

- **B0 endpointed cascade:** all downstream work begins after final endpoint.
- **B1 streaming conventional:** perception streams, response initiation still
  waits for endpoint.
- **M1 fixed microturn:** the B0 component models receive fixed-cadence decision
  opportunities.
- **M2 adaptive microturn:** stability and turn projection can open a decision.
- **M3 fast/slow:** foreground coordination and deliberation are separate.
- **N1 native realtime:** public native audio behavior, where access permits.
- **H1 hybrid:** acoustic or native interaction signals with modular cognition.

The same fixture, prompt, model version, voice, region, device path, network
condition, and random seed are paired wherever the provider permits.

## Primary prospective tests

### H1 cadence

For predictable controlled turns, fixed-cadence microturn scheduling with
incremental perception will reduce median end-of-user-speech to first semantic
audio and median absolute turn-gap error relative to B0 using identical
component models. Primary guardrails are task success, premature takeover rate,
and audible false-start duration. The claim fails if timing improves but a
guardrail’s confidence interval exceeds the preregistered non-inferiority
margin; margins will be set by power analysis before confirmatory collection.

### H2 stable-prefix planning

Planning on stable prefixes will improve first semantic audio latency on
predictable utterances relative to endpoint-only planning. Ambiguous minimal
pairs are expected to require uncertainty-aware deferral. Primary failure
measures are candidate supersession, committed wrong starts, and repairs.

### H3 speculation

Prepared-but-unplayed text and audio will reduce onset only when candidates
carry explicit validity and cancellation. The ungated ablation is expected to
increase audible false-start duration and reduce blinded trust ratings.

### H4 fast/slow separation

On difficult questions, an honest bounded acknowledgement plus cancellable
deliberation will improve the joint latency-quality-cost frontier relative to a
single blocking path. Acknowledgements that imply nonexistent tool progress are
scored untruthful.

## Trial and analysis rules

Use paired prerecorded trials; randomize run order; retain warm and cold starts
as separate strata; declare exclusions before final collection; record every
failed trial; and report effect sizes and confidence intervals. Provider outage
and malformed output are outcomes unless a prospective infrastructure rule
excludes them. Exploratory tuning and confirmatory evaluation use disjoint
fixtures.
