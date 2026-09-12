<!--
CONTRIBUTING.md carries the full checklist and the reasoning behind it. This
template is the short form: the things a reviewer cannot reconstruct from the
diff.
-->

## What this changes

<!-- The problem, who it affects, and why this approach. -->

## How it was verified

<!--
`./scripts/check.sh` is the gate. Paste what it reported, and name anything you
ran beyond it. If a stage skipped because an optional dependency is missing,
say which - a skipped integration is not evidence that it passed.

On a large shared machine, run it as
`OPENREALTIME_TEST_PARALLEL=4 ./scripts/check.sh`.
-->

## If this adds a regression test

<!--
Confirm the test fails for the defect it claims to catch: reintroduce the
defect or make the smallest equivalent mutation, watch it fail for the expected
reason, then restore. Say that you did.
-->

## Origin and license of anything imported

<!--
Code, data, prompts, model weights, generated assets. Write "none" when there
are none.
-->
