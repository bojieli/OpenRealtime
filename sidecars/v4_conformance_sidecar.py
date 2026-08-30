#!/usr/bin/env python3
"""Minimal subprocess fixture for the graph-native protocol-v4 probe."""

from openrealtime_sidecar import ConformanceElementSidecar, run_element


if __name__ == "__main__":
    run_element(ConformanceElementSidecar)
