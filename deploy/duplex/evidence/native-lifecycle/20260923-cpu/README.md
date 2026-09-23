# Native session lifecycle checks, 2026-09-23

Five CPU regression tests passed using:

```
PYTHONPATH=sidecars .runtime/duplex-plan/venvs/minicpm-duplex/bin/python -m pytest -q sidecars/test_moshi_sidecar.py sidecars/test_personaplex_sidecar.py
```

The slow-worker regression failed before feefa586: session shutdown released
the shared model lock while inference remained alive. The reset-failure
regression failed before 3c01b983: failed Moshi setup retained its lock and
blocked later sessions. Both now pass alongside the prior cancellation and
PersonaPlex reset tests.

The retained JSON reports come from `openrealtime conformance sidecar --
<python> sidecars/<model>_sidecar.py --mock` for Moshi and PersonaPlex.
Both protocol-v1 mock checks passed. These do not establish GPU behavior,
model quality, graph-native v4 support, or rendered playback cancellation.
The live PersonaPlex campaign started at 01:29 UTC uses earlier pinned code.
