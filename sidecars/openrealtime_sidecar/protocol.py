"""Framing for the OpenRealtime sidecar protocol.

Each message is one JSON header line terminated by a newline, optionally
followed by exactly ``payload_bytes`` of binary payload::

    {"type":"audio","payload_bytes":960}\\n<960 bytes of PCM16>

Legacy media frames and protocol-v4 typed element frames may use the binary
lane.  In v4 the selected port's explicit wire format, not an audio-specific
message name, owns the JSON/binary lanes and their bounds.  Headers remain
ordinary JSON so the boundary is inspectable without decoding provider data.
"""

from __future__ import annotations

import json
import sys
from dataclasses import dataclass, field
from typing import Any, BinaryIO

# Version 1 remains accepted and is what an engine requests by default.
# Version 2 adds typed interaction-act handoff. Version 3 adds direct encoded
# images and tool-catalog updates. Version 4 carries descriptor-checked element
# ports and is deliberately selected by the engine rather than inferred.
VERSION = 4
SUPPORTED_VERSIONS = (1, 2, 3, 4)

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
    ELEMENT_FRAME = "element_frame"

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
    # Exact header bytes are retained for protocol-v4 attestations and byte
    # limits. Parsing JSON loses harmless-but-byte-significant number spelling
    # and whitespace, so reconstructing a digest from Python values would not
    # prove what actually crossed the process boundary.
    raw_header: bytes = b""

    def get(self, name: str, default: Any = None) -> Any:
        return self.header.get(name, default)

    @property
    def text(self) -> str:
        return str(self.header.get("text", ""))

    @property
    def final(self) -> bool:
        return bool(self.header.get("final", False))


def _reject_duplicate_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for name, value in pairs:
        if name in result:
            raise ValueError(f"sidecar header repeats field {name!r}")
        result[name] = value
    return result


def _reject_non_finite_number(value: str) -> Any:
    raise ValueError(f"sidecar header contains non-finite number {value}")


def _read_exact(stream: BinaryIO, length: int) -> bytes:
    chunks: list[bytes] = []
    remaining = length
    while remaining:
        chunk = stream.read(remaining)
        if not chunk:
            carried = length - remaining
            raise ValueError(f"payload declared {length} bytes and carried {carried}")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def _write_all(stream: BinaryIO, source: bytes) -> None:
    remaining = memoryview(source)
    while remaining:
        written = stream.write(remaining)
        if written is None or written <= 0:
            raise OSError("short write while emitting a sidecar frame")
        remaining = remaining[written:]


def read_message(stream: BinaryIO) -> Message | None:
    """Read one frame, or None at the end of the stream."""
    # Bound the read itself. Checking only after an unbounded readline lets a
    # malicious peer allocate arbitrary memory before the limit is applied.
    line = stream.readline(MAX_HEADER_BYTES + 1)
    if not line:
        return None
    if len(line) > MAX_HEADER_BYTES:
        raise ValueError("sidecar header exceeds the frame limit")
    if not line.endswith(b"\n"):
        raise ValueError("sidecar header is not newline-terminated")
    try:
        header = json.loads(
            line,
            object_pairs_hook=_reject_duplicate_fields,
            parse_constant=_reject_non_finite_number,
        )
    except (TypeError, json.JSONDecodeError) as failure:
        raise ValueError(f"invalid sidecar header: {failure}") from failure
    if not isinstance(header, dict):
        raise ValueError("sidecar header must be one JSON object")
    message_type = header.get("type", "")
    if not isinstance(message_type, str) or not message_type.strip() or message_type != message_type.strip():
        raise ValueError("a sidecar message requires a canonical type")
    declared = header.get("payload_bytes", 0)
    if isinstance(declared, bool) or not isinstance(declared, int):
        raise ValueError("payload length must be an integer")
    payload_bytes = declared
    if payload_bytes < 0 or payload_bytes > MAX_PAYLOAD_BYTES:
        raise ValueError(f"payload length {payload_bytes} is out of range")
    payload = b""
    if payload_bytes:
        # A payload that does not follow its declared length would
        # desynchronise the stream permanently, so it fails here rather than
        # three frames later. BinaryIO.read is allowed to return short reads.
        payload = _read_exact(stream, payload_bytes)
    return Message(
        type=message_type, header=header, payload=payload, raw_header=line[:-1],
    )


def write_message(
    stream: BinaryIO, message_type: str, payload: bytes = b"", **header: Any
) -> None:
    """Write one frame and flush it.

    Flushing every frame is deliberate. A sidecar that buffers is a sidecar
    that appears to hang, and the whole point of streaming audio is that it
    arrives while it is still useful.
    """
    if not isinstance(message_type, str) or not message_type.strip() or message_type != message_type.strip():
        raise ValueError("a sidecar message requires a canonical type")
    if not isinstance(payload, bytes):
        raise TypeError("sidecar payload must be bytes")
    if len(payload) > MAX_PAYLOAD_BYTES:
        raise ValueError(f"sidecar payload exceeds the {MAX_PAYLOAD_BYTES}-byte frame limit")
    header = {name: value for name, value in header.items() if value is not None}
    header["type"] = message_type
    header.pop("payload_bytes", None)
    if payload:
        header["payload_bytes"] = len(payload)
    encoded = json.dumps(
        header, separators=(",", ":"), ensure_ascii=False, allow_nan=False,
    ).encode("utf-8")
    if len(encoded) + 1 > MAX_HEADER_BYTES:
        raise ValueError("sidecar header exceeds the frame limit")
    _write_all(stream, encoded + b"\n")
    if payload:
        _write_all(stream, payload)
    stream.flush()


def log(message: str) -> None:
    """Write a diagnostic line to stderr.

    The engine forwards it. A traceback that reaches the operator is the
    difference between "the model failed" and knowing why.
    """
    print(message, file=sys.stderr, flush=True)
