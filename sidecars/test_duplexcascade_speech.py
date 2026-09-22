import unittest
from duplexcascade_speech import DuplexCascadeSpeech


class Context:
    def __init__(self): self.calls = []
    async def open(self, callback): self.callback = callback; self.calls.append('open')
    async def append(self, text): self.calls.append(('append', text))
    async def flush(self): self.calls.append('flush')
    async def cancel(self): self.calls.append('cancel'); self.callback(b'stale during cancel')


class SpeechTests(unittest.IsolatedAsyncioTestCase):
    async def test_continuation_flush_and_cancel_preserve_native_order(self):
        contexts, audio, text, cancellations = [], [], [], []
        def factory():
            context = Context(); contexts.append(context); return context
        bridge = DuplexCascadeSpeech(factory, audio.append, text.append, lambda: cancellations.append(True))
        await bridge.apply([('control','<|user finish talking|>'), ('text','Paris.'), ('control','<|im_end|>')])
        await bridge.apply([('text',' More.'), ('control','<|user is thinking|>'), ('control','<|user is thinking|>')])
        self.assertEqual(len(contexts), 1)
        self.assertEqual(contexts[0].calls, ['open', ('append','Paris'), ('append',' More'), 'flush'])
        await bridge.apply([('control','<|user backchannel|>')])
        contexts[0].callback(b'valid')
        await bridge.apply([('control','<|user interruption|>')])
        contexts[0].callback(b'stale after cancel')
        self.assertEqual(audio, [b'valid'])
        self.assertEqual(cancellations, [True])
        await bridge.apply([('control','<|user finish talking|>'), ('text','New answer.')])
        self.assertEqual(len(contexts), 2)
        self.assertEqual(text, ['Paris.', ' More.', 'New answer.'])


if __name__ == '__main__': unittest.main()
