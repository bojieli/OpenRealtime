# PersonaPlex resource observation gap

The original sampler exited with ENOSPC while writing resources.jsonl. Its
last complete sample is 2026-09-23T02:52:03.807007+00:00; a partial trailing
record remains unchanged in the original run. Sampling resumed in a separate
file, whose attachment and first sample are retained here. No resource
stability claim spans this gap. The benchmark runner and model stayed live;
server and model logs showed no ENOSPC or session errors when inspected.
This does not prove that every other log write succeeded during disk pressure.

On discovery the filesystem had about 10 GiB available. A subsequent pip cache
purge removed 386 disposable cache files; df then reported 11 GiB available.
Installed environments, checkpoint files and benchmark evidence were retained.
The campaign remains in progress.
