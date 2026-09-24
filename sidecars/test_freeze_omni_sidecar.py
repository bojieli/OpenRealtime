"""Ordering of local output cutoff and an in-flight transport write."""
import threading
import unittest
from freeze_omni_sidecar import OutputPacer


class PacerTests(unittest.TestCase):
    def test_drop_returns_only_after_admitted_write_finishes(self):
        writing, release, dropping, dropped = (threading.Event() for _ in range(4))
        events = []
        def send(kind, payload, **header):
            writing.set()
            if not release.wait(2):
                raise RuntimeError('test writer timed out')
            events.append(kind)
        pacer = OutputPacer(send, 24000, frame_ms=40, lead_ms=0)
        pacer.put('output_audio', bytes(24000 * 2), {})
        def drop():
            dropping.set()
            pacer.drop_audio()
            events.append('cutoff')
            dropped.set()
        worker = threading.Thread(target=drop)
        try:
            self.assertTrue(writing.wait(2))
            worker.start()
            self.assertTrue(dropping.wait(2))
            premature = dropped.wait(.05)
        finally:
            release.set()
            if worker.ident is not None:
                worker.join(2)
            pacer.close(drain_timeout=0)
        self.assertFalse(premature, 'cutoff returned while admitted write was pending')
        self.assertEqual(events[-1], 'cutoff')
        self.assertGreater(pacer.dropped_samples, 0)


class ShutdownTests(unittest.TestCase):
    def test_session_state_outlives_slow_model_workers(self):
        import io
        from types import SimpleNamespace
        from freeze_omni_sidecar import FreezeOmniSidecar

        # Accelerate only the old timed-join path: a timed-out worker remains
        # live just as it would after a slow GPU call exceeds 5 or 10 seconds.
        class SlowWorker(threading.Thread):
            def join(self, timeout=None):
                return super().join(None if timeout is None else .01)

        for kind in ('_listener', '_generation'):
            with self.subTest(worker=kind):
                release, entered, closed = (threading.Event() for _ in range(3))
                cleared = []
                engine = SimpleNamespace(torch=SimpleNamespace(cuda=SimpleNamespace(
                    empty_cache=lambda: cleared.append(True))))
                sidecar = FreezeOmniSidecar(io.BytesIO(), io.BytesIO(), engine=engine, pace=False)
                state = {'still_owned': True}
                sidecar.generate_outputs = state
                sidecar.system_role = state
                def work():
                    entered.set()
                    release.wait(2)
                worker = SlowWorker(target=work)
                setattr(sidecar, kind, worker)
                worker.start()
                def close():
                    sidecar.on_close()
                    closed.set()
                closer = threading.Thread(target=close)
                try:
                    self.assertTrue(entered.wait(1))
                    closer.start()
                    returned_early = closed.wait(.05)
                    retained_state = sidecar.generate_outputs is state
                    cleared_early = bool(cleared)
                finally:
                    release.set()
                    closer.join(2)
                    worker.join(2)
                self.assertFalse(returned_early, 'session released while model worker is live')
                self.assertTrue(retained_state)
                self.assertFalse(cleared_early)
                self.assertTrue(closed.is_set())
                self.assertIsNone(sidecar.generate_outputs)
                self.assertEqual(cleared, [True])


class CacheDiagnosticsTests(unittest.TestCase):
    def test_views_share_storage_but_snapshot_does_not(self):
        import copy
        import torch
        from freeze_omni_sidecar import cache_storage_summary

        backing = torch.zeros(2, 3, 7, 4)
        cache = ((backing[0], backing[1]),)
        live = cache_storage_summary(torch, cache)
        snapshot = cache_storage_summary(torch, copy.deepcopy(cache))
        self.assertEqual(live['storage_bytes'], backing.numel() * backing.element_size())
        self.assertEqual(live['storage_count'], 1)
        self.assertEqual(live['tensor_shapes'], [[3, 7, 4], [3, 7, 4]])
        self.assertEqual(snapshot['storage_bytes'], live['storage_bytes'])
        self.assertFalse(set(live['storages']) & set(snapshot['storages']))
        self.assertEqual(cache_storage_summary(torch, None)['storage_bytes'], 0)
        with self.assertRaisesRegex(TypeError, 'unsupported KV cache'):
            cache_storage_summary(torch, object())


if __name__ == '__main__':
    unittest.main()


class AfterAnswerTests(unittest.TestCase):
    def sidecar(self, tokens, *, limit, release=True):
        import types
        from freeze_omni_sidecar import FreezeOmniSidecar
        s = FreezeOmniSidecar.__new__(FreezeOmniSidecar)
        s.mock, s._kv_lock, s.session_id = False, threading.Lock(), 'test'
        s.max_context_tokens, s.release_allocator_cache = limit, release
        emptied, errors, events = [], [], []
        cuda = types.SimpleNamespace(is_available=lambda: True, empty_cache=lambda: emptied.append(1))
        s.engine = types.SimpleNamespace(torch=types.SimpleNamespace(cuda=cuda))
        key = types.SimpleNamespace(size=lambda dim: tokens if dim == 2 else 1)
        s.generate_outputs = {'past_key_values': ((key, key),)}
        s.control = types.SimpleNamespace(event=lambda *a, **k: events.append(a[1]))
        s.error = lambda text, **kw: errors.append(kw)
        return s, emptied, errors, events

    def test_cache_released_after_each_answer_unless_kept(self):
        for release, expected in ((True, [1]), (False, [])):
            s, emptied, errors, _ = self.sidecar(1000, limit=28000, release=release)
            s._after_answer()
            self.assertEqual(emptied, expected)
            self.assertEqual(errors, [])

    def test_context_limit_ends_the_session_with_a_fatal_error(self):
        s, _, errors, events = self.sidecar(28000, limit=28000)
        s._after_answer()
        self.assertEqual(errors, [{'code': 'context_limit', 'fatal': True}])
        self.assertEqual(events, ['context_limit'])
        s, _, errors, _ = self.sidecar(10**6, limit=0)
        s._after_answer()
        self.assertEqual(errors, [], 'a zero limit must never end the session')
