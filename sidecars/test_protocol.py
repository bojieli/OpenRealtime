"""Framing tests for the sidecar protocol library.

The Go side has its own conformance suite; this covers the Python library that
a non-Go model implementation actually depends on. The cases are the ones that
desynchronise a stream rather than merely fail a frame, because those are the
failures that look like a hung model.
"""

from __future__ import annotations

import io
import json

import pytest

from openrealtime_sidecar import protocol


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
