"""Framing for the OpenRealtime sidecar protocol.

Each message is one JSON header line terminated by a newline, optionally
followed by exactly ``payload_bytes`` of binary payload::

    {"type":"audio","payload_bytes":960}\\n<960 bytes of PCM16>

Audio is raw rather than base64 because a third more bytes and an encode pass
on every frame is a real cost on the hot path. Everything else is ordinary
JSON, so a sidecar can be debugged by reading the stream.
"""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field
from typing import Any, BinaryIO

# Version 1 remains accepted and is what an engine requests by default.
# Version 2 adds typed interaction-act handoff. Version 3 adds direct encoded
# images and tool-catalog updates without changing either earlier contract.
VERSION = 3
SUPPORTED_VERSIONS = (1, 2, 3)

MAX_HEADER_BYTES = 1 << 20
MAX_PAYLOAD_BYTES = 16 << 20


class MessageType:
    """Every frame name in the protocol."""

    # Engine to sidecar.
    HELLO = "hello"
    AUDIO = "audio"
    IMAGE = "image"
    TOOLS_UPDATE = "tools_update"
    TEXT = "text"
    COMMIT = "commit"
    RESPOND = "respond"
    INTERRUPT = "interrupt"
    INTERACTION_ACT = "interaction_act"
    TOOL_RESULT = "tool_result"
    BYE = "bye"

    # Sidecar to engine.
    READY = "ready"
    SPEECH_STARTED = "speech_started"
    SPEECH_STOPPED = "speech_stopped"
    TRANSCRIPT = "transcript"
    TEXT_DELTA = "text_delta"
    TEXT_DONE = "text_done"
    OUTPUT_AUDIO = "output_audio"
    TURN_DONE = "turn_done"
    TOOL_CALL = "tool_call"
    ERROR = "error"
    LOG = "log"


class Capability:
    """What a sidecar declares it can do.

    The engine reads these to decide what it must supply itself, which is what
    makes a partial implementation useful rather than broken.
    """

    NATIVE_VAD = "native_vad"
    TRANSCRIPT = "transcript"
    TEXT_INJECTION = "text_injection"
    TOOLS = "tools"
    BARGE_IN = "barge_in"
    FULL_DUPLEX = "full_duplex"
    NATIVE_INTERACTION = "native_interaction"
    INTERACTION_ACTS = "interaction_acts"
    VISUAL_INPUT = "visual_input"


@dataclass
class Message:
    """One protocol frame."""

    type: str
    header: dict[str, Any] = field(default_factory=dict)
    payload: bytes = b""

    def get(self, name: str, default: Any = None) -> Any:
        return self.header.get(name, default)

    @property
    def text(self) -> str:
        return str(self.header.get("text", ""))

    @property
    def final(self) -> bool:
        return bool(self.header.get("final", False))


def read_message(stream: BinaryIO) -> Message | None:
    """Read one frame, or None at the end of the stream."""
    line = stream.readline()
    if not line:
        return None
    if len(line) > MAX_HEADER_BYTES:
        raise ValueError("sidecar header exceeds the frame limit")
    header = json.loads(line)
    payload_bytes = int(header.get("payload_bytes", 0))
    if payload_bytes < 0 or payload_bytes > MAX_PAYLOAD_BYTES:
        raise ValueError(f"payload length {payload_bytes} is out of range")
    payload = b""
    if payload_bytes:
        payload = stream.read(payload_bytes)
        if len(payload) != payload_bytes:
            # A payload that does not follow its declared length would
            # desynchronise the stream permanently, so it fails here rather
            # than three frames later.
            raise ValueError(
                f"payload declared {payload_bytes} bytes and carried {len(payload)}"
            )
    return Message(type=str(header.get("type", "")), header=header, payload=payload)


def write_message(
    stream: BinaryIO, message_type: str, payload: bytes = b"", **header: Any
) -> None:
    """Write one frame and flush it.

    Flushing every frame is deliberate. A sidecar that buffers is a sidecar
    that appears to hang, and the whole point of streaming audio is that it
    arrives while it is still useful.
    """
    header = {name: value for name, value in header.items() if value is not None}
    header["type"] = message_type
    if payload:
        header["payload_bytes"] = len(payload)
    stream.write(json.dumps(header).encode("utf-8"))
    stream.write(b"\n")
    if payload:
        stream.write(payload)
    stream.flush()


def log(message: str) -> None:
    """Write a diagnostic line to stderr.

    The engine forwards it. A traceback that reaches the operator is the
    difference between "the model failed" and knowing why.
    """
    print(message, file=sys.stderr, flush=True)
