#!/usr/bin/env python3
"""Sample shared-host resources until a particular process incarnation exits.

Samples are observations, not allocator peaks or per-session attribution.
Attaching after a run starts leaves its earlier resource use unmeasured.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess
import time


def identity(pid):
    try:
        fields = Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()
        return None if fields[0] == 'Z' else fields[19]
    except (OSError, IndexError):
        return None


def query(kind, fields):
    try:
        result = subprocess.run(['nvidia-smi', f'--query-{kind}={fields}',
                                 '--format=csv,noheader,nounits'],
                                text=True, capture_output=True, timeout=5, check=True)
        names = fields.split(',')
        return [dict(zip(names, [value.strip() for value in line.split(',')]))
                for line in result.stdout.splitlines() if line.strip()]
    except (OSError, subprocess.SubprocessError) as error:
        return {'error': str(error)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pid', type=int, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--interval', type=float, default=10)
    args = parser.parse_args()
    if not 1 <= args.interval <= 60:
        parser.error('interval must be between 1 and 60 seconds')
    token = identity(args.pid)
    if token is None:
        parser.error('target process is not live')
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open('x') as output:
        def emit(data):
            output.write(json.dumps({'at': datetime.datetime.now(datetime.timezone.utc).isoformat(), **data})+'\n')
            output.flush()
        emit({'event': 'attached', 'pid': args.pid, 'start_ticks': token,
              'interval_seconds': args.interval, 'scope': 'shared host; sampled, not allocator peaks'})
        while identity(args.pid) == token:
            emit({'event': 'sample', 'load_average': os.getloadavg(),
                  'gpu': query('gpu', 'index,memory.used,memory.total,utilization.gpu,temperature.gpu'),
                  'processes': query('compute-apps', 'pid,used_memory')})
            deadline = time.monotonic()+args.interval
            while identity(args.pid) == token and time.monotonic() < deadline:
                time.sleep(min(1, max(0, deadline-time.monotonic())))
        emit({'event': 'target_exited_or_changed'})


if __name__ == '__main__':
    main()
