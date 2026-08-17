# Prospective human-study protocol v0.1

Status: not run. No recruitment, recording, intervention, or participant data
collection is authorized by this repository document.

## Objective and conditions

The future confirmatory study will compare endpointed, microturn, native, and
hybrid audio interactions only after each condition passes protocol, safety,
and pilot reliability gates. Primary hypotheses, outcomes, and exclusions are
frozen in `benchmarks/releases/v0.1.0/preregistration.json`. Provider versions,
voices, regions, devices, network conditions, prompts, and playback gain must
be recorded and paired where technically possible.

## Design

- Use a within-participant randomized crossover design with order balanced by
  a reproducible schedule prepared before enrollment.
- Blind participants and outcome raters to system identity where voices and
  feature behavior permit; record every unavoidable unblinding cue.
- Keep confirmatory fixtures disjoint from tuning and pilot fixtures.
- Obtain the target sample through a documented power analysis using a pilot
  variance estimate and the preregistered smallest effect of interest. No
  convenience sample size is specified in advance of that analysis.
- Analyze language groups separately before any pooled estimate.

Primary timing outcome is observed end-of-user-speech to first semantic audio.
Primary human outcomes are task success and blinded trust/naturalness ratings.
Guardrails are premature takeover, false stop, failure-to-stop, audible false
start, repair, contradiction, and cognitive burden. Latency non-inferiority is
not sufficient if a guardrail crosses its prospectively powered margin.

## Exclusions and failures

Exclude only prospectively named infrastructure failures: corrupted source
fixture, missing hardware capture, or failure of the randomization assignment.
Provider timeouts, malformed outputs, safety refusals, task errors, and user
interruptions remain outcomes. Report enrollment, exclusions, withdrawals,
missingness, and adverse events by condition.

## Ethics, consent, and privacy gate

Before recruitment, an accountable investigator must obtain the applicable
institutional ethics/IRB determination, provide informed-consent and withdrawal
materials, assess accessibility, and define participant compensation. Voice is
identifying biometric data: collect the minimum necessary, encrypt it, restrict
access, define a deletion date, and never publish raw recordings without
specific consent and a rights review. Remove direct identifiers from analysis
data and document residual re-identification risk.

Any deployment involving minors, health data, employment decisions, covert
recording, or high-stakes advice requires a separate protocol and is outside
this study.

## Analysis and release gate

Freeze the statistical analysis code and manifest hashes before unblinding.
Report paired effect sizes, confidence intervals, distribution tails, all
guardrails, and condition-specific failures. Label deviations and exploratory
analyses. A public release requires license/consent review of every fixture and
artifact plus verification that withdrawal commitments can still be honored.
