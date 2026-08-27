"""Framing tests for the sidecar protocol library.

The Go side has its own conformance suite; this covers the Python library that
a non-Go model implementation actually depends on. The cases are the ones that
desynchronise a stream rather than merely fail a frame, because those are the
failures that look like a hung model.
"""

from __future__ import annotations

import io
import json
import time

import pytest

from openrealtime_sidecar import protocol
from openrealtime_sidecar.sidecar import Sidecar


def test_frame_round_trips_with_payload() -> None:
    stream = io.BytesIO()
    audio = bytes(range(256)) * 4
    protocol.write_message(stream, protocol.MessageType.OUTPUT_AUDIO, audio, sample_rate=24000)

    stream.seek(0)
    message = protocol.read_message(stream)
    assert message is not None
    assert message.type == protocol.MessageType.OUTPUT_AUDIO
    assert message.payload == audio
    assert message.get("sample_rate") == 24000
    assert protocol.read_message(stream) is None


def test_frames_without_payload_do_not_declare_one() -> None:
    stream = io.BytesIO()
    protocol.write_message(stream, protocol.MessageType.TURN_DONE)
    header = json.loads(stream.getvalue().split(b"\n")[0])
    assert "payload_bytes" not in header


def test_consecutive_frames_stay_aligned() -> None:
    """A binary payload must not be mistaken for the next header line.

    Payloads contain newlines as a matter of course - PCM16 silence is full of
    zero bytes and audio is full of everything. Reading by declared length
    rather than by line is what keeps the stream aligned.
    """
    stream = io.BytesIO()
    payload = b"\n" * 64 + b'{"type":"not-a-frame"}\n'
    protocol.write_message(stream, protocol.MessageType.AUDIO, payload)
    protocol.write_message(stream, protocol.MessageType.TEXT_DELTA, text="hello")

    stream.seek(0)
    first = protocol.read_message(stream)
    second = protocol.read_message(stream)
    assert first is not None and first.payload == payload
    assert second is not None and second.type == protocol.MessageType.TEXT_DELTA
    assert second.text == "hello"


def test_truncated_payload_fails_at_the_frame_that_lied() -> None:
    stream = io.BytesIO(b'{"type":"audio","payload_bytes":100}\n' + b"\x00" * 10)
    with pytest.raises(ValueError, match="declared 100 bytes"):
        protocol.read_message(stream)


def test_absurd_payload_length_is_rejected_before_allocation() -> None:
    oversized = protocol.MAX_PAYLOAD_BYTES + 1
    stream = io.BytesIO(f'{{"type":"audio","payload_bytes":{oversized}}}\n'.encode())
    with pytest.raises(ValueError, match="out of range"):
        protocol.read_message(stream)


def test_none_valued_header_fields_are_omitted() -> None:
    """An absent field and a null field mean different things to the engine."""
    stream = io.BytesIO()
    protocol.write_message(stream, protocol.MessageType.TRANSCRIPT, text="hi", item_id=None)
    header = json.loads(stream.getvalue().split(b"\n")[0])
    assert header == {"type": "transcript", "text": "hi"}


class _ActSidecar(Sidecar):
    def __init__(self) -> None:
        super().__init__(io.BytesIO(), io.BytesIO())
        self.responses = 0

    def on_respond(self) -> None:
        self.responses += 1


@pytest.mark.parametrize("act", ["answer", "interrupt", "speak-through"])
def test_speaking_interaction_acts_delegate_content_to_the_model(act: str) -> None:
    sidecar = _ActSidecar()
    message = protocol.Message(protocol.MessageType.INTERACTION_ACT, {"act": act})
    assert sidecar.on_interaction_act(message)
    assert sidecar.responses == 1


@pytest.mark.parametrize("act", ["listen", "keep-speaking", "act-silently"])
def test_non_speaking_interaction_acts_do_not_generate_content(act: str) -> None:
    sidecar = _ActSidecar()
    message = protocol.Message(protocol.MessageType.INTERACTION_ACT, {"act": act})
    assert not sidecar.on_interaction_act(message)
    assert sidecar.responses == 0


def test_stop_speaking_sets_the_interrupt_signal() -> None:
    sidecar = _ActSidecar()
    message = protocol.Message(
        protocol.MessageType.INTERACTION_ACT, {"act": "stop-speaking"}
    )
    assert not sidecar.on_interaction_act(message)
    assert sidecar.interrupted()


def test_interrupt_act_signals_generation_before_waiting_for_the_worker() -> None:
    input_stream = io.BytesIO()
    protocol.write_message(
        input_stream,
        protocol.MessageType.INTERACTION_ACT,
        act="interrupt",
        floor="take",
        policy="test-policy",
        evidence_ref="revision:1",
        deadline_ms=int(time.time() * 1000) + 1000,
        confidence=0.8,
    )
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)

    sidecar = Sidecar(input_stream, io.BytesIO())
    sidecar._read_loop()

    assert sidecar.interrupted()
    queued = sidecar._work.get_nowait()
    assert queued is not None and queued.get("act") == "interrupt"
