"""Persistent synthesis driven by released DuplexCascade control tokens.

The caller owns playout pacing and must discard queued audio in on_cancel.
This layer suppresses later callbacks from cancelled synthesis contexts.
Thinking flushes without ending the context; ChatML ends only the micro-turn.
"""


class DuplexCascadeSpeech:
    def __init__(self, factory, on_audio, on_text, on_cancel):
        self.factory = factory
        self.on_audio, self.on_text, self.on_cancel = on_audio, on_text, on_cancel
        self.context = None
        self.epoch = 0
        self.speaking = False
        self.thinking = False

    async def _open(self):
        if self.context is not None:
            return
        context = self.factory()
        epoch = self.epoch
        try:
            await context.open(lambda pcm: self.on_audio(pcm) if epoch == self.epoch else None)
        except BaseException:
            await context.close()
            raise
        self.context = context

    def check(self):
        if self.context is None:
            return
        failure = getattr(self.context, 'failure', None)
        if failure is not None:
            raise failure
        reader = getattr(self.context, 'reader', None)
        if reader is not None and reader.done():
            reader.result()
            raise RuntimeError('persistent TTS context ended unexpectedly')

    async def apply(self, events):
        self.check()
        for kind, value in events:
            if kind == 'text':
                self.on_text(value)
                # Released server strips periods only on its synthesis path.
                text = value.replace('.', '')
                if text:
                    await self._open()
                    await self.context.append(text)
            elif kind == 'control':
                if value == '<|user finish talking|>':
                    self.speaking, self.thinking = True, False
                elif value == '<|user is thinking|>':
                    if not self.thinking and self.context is not None:
                        await self.context.flush()
                    self.thinking = True
                elif value in ('<|user interruption|>', '<|user is talking|>'):
                    if self.speaking:
                        await self.cancel()
                # user backchannel preserves synthesis; im_end is not text.end.
            else:
                raise ValueError(f'unknown DuplexCascade event kind: {kind}')

    async def cancel(self):
        self.epoch += 1
        self.speaking = False
        self.on_cancel()
        context, self.context = self.context, None
        if context is not None:
            await context.cancel()

    async def close(self):
        try:
            self.check()
        finally:
            await self.cancel()
