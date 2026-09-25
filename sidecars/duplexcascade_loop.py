"""Causal micro-turn scheduler for the released DuplexCascade session backend."""
import asyncio
import time


class DuplexCascadeLoop:
    def __init__(self, session, speech, words, *, tick_seconds=.5, trace=None, separator=" ",
                 backlog=None, max_backlog_seconds=None):
        if tick_seconds <= 0:
            raise ValueError('tick duration must be positive')
        if max_backlog_seconds is not None and (backlog is None or max_backlog_seconds <= 0):
            raise ValueError('a backlog bound needs a positive limit and a backlog reader')
        # Off by default: upstream generates every tick however much speech is
        # still unplayed. The bounded variant skips silent ticks while more
        # than max_backlog_seconds of audio awaits playout; ticks carrying new
        # words always run so the model hears the user.
        self.backlog, self.max_backlog_seconds = backlog, max_backlog_seconds
        self.session, self.speech, self.words = session, speech, words
        self.tick_seconds, self.trace = tick_seconds, trace
        self.separator = separator
        self.closed = False
        self.inference = None

    async def run(self):
        # Upstream starts its clock at the first recognized text, then admits
        # new words every tick, including silence ticks after the answer.
        try:
            first = await self.words.get()
            pending = [first]
            deadline = time.monotonic() + self.tick_seconds
            index = 0
            while not self.closed:
                await asyncio.sleep(max(0, deadline-time.monotonic()))
                if self.closed:
                    break
                began = time.monotonic()
                while not self.words.empty():
                    pending.append(self.words.get_nowait())
                if not pending and self.max_backlog_seconds is not None:
                    queued = self.backlog()
                    if queued > self.max_backlog_seconds:
                        if self.trace:
                            self.trace({'tick': index, 'skipped': 'backlog', 'queued_audio_s': queued})
                        index += 1
                        deadline += self.tick_seconds
                        continue
                admitted, pending = pending, []
                chunk = self.separator.join(word.text for word in admitted)
                # Inference must not block ASR's socket reader. Shielding keeps
                # shutdown from abandoning a worker still updating KV/history.
                self.inference = asyncio.create_task(asyncio.to_thread(self.session.step, chunk))
                tokens = await asyncio.shield(self.inference)
                if self.closed:
                    break
                events = list(self.session.events(tokens))
                await self.speech.apply(events)
                finished = time.monotonic()
                if self.trace:
                    self.trace({'tick': index, 'input': chunk, 'events': events,
                                'compute_and_dispatch_ms': (finished-began)*1000,
                                'admission_lag_ms': [(began-word.arrived)*1000 for word in admitted],
                                'deadline_missed': finished-began > self.tick_seconds})
                index += 1
                # No catch-up burst after an overrun; retain actual timing in
                # the trace instead of pretending the model met its clock.
                deadline = max(deadline+self.tick_seconds, finished)
        finally:
            self.closed = True
            try:
                if self.inference is not None:
                    await asyncio.shield(self.inference)
            finally:
                await self.speech.close()
