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

    async def test_tts_error_is_not_reported_as_successful_audio_end(self):
        context = Context()
        bridge = DuplexCascadeSpeech(lambda:context, lambda pcm:None, lambda text:None, lambda:None)
        await bridge.apply([('text','Hello')])
        context.failure = RuntimeError('synthesis failed')
        with self.assertRaisesRegex(RuntimeError, 'synthesis failed'):
            await bridge.apply([('control','<|im_end|>')])
        with self.assertRaisesRegex(RuntimeError, 'synthesis failed'):
            await bridge.close()
        self.assertIn('cancel', context.calls)

    async def test_failed_open_closes_socket(self):
        class Failed(Context):
            async def open(self, callback): raise RuntimeError('bad handshake')
            async def close(self): self.calls.append('close')
        context = Failed()
        bridge = DuplexCascadeSpeech(lambda:context, lambda pcm:None, lambda text:None, lambda:None)
        with self.assertRaisesRegex(RuntimeError, 'bad handshake'):
            await bridge.apply([('text','Hello')])
        self.assertEqual(context.calls, ['close'])


if __name__ == '__main__': unittest.main()
