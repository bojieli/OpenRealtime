import numpy as np
import pytest
from native_probe import question_timeline, RATE


def test_repeated_questions_preserve_silence_and_do_not_truncate_last_input():
    question = np.ones(RATE, dtype=np.float32)
    pcm, windows = question_timeline(question, 8.5, 1, 3)
    assert windows == [{'start_s': 1, 'end_s': 2}, {'start_s': 4, 'end_s': 5},
                       {'start_s': 7, 'end_s': 8}]
    assert np.count_nonzero(pcm) == 3 * RATE
    assert not pcm[2 * RATE:4 * RATE].any()
    _, windows = question_timeline(question, 7.5, 1, 3)
    assert len(windows) == 2


def test_overlapping_questions_and_incomplete_first_question_are_rejected():
    question = np.ones(RATE)
    with pytest.raises(ValueError):
        question_timeline(question, 10, 1, .5)
    with pytest.raises(ValueError):
        question_timeline(question, 1.5, 1)


def test_early_disconnect_does_not_claim_unsent_questions():
    from native_probe import delivered_windows
    windows = [{'start_s': 1, 'end_s': 2}, {'start_s': 4, 'end_s': 5}]
    assert delivered_windows(windows, .5) == []
    assert delivered_windows(windows, 1.5) == [
        {'start_s': 1, 'end_s': 2, 'sent_end_s': 1.5, 'complete': False}]
    assert delivered_windows(windows, 4) == [
        {'start_s': 1, 'end_s': 2, 'sent_end_s': 2, 'complete': True}]
