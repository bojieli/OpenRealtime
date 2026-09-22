"""Exercise the actual row scheduler without model weights or CUDA."""
import threading
from collections import deque
from types import SimpleNamespace

from tts_kyutai import KyutaiTTS, Row, OUTPUT_BUFFER_FRAMES


class Selected(BaseException):
    pass


def setup_rows():
    model = KyutaiTTS.__new__(KyutaiTTS)
    model.wake = threading.Condition()
    model.pending_open = deque()
    model.stats = {'cancelled':0}
    model.delay_steps = 16
    model.final_padding = 2
    model.max_steps = 1000
    model.lookahead = 2
    model._admit = lambda row: None
    rows = [Row(i, 'default') for i in range(2)]
    for row in rows:
        row.state = SimpleNamespace(entries=[1,2,3], end_step=None)
    model.rows = rows[:]
    for _ in range(OUTPUT_BUFFER_FRAMES):
        rows[0].out.put(b'pcm')
    chosen = []
    def step(ready):
        chosen.extend(row.index for row in ready)
        raise Selected()
    model._step = step
    return model, rows, chosen


def select(model):
    try:
        model._loop()
    except Selected:
        return
    raise AssertionError('scheduler did not select a ready row')


def test_full_ending_row_pauses_without_finishing_or_blocking_other_row():
    model, rows, chosen = setup_rows()
    rows[0].ending = True
    select(model)
    assert chosen == [1]
    assert model.rows[0] is rows[0] and not rows[0].done
    assert rows[0].out.qsize() == OUTPUT_BUFFER_FRAMES
    rows[0].out.get_nowait()
    chosen.clear()
    select(model)
    assert chosen == [0,1]


def test_cancel_full_row_does_not_wait_for_output_capacity():
    model, rows, chosen = setup_rows()
    rows[0].cancel.set()
    select(model)
    assert chosen == [1]
    assert model.rows[0] is None and rows[0].done
    assert model.stats['cancelled'] == 1
    assert rows[0].out.qsize() == OUTPUT_BUFFER_FRAMES+1  # terminal sentinel, no new PCM
