# M5 reference artifacts

These files are deterministic outputs of `./scripts/reproduce_m5.sh` at seed
20260817 with 30 trials per condition. The report excludes embedded traces;
one complete trace per condition and the two public-demo timelines are retained
for byte-stable review.

All translation traces validate against the OpenAI Translation profile. All
Signal Match traces validate against the GA Realtime profile. Scores and
compute units are symbolic instrumentation over the project-authored CC0
fixture, not provider benchmarks.
