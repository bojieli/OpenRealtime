"""Failure-path checks for retained end-to-end measurements (no model needed)."""
import json
import os
from pathlib import Path
import socket
import tempfile
import unittest
from unittest.mock import patch

import e2e_run
import e2e_summary

FAKE = '''#!/usr/bin/env python3
import http.server, json, os, sys
from pathlib import Path
args=sys.argv[1:]
if args[0]=='serve':
    if os.environ.get('FAKE_EXIT'): sys.exit(3)
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200); self.end_headers(); self.wfile.write(b'{}')
        def log_message(self, *args): pass
    port=int(args[args.index('-listen')+1].split(':')[1])
    http.server.HTTPServer(('127.0.0.1',port),Handler).serve_forever()
else:
    out=Path(args[args.index('-out')+1])
    task={'passed':True,'notes':{'applicable':'true'},'metrics':{'turns':1,'answered':1}}
    if os.environ.get('FAKE_TASK_ERROR'): task['error']='connection refused'
    out.write_text(json.dumps({'tasks':[task]}))
    sys.exit(int(os.environ.get('FAKE_BENCH_EXIT','0')))
'''


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        profiles = self.root / 'deploy/duplex/profiles'
        profiles.mkdir(parents=True)
        (profiles / 'sample.yaml').write_text('binding: duplex\n')
        binary = self.root / 'fake'
        binary.write_text(FAKE)
        binary.chmod(0o755)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        self.patches = [patch.object(e2e_run, 'ROOT', self.root),
                        patch.object(e2e_run, 'capture', return_value='test'),
                        patch.dict(os.environ, OPENREALTIME_BIN=str(binary), E2E_PORT=str(self.port), E2E_READY_TIMEOUT='2')]
        for item in self.patches:
            item.start()
            self.addCleanup(item.stop)
        self.parent = self.root / '.runtime/duplex-plan/results/e2e/sample'

    def test_component_health_captures_settings_without_probing_remote_or_credentials(self):
        import io
        config=self.root/'components.yaml'
        config.write_text('tts-url: ws://127.0.0.1:9124/v1/tts/stream\n'
                          'slow-url: https://remote.example/v1\n'
                          'asr-url: http://secret@localhost:9110/api\n')
        from unittest.mock import Mock
        opener=Mock()
        opener.open.return_value=io.BytesIO(b'{"compiled":false,"sample_rate":44100}')
        with patch.object(e2e_run,'build_opener',return_value=opener):
            result=e2e_run.component_health(config)
        self.assertEqual(list(result),['tts-url'])
        self.assertFalse(result['tts-url']['response']['compiled'])
        opener.open.assert_called_once_with('http://127.0.0.1:9124/health',timeout=3)

    def test_runs_preserved_and_tampering_excluded(self):
        self.assertEqual(e2e_run.run('sample', 1, 1), 0)
        first = (self.parent / 'latest').resolve()
        self.assertEqual(e2e_summary.staleness(first), '')
        self.assertEqual(e2e_run.run('sample', 1, 0), 0)
        self.assertNotEqual(first, (self.parent / 'latest').resolve())
        self.assertTrue((first / 'fdbench.json').exists())
        (first / 'fdbench.json').write_text('{}')
        self.assertIn('MODIFIED', e2e_summary.staleness(first))

    def test_full_selected_removes_limits_and_keeps_condition_explicit(self):
        self.assertEqual(e2e_run.run('sample', 1, 0, full_selected=True), 0)
        metadata = json.loads((self.parent / 'latest/run.json').read_text())
        self.assertEqual(metadata['bench_timeout_seconds'], 86400)
        done = json.loads((self.parent / 'latest/finished.json').read_text())
        self.assertEqual(len(done['commands']), 5)
        for entry in done['commands'].values():
            argv = entry['argv']
            self.assertEqual(argv[argv.index('-limit') + 1], '0')
        argv = done['commands']['fdbench.json']['argv']
        self.assertEqual(argv[argv.index('-conditions') + 1], 'cosyvoice2-single-round-combine-med')

    def test_nondefault_condition_is_recorded_and_not_merged(self):
        condition = 'cosyvoice2-single-round-combine-easy-noisy-bg-0dB'
        self.assertEqual(e2e_run.run('sample', 1, 0, full_selected=True,
                                    fdbench_condition=condition), 0)
        path = self.parent / 'latest'
        run = json.loads((path / 'run.json').read_text())
        self.assertEqual(run['fdbench_condition'], condition)
        done = json.loads((path / 'finished.json').read_text())
        argv = done['commands']['fdbench.json']['argv']
        self.assertEqual(argv[argv.index('-conditions') + 1], condition)
        self.assertEqual(e2e_summary.staleness(path), '')
        with self.assertRaises(ValueError):
            e2e_run.run('sample', 1, 1, fdbench_condition='clean,noisy')

    def test_existing_listener_is_not_adopted(self):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', self.port))
            sock.listen()
            self.assertEqual(e2e_run.run('sample', 1, 0), 1)
        done = json.loads((self.parent / 'latest/finished.json').read_text())
        self.assertEqual(done['commands'], {})

    def test_failed_bench_and_task_errors_fail_run(self):
        for variable, value in [('FAKE_BENCH_EXIT', '7'), ('FAKE_TASK_ERROR', '1'), ('FAKE_EXIT', '1')]:
            with self.subTest(variable=variable), patch.dict(os.environ, {variable: value}):
                self.assertEqual(e2e_run.run('sample', 1, 0), 1)
                self.assertIn('FAILED', e2e_summary.staleness(self.parent / 'latest'))

    def test_legacy_is_not_verified_by_mtime(self):
        self.parent.mkdir(parents=True)
        (self.parent / 'run.json').write_text('{}')
        (self.parent / 'finished.json').write_text('{}')
        self.assertIn('UNVERIFIED', e2e_summary.staleness(self.parent))


if __name__ == '__main__':
    unittest.main()


class SourceInventoryTests(unittest.TestCase):
    def test_edit_add_and_delete_change_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'sidecars' / 'protocol' / 'reader.py'
            source.parent.mkdir(parents=True)
            source.write_text('old')
            before = e2e_run.sidecar_sources(root)
            source.write_text('new')
            self.assertNotEqual(before, e2e_run.sidecar_sources(root))
            source.write_text('old')
            extra = source.with_name('writer.py')
            extra.write_text('added')
            self.assertNotEqual(before, e2e_run.sidecar_sources(root))
            extra.unlink()
            self.assertEqual(before, e2e_run.sidecar_sources(root))
            source.unlink()
            self.assertNotEqual(before, e2e_run.sidecar_sources(root))
