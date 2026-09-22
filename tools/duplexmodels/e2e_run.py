#!/usr/bin/env python3
"""Run an isolated duplex profile measurement, retaining every attempt."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tempfile
import time
from datetime import datetime, timezone
from urllib.request import build_opener, ProxyHandler
from urllib.parse import urlsplit, urlunsplit

ROOT = Path(__file__).resolve().parents[2]
CATEGORIES = ('user_interruption', 'user_backchannel', 'background_speech', 'talking_to_other')


def digest(path: Path) -> str:
    with path.open('rb') as source:
        checksum = hashlib.sha256()
        for block in iter(lambda: source.read(1 << 20), b''):
            checksum.update(block)
        return checksum.hexdigest()


def sidecar_sources(root: Path) -> dict[str, str]:
    """Record local Python adapter code, including its shared protocol library.

    External serving processes have separate provenance; this inventory only
    guards scripts that a profile can spawn from this checkout per session.
    """
    return {str(path.relative_to(root)): digest(path)
            for path in sorted((root / 'sidecars').rglob('*.py')) if path.is_file()}


def write_json(path: Path, value: dict) -> None:
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(value, indent=2) + '\n')
    temporary.replace(path)


def now() -> str:
    return datetime.now(timezone.utc).isoformat()


def capture(*command: str) -> str:
    return subprocess.check_output(command, cwd=ROOT, text=True).strip()


def component_health(config: Path) -> dict:
    """Snapshot loopback component settings; never probe remote providers.

    Deployment profiles use scalar top-level *-url keys. Keep failures as
    evidence rather than implying that absent health data validated a service.
    """
    opener = build_opener(ProxyHandler({}))
    result = {}
    for line in config.read_text().splitlines():
        match = re.fullmatch(r"([a-z0-9-]+-url):\s*(\S+)\s*", line)
        if not match:
            continue
        key, value = match.groups()
        parsed = urlsplit(value.strip("\"'"))
        if (parsed.hostname not in ('127.0.0.1', 'localhost', '::1') or parsed.username
                or parsed.scheme not in ('http', 'https', 'ws', 'wss')):
            continue
        scheme = 'https' if parsed.scheme in ('https', 'wss') else 'http'
        url = urlunsplit((scheme, parsed.netloc, '/health', '', ''))
        entry = {'url': url, 'captured': now()}
        try:
            with opener.open(url, timeout=3) as response:
                payload = response.read((1 << 20)+1)
            if len(payload) > 1 << 20:
                raise ValueError('health response exceeds one MiB')
            entry['response'] = json.loads(payload)
        except Exception as error:
            entry['error'] = f'{type(error).__name__}: {error}'
        result[key] = entry
    return result


def owns_listener(pid: int, port: int) -> bool:
    """Require this server's socket, not another process's healthy endpoint (Linux)."""
    try:
        sockets = {entry.readlink().name for entry in Path(f'/proc/{pid}/fd').iterdir()}
        for table in ('tcp', 'tcp6'):
            for line in Path(f'/proc/{pid}/net/{table}').read_text().splitlines()[1:]:
                fields = line.split()
                if (int(fields[1].split(':')[1], 16) == port and fields[3] == '0A'
                        and f'socket:[{fields[9]}]' in sockets):
                    return True
    except (OSError, ValueError):
        pass
    return False


def validate_result(path: Path) -> str:
    try:
        data = json.loads(path.read_text())
        tasks = data.get('tasks')
        if not isinstance(tasks, list) or not tasks:
            return 'no tasks recorded'
        errors = sum(bool(task.get('error')) for task in tasks)
        return f'{errors} task errors' if errors else ''
    except (OSError, ValueError, TypeError, AttributeError) as error:
        return f'invalid result: {error}'


def terminate(process: subprocess.Popen) -> None:
    # The server owns a new session, including any spawned model sidecars.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        pass
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait()


def run(profile: str, per_category: int, conversations: int, *, full_selected: bool = False) -> int:
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]*', profile):
        raise ValueError('invalid profile name')
    config = ROOT / 'deploy/duplex/profiles' / f'{profile}.yaml'
    binary = Path(os.environ.get('OPENREALTIME_BIN', str(ROOT / '.runtime/duplex-plan/bin/openrealtime'))).resolve()
    if not config.is_file() or not binary.is_file():
        raise ValueError('profile or executable does not exist')
    if per_category <= 0 or conversations < 0:
        raise ValueError('FDB count must be positive; FD-Bench count must be nonnegative')
    bench_timeout = float(os.environ.get('E2E_BENCH_TIMEOUT', '86400' if full_selected else '1800'))
    if not 0 < bench_timeout < float('inf'):
        raise ValueError('benchmark timeout must be finite and positive')
    port = int(os.environ.get('E2E_PORT', '9290'))
    parent = ROOT / '.runtime/duplex-plan/results/e2e' / profile
    runs = parent / 'runs'
    runs.mkdir(parents=True, exist_ok=True)
    out = Path(tempfile.mkdtemp(prefix=datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ-'), dir=runs))
    # Publish the latest attempt immediately, including a failed/incomplete one.
    link = parent / f'.latest-{out.name}'
    link.symlink_to(Path('runs') / out.name)
    link.replace(parent / 'latest')
    (out / 'profile.yaml').write_bytes(config.read_bytes())
    try:
        gpu = capture('nvidia-smi', '--query-gpu=memory.used', '--format=csv,noheader,nounits')
    except (OSError, subprocess.SubprocessError):
        gpu = 'unknown'
    expected = [f'fdb-{category}.json' for category in CATEGORIES]
    if conversations or full_selected:
        expected.append('fdbench.json')
    metadata = {
        'schema': 2, 'profile': profile, 'bench_timeout_seconds': bench_timeout,
        'campaign_scope': 'all FDB categories and complete cosyvoice2-single-round-combine-med' if full_selected else 'smoke subset', 'profile_sha256': digest(out / 'profile.yaml'),
        'binary_sha256': digest(binary), 'binary': str(binary),
        'sidecar_source_sha256': sidecar_sources(ROOT),
        'revision': capture('git', 'rev-parse', 'HEAD'),
        'tree_modified': bool(capture('git', 'status', '--porcelain', '--', '.', ':!.runtime')),
        'started': now(), 'load_average': Path('/proc/loadavg').read_text().split()[:3],
        'gpu_memory_mib': gpu, 'fdb_per_category': 0 if full_selected else per_category,
        'fdbench_conversations': 0 if full_selected else conversations, 'expected_results': expected,
        'component_health': component_health(config),
    }
    write_json(out / 'run.json', metadata)
    server = None
    commands = {}
    errors = []
    status = 1
    try:
        # Fail before launching if the requested port is already occupied.
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(('127.0.0.1', port))
        with (out / 'server.log').open('wb') as log:
            server = subprocess.Popen([str(binary), 'serve', '-config', str(out / 'profile.yaml'),
                                       '-listen', f'127.0.0.1:{port}', '-timeline-log', str(out / 'timeline.log')],
                                      cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        opener = build_opener(ProxyHandler({}))
        deadline = time.monotonic() + float(os.environ.get('E2E_READY_TIMEOUT', '300'))
        while time.monotonic() < deadline:
            if server.poll() is not None:
                raise RuntimeError(f'server exited during startup: {server.returncode}')
            if owns_listener(server.pid, port):
                try:
                    with opener.open(f'http://127.0.0.1:{port}/healthz', timeout=2) as response:
                        (out / 'healthz.json').write_bytes(response.read())
                    if server.poll() is None and owns_listener(server.pid, port):
                        break
                except OSError:
                    pass
            time.sleep(0.1)
        else:
            raise RuntimeError('server readiness timeout')
        for filename in expected:
            if sidecar_sources(ROOT) != metadata['sidecar_source_sha256']:
                raise RuntimeError('local sidecar source inventory changed during run')
            if server.poll() is not None or not owns_listener(server.pid, port):
                raise RuntimeError('owned server is no longer listening')
            if filename == 'fdbench.json':
                arguments = ['fdbench', '-conditions', 'cosyvoice2-single-round-combine-med', '-limit', str(0 if full_selected else conversations)]
            else:
                arguments = ['fdb', '-categories', filename[4:-5], '-limit', str(0 if full_selected else per_category)]
            command = [str(binary), 'bench', *arguments, '-endpoint', f'ws://127.0.0.1:{port}/v1/realtime',
                       '-cell', profile, '-out', str(out / filename)]
            with (out / filename.replace('.json', '.log')).open('wb') as log:
                process = subprocess.Popen(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
                try:
                    code = process.wait(timeout=bench_timeout)
                except subprocess.TimeoutExpired:
                    terminate(process)
                    code = 124
                except BaseException:
                    terminate(process)
                    raise
            error = validate_result(out / filename)
            commands[filename] = {'argv': command, 'exit_code': code, 'error': error}
            if code or error:
                errors.append(f'{filename}: exit {code}; {error}')
        if digest(binary) != metadata['binary_sha256']:
            errors.append('executable changed during run')
        if sidecar_sources(ROOT) != metadata['sidecar_source_sha256']:
            errors.append('local sidecar source inventory changed during run')
        if server.poll() is not None or not owns_listener(server.pid, port):
            errors.append('owned server exited before measurement completed')
        status = int(bool(errors))
    except (Exception, KeyboardInterrupt) as error:
        errors.append(str(error) or type(error).__name__)
    finally:
        if server is not None:
            terminate(server)
        write_json(out / 'finished.json', {
            'finished': now(), 'exit_code': status, 'errors': errors, 'commands': commands,
            'load_average_end': Path('/proc/loadavg').read_text().split()[:3],
            'sha256': {name: digest(out / name) for name in ['run.json', 'profile.yaml', *expected] if (out / name).is_file()},
        })
        print(f'results in {out}; exit {status}', flush=True)
        for error in errors:
            print(error, flush=True)
    return status


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('profile')
    parser.add_argument('per_category', type=int, nargs='?', default=10)
    parser.add_argument('conversations', type=int, nargs='?', default=12)
    parser.add_argument('--full-selected', action='store_true',
                        help='all FDB recordings and all conversations in the selected clean FD-Bench condition')
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise KeyboardInterrupt(f'signal {signum}')
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(run(args.profile, args.per_category, args.conversations, full_selected=args.full_selected))


if __name__ == '__main__':
    main()
