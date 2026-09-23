"""Attach to an orphaned fdbench process, sample resources, and write a
reconstructed completion record once it exits. The original runner was killed
without its finally block, so its finished.json can never exist; this file is
named finished-reconstructed.json to keep that distinction visible."""
import hashlib, json, os, subprocess, sys, time
from datetime import datetime, timezone
from pathlib import Path

RUN = Path(sys.argv[1]); BENCH = int(sys.argv[2]); SERVER = int(sys.argv[3])
now = lambda: datetime.now(timezone.utc).isoformat()
def ticks(pid):
    try: return Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[19]
    except OSError: return None
def alive(pid, t): return ticks(pid) == t
def smi(*q):
    try: return subprocess.run(['nvidia-smi', *q, '--format=csv,noheader,nounits'], capture_output=True, text=True, timeout=20).stdout
    except Exception as e: return ''
def append(path, rec):
    with open(path, 'a') as f: f.write(json.dumps(rec) + '\n')
def digest(p):
    h = hashlib.sha256()
    with open(p, 'rb') as f:
        for b in iter(lambda: f.read(1 << 20), b''): h.update(b)
    return h.hexdigest()

samples = RUN / 'resources-resumed-2.jsonl'
bt, st = ticks(BENCH), ticks(SERVER)
append(samples, {'at': now(), 'event': 'attached', 'pid': BENCH, 'start_ticks': bt, 'server_pid': SERVER,
                 'interval_seconds': 10, 'scope': 'shared host; sampled, not allocator peaks; attached to orphaned fdbench after runner loss'})
while alive(BENCH, bt):
    gpu = [dict(zip(['index', 'memory.used', 'memory.total', 'utilization.gpu', 'temperature.gpu'], [x.strip() for x in l.split(',')]))
           for l in smi('--query-gpu=index,memory.used,memory.total,utilization.gpu,temperature.gpu').splitlines() if l.strip()]
    procs = [dict(zip(['pid', 'used_memory'], [x.strip() for x in l.split(',')]))
             for l in smi('--query-compute-apps=pid,used_memory').splitlines() if l.strip()]
    append(samples, {'at': now(), 'event': 'sample', 'load_average': [float(x) for x in Path('/proc/loadavg').read_text().split()[:3]],
                     'gpu': gpu, 'processes': procs, 'server_alive': alive(SERVER, st)})
    time.sleep(10)
append(samples, {'at': now(), 'event': 'target_exited', 'pid': BENCH})
time.sleep(2)
result = RUN / 'fdbench.json'
error = ''
try:
    data = json.loads(result.read_text()); tasks = data.get('tasks')
    if not isinstance(tasks, list) or not tasks: error = 'no tasks recorded'
    elif sum(bool(t.get('error')) for t in tasks): error = f"{sum(bool(t.get('error')) for t in tasks)} task errors"
    elif data.get('summary', {}).get('complete') is False: error = f"incomplete benchmark: {data['summary'].get('incompleteness')}"
except (OSError, ValueError, TypeError, AttributeError) as e:
    error = f'invalid result: {e}'
names = ['run.json', 'profile.yaml', 'fdb-user_interruption.json', 'fdb-user_backchannel.json',
         'fdb-background_speech.json', 'fdb-talking_to_other.json', 'fdbench.json']
json.dump({'finished': now(), 'reconstructed': True,
           'reason': 'runner PID 3810341 was killed ~08:05:48-08:05:58 UTC without executing its finally block; fdbench child PID survived in its own session',
           'fdbench_exit_code': 'unobservable (process is not a child of the finalizer)',
           'fdbench_validation_error': error, 'server_alive_at_bench_exit': alive(SERVER, st),
           'load_average_end': Path('/proc/loadavg').read_text().split()[:3],
           'sha256': {n: digest(RUN / n) for n in names if (RUN / n).is_file()}},
          open(RUN / 'finished-reconstructed.json', 'w'), indent=2)
