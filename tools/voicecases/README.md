# voicecases

Turn-level cases for the voice, replayed from real request dumps.

Each case is an actual provider request captured by
`OPENREALTIME_DUMP_REQUESTS` during a scenario run, together with the answer
that turn should have given: `wait` for the turns where the right answer is
silence, `speak` for the turns where it is not.

## Why these exist

The scenario suite measures conversations, and a conversation is a compound
event: one policy fires a dozen times, the recogniser splits the audio
differently on every run, and every one of those turns has to go right for the
run to pass. Measured at fifteen repeats, scenarios in this suite sit anywhere
from 53% to 100%, so a five-run reading of one cannot separate a real change
from a lucky draw - and several conclusions in docs/measurement.md had to be
retracted for exactly that reason (F62, F63).

A case here is one turn, one prompt, one answer, at temperature zero. It
isolates the request that exposed a defect and allows repeated comparison. It cannot tell you
whether a scenario will pass, and F64 records a fix that was real here and
invisible there. Both measurements are needed and they answer different
questions.

## Running them

    python3 tools/voicecases/run.py                 # as committed
    python3 tools/voicecases/run.py pairs.json      # with prompt edits applied

`pairs.json` is a list of `[old, new]` string replacements applied to each
case's system prompt, which is how a candidate wording is measured before it is
committed. A case whose prompt does not contain the anchor is reported rather
than silently skipped: the dumps were captured at different times and not all
of them carry the same text.

## Adding a case

Run a scenario under `probe.sh`, find the request in the dump whose last
message is the moment you care about, and add it with the answer it should have
given. Reconstructing a prompt by hand does not work - four attempts at the
interpreting failure were spent on a hand-written wrapper that behaved
differently from the real one (F61).
