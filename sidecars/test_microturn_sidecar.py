"""Unit tests for the micro-turn cascade sidecar's evidence handling.

The network-facing parts (recogniser sockets, the language model, the
synthesiser) are exercised end to end by the conformance command and the
benchmarks; these pin the pieces whose mistakes were silent: a final word
held back until the next utterance, words said before the assistant began
being mistaken for an interruption, and sound that is not speech.
"""

from __future__ import annotations

import asyncio
import math
import time

import numpy as np

import microturn_sidecar as microturn


def _drain(queue: "asyncio.Queue[microturn.Word]") -> list[str]:
    words = []
    while not queue.empty():
        words.append(queue.get_nowait().text)
    return words


def test_the_last_word_is_committed_without_waiting_for_the_next_utterance():
    async def scenario():
        words: asyncio.Queue = asyncio.Queue()
        recognizer = microturn.RealtimeRecognizer("ws://unused", "m", words, delay_s=0.48)
        recognizer.idle_flush_s = 0.05
        for piece in [" How", " should", " I", " water", " plants"]:
            recognizer._emit(piece)
        assert _drain(words) == ["How", "should", "I", "water"]
        # No further delta arrives: the utterance is over.
        await asyncio.sleep(0.12)
        assert _drain(words) == ["plants"]
        # Sentence punctuation commits at once.
        recognizer._emit(" Why")
        recognizer._emit("?")
        assert _drain(words) == ["Why?"]

    asyncio.run(scenario())


def test_mandarin_pieces_are_committed_as_they_arrive():
    async def scenario():
        words: asyncio.Queue = asyncio.Queue()
        recognizer = microturn.RealtimeRecognizer("ws://unused", "m", words, delay_s=0.48)
        recognizer._emit("你好")
        recognizer._emit("世界")
        assert _drain(words) == ["你好", "世界"]

    asyncio.run(scenario())


def test_evidence_separates_words_said_before_and_while_speaking():
    now = 100.0
    began = 98.0
    evidence = microturn.Evidence(
        now=now, ticks=[], speaking=True, began_speaking=began, answering="what are the latest trends",
        said="Phones are getting",
        pending=[microturn.Word("in", now - 0.2, began - 0.5), microturn.Word("smartphones?", now, began - 0.2),
                 microturn.Word("wait", now, began + 1.5)],
        sounding=0.4, quiet=0.0)
    text = evidence.render()
    assert 'said BEFORE it began, recognised late: "in smartphones?"' in text
    assert 'said WHILE it has been speaking: "wait"' in text
    assert "making sound now" in text
    assert evidence.choices() == [microturn.CONTINUE, microturn.STOP]


def test_backchannels_are_offered_only_when_enabled():
    evidence = microturn.Evidence(now=1.0, ticks=[], pending=[], speaking=False)
    assert evidence.choices() == [microturn.WAIT, microturn.RESPOND]
    evidence.backchannels = True
    assert microturn.BACKCHANNEL in evidence.choices()


def test_activity_reports_sound_and_quiet():
    activity = microturn.Activity(rate=16_000, hangover_ms=100)
    quiet = np.zeros(1_600, dtype=np.float32)
    assert activity.push(quiet) is False
    sounding, since = activity.state(time.monotonic())
    assert sounding == 0.0 and math.isinf(since)
    tone = (0.2 * np.sin(np.arange(3_200) / 16_000 * 2 * math.pi * 200)).astype(np.float32)
    assert activity.push(tone) is True
    sounding, since = activity.state(time.monotonic())
    assert since < 0.1
    time.sleep(0.15)
    assert activity.state(time.monotonic())[0] == 0.0


def test_a_quiet_period_is_reported_once():
    activity = microturn.Activity(rate=16_000, hangover_ms=100)
    tone = (0.2 * np.sin(np.arange(3_200) / 16_000 * 2 * math.pi * 200)).astype(np.float32)
    activity.push(tone)
    assert activity.settled(time.monotonic()) == 0.0  # still within the hangover
    time.sleep(0.15)
    first = activity.settled(time.monotonic())
    assert first > 0
    # The same quiet period keeps reporting the same instant, so a caller that
    # acts on a change acts once.
    assert activity.settled(time.monotonic()) == first
    activity.push(tone)
    time.sleep(0.15)
    assert activity.settled(time.monotonic()) > first
