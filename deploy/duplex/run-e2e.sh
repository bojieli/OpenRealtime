#!/usr/bin/env bash
# Run one duplex profile against its already-running component services.
# Usage: run-e2e.sh PROFILE [FDB_PER_CATEGORY=10] [FDBENCH_CONVERSATIONS=12]
# Every attempt is retained under results/e2e/PROFILE/runs/; latest points to
# the most recent attempt, including failed attempts. Subsets are smoke tests.
set -euo pipefail
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
exec python3 "${repository}/tools/duplexmodels/e2e_run.py" "$@"
