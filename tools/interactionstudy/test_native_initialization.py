"""Exercise failure retention without loading GPU dependencies."""
import asyncio
import json
from pathlib import Path
import runpy
import sys
import tempfile
import types
import unittest
from unittest.mock import patch


def stub_modules(backend=object):
    modules = {}
    for name, clsname, cls in (
        ('duplexcascade_model', 'DuplexCascadeModel', backend),
        ('duplexcascade_speech', 'DuplexCascadeSpeech', object),
        ('microturn_sidecar', 'SpeechContext', object),
    ):
        module = types.ModuleType(name)
        setattr(module, clsname, cls)
        modules[name] = module
    return modules


class ContextErrorTest(unittest.TestCase):
    def test_cancelled_reader_is_not_a_failure(self):
        script = Path(__file__).with_name('native_live.py')
        with patch.dict(sys.modules, stub_modules()), patch.object(sys, 'path', list(sys.path)):
            context_error = runpy.run_path(str(script), run_name='native_live')['context_error']

        async def states():
            async def fail():
                raise ValueError('reader failed')
            cancelled = asyncio.create_task(asyncio.sleep(10))
            failed = asyncio.create_task(fail())
            running = asyncio.create_task(asyncio.sleep(10))
            cancelled.cancel()
            await asyncio.gather(cancelled, failed, return_exceptions=True)
            out = {name: context_error(types.SimpleNamespace(failure=None, reader=task))
                   for name, task in (('cancelled', cancelled), ('failed', failed), ('running', running))}
            out['own'] = context_error(types.SimpleNamespace(failure=RuntimeError('own'), reader=running))
            running.cancel()
            return out

        out = asyncio.run(states())
        self.assertIsNone(out['cancelled'])
        self.assertIsNone(out['running'])
        self.assertIsInstance(out['failed'], ValueError)
        self.assertEqual(str(out['own']), 'own')


class InitializationFailureTest(unittest.TestCase):
    def test_campaign_cancellation_retains_failure(self):
        # CancelledError is a BaseException: it must still leave failure.json.
        script = Path(__file__).with_name('native_live.py')
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / 'upstream'
            source.mkdir()
            (source / 'model.py').write_text('# fake upstream fixture\n')
            pair = root / 'pair.json'
            pair.write_text('{"variants": []}')
            out = root / 'run'

            class Backend:
                metadata = {'fixture': True}
                torch = types.SimpleNamespace(cuda=types.SimpleNamespace(synchronize=lambda: None))

                def __init__(self, *args, **kwargs):
                    pass

                def session(self):
                    return types.SimpleNamespace(step=lambda text: [])

            def cancelled(coroutine):
                coroutine.close()
                raise asyncio.CancelledError()

            argv = [str(script), '--source', str(source), '--snapshot', str(root),
                    '--base', str(root), '--prepared-pair', str(pair), '--out', str(out)]
            with patch.dict(sys.modules, stub_modules(Backend)), patch.object(sys, 'argv', argv), \
                    patch.object(sys, 'path', list(sys.path)), patch('asyncio.run', cancelled):
                with self.assertRaises(asyncio.CancelledError):
                    runpy.run_path(str(script), run_name='__main__')
            failure = json.loads((out / 'failure.json').read_text())
            self.assertEqual(failure['stage'], 'campaign')
            self.assertTrue(failure['trials_started'])
            self.assertIn('CancelledError', failure['error'])

    def test_model_and_warmup_failures_retain_stage(self):
        script = Path(__file__).with_name('native_live.py')
        for stage in ('model-load', 'warmup'):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                source = root / 'upstream'
                source.mkdir()
                (source / 'model.py').write_text('# fake upstream fixture\n')
                pair = root / 'pair.json'
                pair.write_text('{"variants": []}')
                out = root / 'run'

                class Backend:
                    def __init__(self, *args, **kwargs):
                        if stage == 'model-load':
                            raise RuntimeError('injected model failure')
                        self.metadata = {'fixture': True}

                    def session(self):
                        raise RuntimeError('injected warmup failure')

                modules = {}
                for name, clsname, cls in (
                    ('duplexcascade_model', 'DuplexCascadeModel', Backend),
                    ('duplexcascade_speech', 'DuplexCascadeSpeech', object),
                    ('microturn_sidecar', 'SpeechContext', object),
                ):
                    module = types.ModuleType(name)
                    setattr(module, clsname, cls)
                    modules[name] = module
                argv = [str(script), '--source', str(source), '--snapshot', str(root),
                        '--base', str(root), '--prepared-pair', str(pair), '--out', str(out)]
                with patch.dict(sys.modules, modules), patch.object(sys, 'argv', argv), patch.object(sys, 'path', list(sys.path)):
                    with self.assertRaisesRegex(RuntimeError, 'injected'):
                        runpy.run_path(str(script), run_name='__main__')
                failure = json.loads((out / 'failure.json').read_text())
                manifest = json.loads((out / 'manifest.json').read_text())
                self.assertEqual(failure['stage'], stage)
                self.assertFalse(failure['trials_started'])
                self.assertIsNone(failure['capability_score'])
                self.assertEqual(manifest['terminal_failure'], failure)
                self.assertFalse((out / 'campaign.json').exists())
