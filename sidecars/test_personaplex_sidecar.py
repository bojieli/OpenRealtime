"""Session isolation regressions without loading model weights."""
import io
import sys
import threading
import types
import unittest
from unittest.mock import patch

from personaplex_sidecar import PersonaPlexModel, PersonaPlexSidecar


class SessionTests(unittest.TestCase):
    def test_failed_prompt_does_not_lock_out_next_session(self):
        class Model:
            lock = threading.Lock()
            def reset(self, instructions):
                raise ValueError('bad prompt')
        model = Model()
        sidecar = PersonaPlexSidecar(io.BytesIO(), io.BytesIO(), repository='test',
                                    mock=False, device='cpu', shared=model)
        sidecar.input_rate, sidecar.instructions = 24000, ''
        with self.assertRaisesRegex(ValueError, 'bad prompt'):
            sidecar.configure(None)
        self.assertFalse(sidecar._holds_model)
        self.assertTrue(model.lock.acquire(blocking=False))
        model.lock.release()

    def test_empty_prompt_is_iterable_and_resets_previous_session(self):
        import contextlib
        events = []
        class Stream:
            def __init__(self, name): self.name = name
            def reset_streaming(self): events.append(self.name)
        class Generator(Stream):
            text_prompt_tokens = [99]
            def step_system_prompts(self, mimi):
                self.seen = list(self.text_prompt_tokens)
                events.append('prompts')
        model = PersonaPlexModel.__new__(PersonaPlexModel)
        model.torch = types.SimpleNamespace(no_grad=contextlib.nullcontext)
        model.mimi, model.other_mimi = Stream('mimi'), Stream('other')
        model.generator = Generator('generator')
        model.tokenizer = types.SimpleNamespace(encode=lambda text: [len(text)])
        offline = types.ModuleType('moshi.offline')
        offline.wrap_with_system_tags = lambda text: '<system>'+text+'<system>'
        with patch.dict(sys.modules, {'moshi.offline': offline}):
            model.reset('')
        self.assertEqual(model.generator.seen, [])
        self.assertEqual(events, ['mimi', 'other', 'generator', 'prompts', 'mimi'])


if __name__ == '__main__':
    unittest.main()
