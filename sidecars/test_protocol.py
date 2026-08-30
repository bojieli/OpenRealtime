"""Framing tests for the sidecar protocol library.

The Go side has its own conformance suite; this covers the Python library that
a non-Go model implementation actually depends on. The cases are the ones that
desynchronise a stream rather than merely fail a frame, because those are the
failures that look like a hung model.
"""

from __future__ import annotations

import copy
import hashlib
import io
import json
import threading
import time

import pytest

from openrealtime_sidecar import protocol
from openrealtime_sidecar.element import (
    ConformanceElementSidecar,
    ElementSidecar,
    conformance_descriptor,
)
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


def test_writer_rejects_payload_over_the_shared_frame_limit_before_output() -> None:
    stream = io.BytesIO()
    with pytest.raises(ValueError, match="frame limit"):
        protocol.write_message(
            stream,
            protocol.MessageType.AUDIO,
            b"x" * (protocol.MAX_PAYLOAD_BYTES + 1),
        )
    assert stream.getvalue() == b""


def test_reader_rejects_duplicate_fields_and_unterminated_headers() -> None:
    duplicate = io.BytesIO(b'{"type":"log","type":"error"}\n')
    with pytest.raises(ValueError, match="repeats field"):
        protocol.read_message(duplicate)

    unterminated = io.BytesIO(b'{"type":"log"}')
    with pytest.raises(ValueError, match="newline-terminated"):
        protocol.read_message(unterminated)

    non_finite = io.BytesIO(b'{"type":"log","confidence":NaN}\n')
    with pytest.raises(ValueError, match="non-finite"):
        protocol.read_message(non_finite)


class _ShortIO:
    def __init__(self, source: bytes = b"") -> None:
        self.buffer = io.BytesIO(source)

    def readline(self, limit: int = -1) -> bytes:
        return self.buffer.readline(limit)

    def read(self, length: int = -1) -> bytes:
        return self.buffer.read(min(length, 3))

    def write(self, value) -> int:
        return self.buffer.write(value[:3])

    def flush(self) -> None:
        pass


def test_framing_handles_permitted_short_reads_and_writes() -> None:
    output = _ShortIO()
    payload = b"short-io-payload"
    protocol.write_message(output, protocol.MessageType.AUDIO, payload)
    encoded = output.buffer.getvalue()
    decoded = protocol.read_message(_ShortIO(encoded))
    assert decoded is not None and decoded.payload == payload


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


def _v4_conformance_hello() -> dict:
    descriptor = conformance_descriptor()
    wire_format = {"payload_mode": "json", "max_json_bytes": 512 << 10}
    return {
        "version": 4,
        "element_descriptor": descriptor,
        "element_config": {"mode": "conformance"},
        "selected_ports": [{
            "name": port["name"], "direction": port["direction"],
            "type": port["type"], "formats": [dict(wire_format)],
        } for port in descriptor["ports"]],
    }


def test_python_sdk_serves_the_exact_descriptor_backed_v4_probe() -> None:
    input_stream = io.BytesIO()
    hello = _v4_conformance_hello()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    protocol.write_message(
        input_stream,
        protocol.MessageType.ELEMENT_FRAME,
        port="request",
        envelope={
            "type": hello["selected_ports"][0]["type"],
            "item_id": "request-1",
            "session_id": "session-1",
            "run_id": "run-1",
            "sequence": 1,
            "trace_id": "trace-1",
            "cancellation_scope": "run-1",
            "json": {"challenge": "openrealtime-sidecar-v4"},
        },
    )
    protocol.write_message(
        input_stream,
        protocol.MessageType.ELEMENT_FRAME,
        port="request",
        envelope={
            "type": hello["selected_ports"][0]["type"],
            "item_id": "request-cancel",
            "session_id": "session-1",
            "run_id": "run-cancel",
            "sequence": 1,
            "trace_id": "trace-cancel",
            "cancellation_scope": "run-cancel",
            "json": {
                "challenge": "openrealtime-sidecar-v4-cancel",
                "wait_for_cancel": True,
            },
        },
    )
    protocol.write_message(
        input_stream,
        protocol.MessageType.ELEMENT_FRAME,
        port="cancel",
        envelope={
            "type": hello["selected_ports"][1]["type"],
            "item_id": "cancel-1",
            "session_id": "session-1",
            "run_id": "run-cancel",
            "sequence": 2,
            "trace_id": "trace-cancel",
            "cancellation_scope": "run-cancel",
            "causal_parents": ["request-cancel"],
            "json": {"reason": "test cancellation"},
        },
    )
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    ConformanceElementSidecar(input_stream, output_stream).run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    result = protocol.read_message(output_stream)
    canceled = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert ready.get("version") == 4
    assert ready.get("element_descriptor") == hello["element_descriptor"]
    assert ready.get("applied_config_digest") == "sha256:" + hashlib.sha256(
        b'{"mode":"conformance"}'
    ).hexdigest()
    assert len(ready.get("resolved_capabilities")) == 3
    assert len(ready.get("negotiated_ports")) == 3
    assert result is not None and result.type == protocol.MessageType.ELEMENT_FRAME
    assert result.get("port") == "result"
    assert result.get("envelope")["run_id"] == "run-1"
    assert result.get("envelope")["causal_parents"] == ["request-1"]
    assert result.get("envelope")["json"] == {
        "challenge": "openrealtime-sidecar-v4",
        "status": "ok",
    }
    assert canceled is not None and canceled.type == protocol.MessageType.ELEMENT_FRAME
    assert canceled.get("port") == "result"
    assert canceled.get("envelope")["run_id"] == "run-cancel"
    assert canceled.get("envelope")["cancellation_scope"] == "run-cancel"
    assert canceled.get("envelope")["causal_parents"] == ["request-cancel", "cancel-1"]
    assert canceled.get("envelope")["json"] == {
        "challenge": "openrealtime-sidecar-v4-cancel",
        "status": "canceled",
    }
    assert protocol.read_message(output_stream) is None


class _BlobElement(ElementSidecar):
    element_descriptor = {
        "format_version": 1,
        "name": "test.BlobElement",
        "revision": 1,
        "ports": [
            {
                "name": "blob", "direction": "input",
                "type": {"name": "Stream", "arguments": [{"name": "artifact.Chunk"}]},
                "cardinality": "one", "default_depth": 2,
            },
            {
                "name": "ack", "direction": "output",
                "type": {"name": "Event", "arguments": [{"name": "artifact.Accepted"}]},
                "cardinality": "one", "default_depth": 2,
            },
        ],
        "reaction": {"triggers": ["blob"], "outcomes": ["ack"]},
    }
    runtime_artifact = {"id": "runtime/blob-element", "revision": "4"}
    provider_artifact = {"id": "provider/blob-element", "revision": "1"}
    adapter_artifact = {"id": "adapter/blob-wire", "revision": "4"}

    def __init__(self, input_stream, output_stream) -> None:
        super().__init__(input_stream, output_stream)
        self.seen: list[bytes] = []

    def on_element_frame(self, port, envelope, payload) -> None:
        assert port == "blob"
        self.seen.append(bytes(payload))
        # Deliberately mutate callback-owned memory. The retained selection and
        # the next frame's type validation must remain unchanged.
        envelope["type"]["arguments"][0]["name"] = "callback.Mutated"
        self.send_element_frame("ack", {
            "type": self.element_descriptor["ports"][1]["type"],
            "item_id": "ack-" + str(len(self.seen)),
            "causal_parents": [envelope["item_id"]],
            "json": {"accepted_bytes": len(payload)},
        })


def _blob_hello() -> dict:
    descriptor = copy.deepcopy(_BlobElement.element_descriptor)
    return {
        "version": 4,
        "element_descriptor": descriptor,
        "selected_ports": [
            {
                "name": "blob", "direction": "input", "type": descriptor["ports"][0]["type"],
                "formats": [{"payload_mode": "binary", "max_binary_bytes": 8}],
            },
            {
                "name": "ack", "direction": "output", "type": descriptor["ports"][1]["type"],
                "formats": [{"payload_mode": "json", "max_json_bytes": 1024}],
            },
        ],
    }


def test_generic_v4_python_element_carries_non_audio_binary_ports_with_owned_state() -> None:
    hello = _blob_hello()
    descriptor = hello["element_descriptor"]
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    for index, payload in enumerate((b"blob-one", b"blob-two"), start=1):
        protocol.write_message(
            input_stream, protocol.MessageType.ELEMENT_FRAME, payload,
            port="blob",
            envelope={
                "type": descriptor["ports"][0]["type"],
                "item_id": f"blob-{index}",
            },
        )
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()
    element = _BlobElement(input_stream, output_stream)

    element.run()

    assert element.seen == [b"blob-one", b"blob-two"]
    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    first = protocol.read_message(output_stream)
    second = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert ready.get("version") == 4
    assert ready.get("element_descriptor") == descriptor
    assert ready.get("runtime_artifact") == {"id": "runtime/blob-element", "revision": "4"}
    assert {
        capability["provider"]["id"] for capability in ready.get("resolved_capabilities")
    } == {"provider/blob-element"}
    assert "model" not in ready.header and "output_rate" not in ready.header
    assert first is not None and first.get("envelope")["json"] == {"accepted_bytes": 8}
    assert second is not None and second.get("envelope")["json"] == {"accepted_bytes": 8}
    assert protocol.read_message(output_stream) is None


def test_generic_v4_python_element_captures_immutable_session_identity() -> None:
    hello = _blob_hello()
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()
    element = _BlobElement(input_stream, output_stream)
    # Reusing instance attributes for later provider setup cannot rewrite the
    # contract already captured for this session.
    element.element_descriptor = {"format_version": 1, "name": "mutated.Provider"}
    element.runtime_artifact = {"id": "runtime/mutated", "revision": "later"}
    element.provider_artifact = {"id": "provider/mutated", "revision": "later"}
    element.adapter_artifact = {"id": "adapter/mutated", "revision": "later"}

    element.run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert ready.get("element_descriptor") == hello["element_descriptor"]
    assert ready.get("runtime_artifact") == {"id": "runtime/blob-element", "revision": "4"}
    assert {
        capability["provider"]["id"] for capability in ready.get("resolved_capabilities")
    } == {"provider/blob-element"}
    assert {
        capability["adapter"]["id"] for capability in ready.get("resolved_capabilities")
    } == {"adapter/blob-wire"}
    assert protocol.read_message(output_stream) is None


def test_generic_v4_python_element_attests_exact_unreconstructed_config_bytes() -> None:
    hello = _blob_hello()
    hello["element_config"] = {"threshold": 1e-7}
    encoded = io.BytesIO()
    protocol.write_message(encoded, protocol.MessageType.HELLO, **hello)
    # Python normally spells this exponent with a leading zero while Go
    # RawMessage permits either compact spelling. The sidecar must attest the
    # received bytes, not a Python reconstruction of their numeric value.
    header = encoded.getvalue().replace(b"1e-07", b"1e-7", 1)
    assert b'"element_config":{"threshold":1e-7}' in header
    input_stream = io.BytesIO(header)
    input_stream.seek(0, io.SEEK_END)
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    _BlobElement(input_stream, output_stream).run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert ready.get("applied_config_digest") == "sha256:" + hashlib.sha256(
        b'{"threshold":1e-7}'
    ).hexdigest()
    assert protocol.read_message(output_stream) is None


def test_generic_v4_python_element_applies_json_bounds_to_exact_wire_bytes() -> None:
    hello = _v4_conformance_hello()
    hello["selected_ports"][0]["formats"] = [
        {"payload_mode": "json", "max_json_bytes": 17},
    ]
    encoded = io.BytesIO()
    protocol.write_message(encoded, protocol.MessageType.HELLO, **hello)
    protocol.write_message(
        encoded, protocol.MessageType.ELEMENT_FRAME,
        port="request",
        envelope={
            "type": hello["selected_ports"][0]["type"],
            "item_id": "wire-byte-bound",
            "json": {"challenge": "x"},
        },
    )
    protocol.write_message(encoded, protocol.MessageType.BYE)
    # The compact JSON value is exactly 17 bytes. Add one legal byte of raw
    # whitespace; parsing and re-encoding would erase it and bypass the bound.
    source = encoded.getvalue().replace(
        b'"json":{"challenge":"x"}', b'"json":{ "challenge":"x"}', 1,
    )
    input_stream = io.BytesIO(source)
    output_stream = io.BytesIO()

    ConformanceElementSidecar(input_stream, output_stream).run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    failure = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert failure is not None and failure.type == protocol.MessageType.ERROR
    assert failure.get("fatal") is True and failure.get("code") == "bad_frame"
    assert "payload bounds" in failure.get("text")
    assert protocol.read_message(output_stream) is None


@pytest.mark.parametrize(
    ("case", "port", "type_index", "payload", "json_value", "error_text"),
    [
        ("wrong direction", "ack", 1, b"", {"accepted_bytes": 0}, "unselected input port"),
        ("wrong type", "blob", 1, b"blob-one", None, "wrong type"),
        ("binary bound", "blob", 0, b"nine-byte", None, "payload bounds"),
        ("JSON on binary lane", "blob", 0, b"blob-one", {"unexpected": True}, "binary-only"),
        ("missing binary body", "blob", 0, b"", None, "binary-only"),
    ],
)
def test_generic_v4_python_element_rejects_adversarial_typed_frames(
    case: str, port: str, type_index: int, payload: bytes,
    json_value: object | None, error_text: str,
) -> None:
    hello = _blob_hello()
    descriptor = hello["element_descriptor"]
    envelope = {
        "type": descriptor["ports"][type_index]["type"],
        "item_id": "adversarial-" + case.replace(" ", "-"),
    }
    if json_value is not None:
        envelope["json"] = json_value
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    protocol.write_message(
        input_stream, protocol.MessageType.ELEMENT_FRAME, payload,
        port=port, envelope=envelope,
    )
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    _BlobElement(input_stream, output_stream).run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    failure = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert failure is not None and failure.type == protocol.MessageType.ERROR
    assert failure.get("fatal") is True and failure.get("code") == "bad_frame"
    assert error_text in failure.get("text")
    assert protocol.read_message(output_stream) is None


def test_generic_v4_python_element_rejects_unbounded_negotiation_before_ready() -> None:
    hello = _blob_hello()
    hello["selected_ports"][0]["formats"] = [
        {"payload_mode": "binary", "max_binary_bytes": index + 1}
        for index in range(17)
    ]
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    _BlobElement(input_stream, output_stream).run()

    output_stream.seek(0)
    failure = protocol.read_message(output_stream)
    assert failure is not None and failure.type == protocol.MessageType.ERROR
    assert failure.get("fatal") is True and failure.get("code") == "unsupported_element"
    assert "bounded format offer" in failure.get("text")
    assert protocol.read_message(output_stream) is None


@pytest.mark.parametrize(
    ("case", "error_text"),
    [
        ("unknown descriptor field", "unknown fields"),
        ("misdirected interrupt", "wrong direction"),
        ("reversible external effect", "cannot claim reversibility"),
        ("unresolved selected generic", "unresolved element type variable"),
    ],
)
def test_generic_v4_python_element_rejects_malformed_graph_contracts(
    case: str, error_text: str,
) -> None:
    hello = _blob_hello()
    descriptor = hello["element_descriptor"]
    if case == "unknown descriptor field":
        descriptor["legacy_model"] = "must-not-acquire-authority"
    elif case == "misdirected interrupt":
        descriptor["reaction"] = {"triggers": ["blob"], "interrupts": ["ack"]}
    elif case == "reversible external effect":
        descriptor["effects"] = [{
            "name": "device.click", "external": True,
            "authority": "authority.ComputerUse", "reversible": True,
        }]
    elif case == "unresolved selected generic":
        descriptor["generics"] = ["T"]
        descriptor["ports"][0]["type"] = {"variable": "T"}
        hello["selected_ports"][0]["type"] = {"variable": "T"}
    bad_element = type(
        "BadContractElement", (_BlobElement,), {"element_descriptor": descriptor},
    )
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    bad_element(input_stream, output_stream).run()

    output_stream.seek(0)
    failure = protocol.read_message(output_stream)
    assert failure is not None and failure.type == protocol.MessageType.ERROR
    assert failure.get("fatal") is True and failure.get("code") == "unsupported_element"
    assert error_text in failure.get("text")
    assert protocol.read_message(output_stream) is None


class _PreCancellationElement(ConformanceElementSidecar):
    def __init__(self, input_stream, output_stream) -> None:
        super().__init__(input_stream, output_stream)
        self._release_request = threading.Event()

    def on_element_frame(self, port, envelope, payload) -> None:
        if port == "request":
            if not self._release_request.wait(timeout=1):
                raise TimeoutError("typed cancellation did not overtake queued work")
        super().on_element_frame(port, envelope, payload)
        if port == "cancel":
            self._release_request.set()


def test_generic_v4_python_element_retains_pre_cancellation_until_queued_work_observes_it() -> None:
    hello = _v4_conformance_hello()
    input_stream = io.BytesIO()
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    protocol.write_message(
        input_stream, protocol.MessageType.ELEMENT_FRAME,
        port="request",
        envelope={
            "type": hello["selected_ports"][0]["type"],
            "item_id": "request-before-cancel", "run_id": "pre-cancel-run",
            "cancellation_scope": "pre-cancel-run",
            "json": {"challenge": "pre-cancel", "wait_for_cancel": True},
        },
    )
    protocol.write_message(
        input_stream, protocol.MessageType.ELEMENT_FRAME,
        port="cancel",
        envelope={
            "type": hello["selected_ports"][1]["type"],
            "item_id": "cancel-before-worker", "run_id": "pre-cancel-run",
            "cancellation_scope": "pre-cancel-run",
            "causal_parents": ["request-before-cancel"],
            "json": {"reason": "pre-cancel test"},
        },
    )
    protocol.write_message(input_stream, protocol.MessageType.BYE)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    _PreCancellationElement(input_stream, output_stream).run()

    output_stream.seek(0)
    ready = protocol.read_message(output_stream)
    canceled = protocol.read_message(output_stream)
    assert ready is not None and ready.type == protocol.MessageType.READY
    assert canceled is not None and canceled.type == protocol.MessageType.ELEMENT_FRAME
    assert canceled.get("envelope")["json"] == {
        "challenge": "pre-cancel", "status": "canceled",
    }
    assert canceled.get("envelope")["causal_parents"] == [
        "request-before-cancel", "cancel-before-worker",
    ]
    assert protocol.read_message(output_stream) is None


def test_legacy_python_sidecar_refuses_unknown_v4_descriptors() -> None:
    input_stream = io.BytesIO()
    hello = _v4_conformance_hello()
    hello["element_descriptor"]["name"] = "model.External"
    protocol.write_message(input_stream, protocol.MessageType.HELLO, **hello)
    input_stream.seek(0)
    output_stream = io.BytesIO()

    Sidecar(input_stream, output_stream).run()

    output_stream.seek(0)
    refusal = protocol.read_message(output_stream)
    assert refusal is not None and refusal.type == protocol.MessageType.ERROR
    assert refusal.get("fatal") is True
    assert refusal.get("code") == "unsupported_element"
