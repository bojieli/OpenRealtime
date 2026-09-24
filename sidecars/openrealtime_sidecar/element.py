"""Strict protocol-v4 server support for arbitrary typed graph elements.

This module is intentionally separate from :mod:`sidecar`.  The legacy
``Sidecar`` class owns the frozen v1-v3 audio/turn callback vocabulary;
``ElementSidecar`` owns descriptor-selected v4 ports.  A model therefore
cannot acquire graph authority merely by routing a v4 Hello through legacy
``on_audio`` or ``on_respond`` methods.
"""

from __future__ import annotations

import copy
import hashlib
import json
import queue
import re
import sys
import threading
import time
import traceback
from typing import Any, BinaryIO

from .protocol import Message, MessageType, log, read_message, write_message

ELEMENT_PROTOCOL_VERSION = 4
MAX_ELEMENT_PORTS = 256
MAX_PORT_FORMATS = 16
MAX_CAPABILITIES = 512
MAX_REQUIREMENTS = 256
MAX_JSON_BYTES = 512 << 10
MAX_BINARY_BYTES = 16 << 20
MAX_IDENTIFIER_BYTES = 1024
MAX_CAUSAL_PARENTS = 256
MAX_QUEUED_FRAMES = 256

_PAYLOAD_JSON = "json"
_PAYLOAD_BINARY = "binary"
_PAYLOAD_JSON_BINARY = "json_binary"
_DIRECTIONS = ("input", "output")
_CARDINALITIES = ("one", "variadic")
_PROTOCOL_ARITY = {
    "Event": 1, "Stream": 1, "Segmented": 2, "Revisions": 2,
    "State": 1, "Trigger": 1, "Interrupt": 1, "Request": 2, "Reply": 2,
}
_ELEMENT_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_-]*(?:\.[A-Za-z][A-Za-z0-9_-]*)+$")
_PORT_NAME = re.compile(r"^[a-z][a-z0-9_-]*$")
_TYPE_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_.-]*$")
_VARIABLE_NAME = re.compile(r"^[A-Z][A-Za-z0-9_]*$")
_CONTRACT_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_.:/-]*$")
_ENVELOPE_FIELDS = {
    "type", "item_id", "session_id", "source_id", "opportunity_id",
    "run_id", "sequence", "capture_ns", "receive_ns", "trace_id",
    "cancellation_scope", "causal_parents", "media", "json",
}


def _compact_json_bytes(value: Any) -> bytes:
    """Encode JSON exactly as the bundled Python frame writer does."""
    return json.dumps(
        value, separators=(",", ":"), ensure_ascii=False, allow_nan=False,
    ).encode("utf-8")


def _raw_object_field(source: bytes, wanted: str) -> tuple[bool, bytes]:
    """Return one exact top-level JSON member without reconstructing it.

    ``json.loads`` intentionally discards number spelling and whitespace. Both
    are part of a v4 configuration digest and negotiated JSON byte limits, so
    the transport retains the relevant raw value after the strict parser has
    already rejected duplicate fields and non-finite numbers.
    """
    try:
        text = source.decode("utf-8")
    except UnicodeDecodeError as failure:
        raise ValueError("sidecar header is not UTF-8") from failure
    decoder = json.JSONDecoder()

    def skip_space(position: int) -> int:
        while position < len(text) and text[position] in " \t\r\n":
            position += 1
        return position

    position = skip_space(0)
    if position >= len(text) or text[position] != "{":
        raise ValueError("sidecar header must be one JSON object")
    position = skip_space(position + 1)
    while position < len(text) and text[position] != "}":
        name, position = decoder.raw_decode(text, position)
        if not isinstance(name, str):
            raise ValueError("sidecar object field name must be a string")
        position = skip_space(position)
        if position >= len(text) or text[position] != ":":
            raise ValueError("sidecar object field is missing a colon")
        start = skip_space(position + 1)
        _, end = decoder.raw_decode(text, start)
        if name == wanted:
            return True, text[start:end].encode("utf-8")
        position = skip_space(end)
        if position < len(text) and text[position] == ",":
            position = skip_space(position + 1)
            continue
        if position < len(text) and text[position] == "}":
            break
        raise ValueError("sidecar object field is not comma-delimited")
    return False, b""


def _bounded_identifier(label: str, value: Any, *, required: bool = True) -> str:
    if not isinstance(value, str):
        raise ValueError(f"{label} must be a string")
    if required and not value:
        raise ValueError(f"{label} is required")
    if value != value.strip() or "\x00" in value or "\r" in value or "\n" in value:
        raise ValueError(f"{label} is not canonical")
    if len(value.encode("utf-8")) > MAX_IDENTIFIER_BYTES:
        raise ValueError(f"{label} exceeds {MAX_IDENTIFIER_BYTES} bytes")
    return value


def _bounded_int(label: str, value: Any, maximum: int, *, positive: bool = False) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{label} must be an integer")
    minimum = 1 if positive else 0
    if value < minimum or value > maximum:
        raise ValueError(f"{label} is outside [{minimum},{maximum}]")
    return value


def type_string(
    value: Any, *, _depth: int = 0, _budget: list[int] | None = None,
    _generics: frozenset[str] = frozenset(), _descriptor: bool = False,
) -> str:
    """Validate and render one language-neutral graph-port type.

    The public/default path accepts only a fully concrete temporal port. The
    descriptor path additionally accepts variables declared by that exact
    descriptor; selected live ports never do.
    """
    if _depth > 64:
        raise ValueError("element port type nesting exceeds 64")
    if _budget is None:
        _budget = [1024]
    _budget[0] -= 1
    if _budget[0] < 0:
        raise ValueError("element port type exceeds 1024 nodes")
    if not isinstance(value, dict):
        raise ValueError("element port type must be an object")
    unknown = set(value) - {"name", "variable", "arguments"}
    if unknown:
        raise ValueError(f"element port type has unknown fields {sorted(unknown)}")
    variable = value.get("variable", "")
    name = value.get("name", "")
    arguments = value.get("arguments", [])
    if not isinstance(arguments, list):
        raise ValueError("element type arguments must be a list")
    if variable:
        _bounded_identifier("element type variable", variable)
        if not _VARIABLE_NAME.fullmatch(variable):
            raise ValueError(f"invalid element type variable {variable!r}")
        if name or arguments:
            raise ValueError("element type variable cannot have a name or arguments")
        if not _descriptor or variable not in _generics:
            raise ValueError(f"undeclared or unresolved element type variable {variable!r}")
        return "$" + variable
    _bounded_identifier("element type name", name)
    if not _TYPE_NAME.fullmatch(name):
        raise ValueError(f"invalid element type name {name!r}")
    rendered = [
        type_string(
            argument, _depth=_depth + 1, _budget=_budget,
            _generics=_generics, _descriptor=_descriptor,
        )
        for argument in arguments
    ]
    if _depth == 0:
        if name not in _PROTOCOL_ARITY:
            raise ValueError(f"port type root {name!r} is not a temporal protocol")
        if len(arguments) != _PROTOCOL_ARITY[name]:
            raise ValueError(
                f"protocol {name} has {len(arguments)} arguments, "
                f"want {_PROTOCOL_ARITY[name]}"
            )
    if not rendered:
        return name
    return name + "<" + ", ".join(rendered) + ">"


def _validate_artifact(label: str, value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be an object")
    unknown = set(value) - {"id", "revision", "digest"}
    if unknown:
        raise ValueError(f"{label} has unknown fields {sorted(unknown)}")
    identity = _bounded_identifier(f"{label} id", value.get("id", ""))
    revision = _bounded_identifier(
        f"{label} revision", value.get("revision", ""), required=False,
    )
    digest = _bounded_identifier(
        f"{label} digest", value.get("digest", ""), required=False,
    )
    if not revision and not digest:
        raise ValueError(f"{label} requires a revision or digest")
    lowered = (identity + "\x00" + revision).lower()
    for placeholder in ("latest", "current", "unknown", "unresolved"):
        if any(
            part == placeholder or part.endswith((":" + placeholder, "@" + placeholder, "/" + placeholder))
            for part in (identity.lower(), revision.lower())
        ):
            raise ValueError(f"{label} contains a mutable selector")
    if any(marker in lowered for marker in ("${", "{{", "<revision>", "<digest>", "<version>")):
        raise ValueError(f"{label} contains a placeholder selector")
    if digest:
        if len(digest) != len("sha256:") + 64 or not digest.startswith("sha256:"):
            raise ValueError(f"{label} digest must be a SHA-256")
        encoded = digest.removeprefix("sha256:")
        if encoded != encoded.lower() or any(character not in "0123456789abcdef" for character in encoded):
            raise ValueError(f"{label} digest must be canonical lowercase hexadecimal")
    return copy.deepcopy(value)


def _validate_media_format(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError("wire media profile must be an object")
    allowed = {
        "kind", "encoding", "sample_format", "sample_rate_hz", "channels",
        "max_frame_duration_ms", "max_width", "max_height",
        "max_frame_rate_millihz",
    }
    unknown = set(value) - allowed
    if unknown:
        raise ValueError(f"wire media profile has unknown fields {sorted(unknown)}")
    kind = value.get("kind")
    if kind not in ("audio", "video"):
        raise ValueError(f"unknown media kind {kind!r}")
    encoding = _bounded_identifier("media encoding", value.get("encoding", ""))
    if encoding != encoding.lower() or len(encoding.encode()) > 128:
        raise ValueError("media encoding must be bounded canonical lowercase")
    if kind == "audio":
        sample = _bounded_identifier("media sample format", value.get("sample_format", ""))
        if sample != sample.lower() or len(sample.encode()) > 128:
            raise ValueError("media sample format must be bounded canonical lowercase")
        _bounded_int("media sample rate", value.get("sample_rate_hz", 0), 768_000, positive=True)
        _bounded_int("media channels", value.get("channels", 0), 64, positive=True)
        _bounded_int(
            "media maximum frame duration", value.get("max_frame_duration_ms", 0),
            10_000, positive=True,
        )
        for field in ("max_width", "max_height", "max_frame_rate_millihz"):
            if value.get(field, 0) != 0:
                raise ValueError("audio media cannot declare video bounds")
    else:
        _bounded_int("media maximum width", value.get("max_width", 0), 32_768, positive=True)
        _bounded_int("media maximum height", value.get("max_height", 0), 32_768, positive=True)
        _bounded_int(
            "media maximum frame rate", value.get("max_frame_rate_millihz", 0),
            1_000_000, positive=True,
        )
        for field in ("sample_format", "sample_rate_hz", "channels", "max_frame_duration_ms"):
            if value.get(field, "" if field == "sample_format" else 0) not in ("", 0):
                raise ValueError("video media cannot declare audio bounds")
    return copy.deepcopy(value)


def _validate_wire_format(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError("port wire format must be an object")
    unknown = set(value) - {"payload_mode", "max_json_bytes", "max_binary_bytes", "media"}
    if unknown:
        raise ValueError(f"port wire format has unknown fields {sorted(unknown)}")
    mode = value.get("payload_mode")
    json_bytes = value.get("max_json_bytes", 0)
    binary_bytes = value.get("max_binary_bytes", 0)
    if mode == _PAYLOAD_JSON:
        _bounded_int("maximum JSON bytes", json_bytes, MAX_JSON_BYTES, positive=True)
        if binary_bytes != 0:
            raise ValueError("JSON wire format cannot allow binary bytes")
    elif mode == _PAYLOAD_BINARY:
        _bounded_int("maximum binary bytes", binary_bytes, MAX_BINARY_BYTES, positive=True)
        if json_bytes != 0:
            raise ValueError("binary wire format cannot allow JSON bytes")
    elif mode == _PAYLOAD_JSON_BINARY:
        _bounded_int("maximum JSON bytes", json_bytes, MAX_JSON_BYTES, positive=True)
        _bounded_int("maximum binary bytes", binary_bytes, MAX_BINARY_BYTES, positive=True)
    else:
        raise ValueError(f"unknown payload mode {mode!r}")
    if "media" in value and value["media"] is not None:
        if mode == _PAYLOAD_JSON:
            raise ValueError("live media requires a binary payload lane")
        _validate_media_format(value["media"])
    return copy.deepcopy(value)


def _validate_frame_media(metadata: Any, profile: dict[str, Any]) -> None:
    if not isinstance(metadata, dict):
        raise ValueError("media frame metadata must be an object")
    allowed = {
        "kind", "encoding", "sample_format", "sample_rate_hz", "channels",
        "frame_duration_ms", "width", "height", "frame_rate_millihz",
    }
    unknown = set(metadata) - allowed
    if unknown:
        raise ValueError(f"media frame metadata has unknown fields {sorted(unknown)}")
    if metadata.get("kind") != profile.get("kind") or metadata.get("encoding") != profile.get("encoding"):
        raise ValueError("media frame kind or encoding differs from the negotiated profile")
    if profile["kind"] == "audio":
        for field in ("sample_format", "sample_rate_hz", "channels"):
            if metadata.get(field) != profile.get(field):
                raise ValueError(f"audio frame {field} differs from the negotiated profile")
        duration = _bounded_int(
            "audio frame duration", metadata.get("frame_duration_ms", 0),
            10_000, positive=True,
        )
        if duration > profile["max_frame_duration_ms"]:
            raise ValueError("audio frame duration exceeds the negotiated profile")
        if any(metadata.get(field, 0) != 0 for field in ("width", "height", "frame_rate_millihz")):
            raise ValueError("audio frame metadata contains video fields")
    else:
        width = _bounded_int("video frame width", metadata.get("width", 0), 32_768, positive=True)
        height = _bounded_int("video frame height", metadata.get("height", 0), 32_768, positive=True)
        rate = _bounded_int(
            "video frame rate", metadata.get("frame_rate_millihz", 0),
            1_000_000, positive=True,
        )
        if width > profile["max_width"] or height > profile["max_height"] or rate > profile["max_frame_rate_millihz"]:
            raise ValueError("video frame exceeds the negotiated profile")
        if any(metadata.get(field, "" if field == "sample_format" else 0) not in ("", 0)
               for field in ("sample_format", "sample_rate_hz", "channels", "frame_duration_ms")):
            raise ValueError("video frame metadata contains audio fields")


class ElementSidecar:
    """Base server for one descriptor-backed protocol-v4 element.

    Subclasses publish immutable descriptor/runtime/provider/adapter identities
    and implement :meth:`on_element_frame`.  Interrupt ports declared by the
    descriptor are dispatched on the reader thread; all other inputs use one
    bounded worker queue.  This keeps cancellation responsive without giving
    the transport knowledge of audio, models, or provider-specific actions.
    """

    element_descriptor: dict[str, Any] = {}
    runtime_artifact: dict[str, Any] = {}
    provider_artifact: dict[str, Any] = {}
    adapter_artifact: dict[str, Any] = {}
    resolved_capabilities: tuple[dict[str, Any], ...] = ()

    def __init__(self, input_stream: BinaryIO, output_stream: BinaryIO) -> None:
        self._input = input_stream
        self._output = output_stream
        # Capture the implementation contract once. Provider code may reuse
        # or mutate the class-level construction dictionaries later, but that
        # must not rewrite the identity of an already-created session.
        self._descriptor = copy.deepcopy(self.element_descriptor)
        self._runtime_artifact = copy.deepcopy(self.runtime_artifact)
        self._provider_artifact = copy.deepcopy(self.provider_artifact)
        self._adapter_artifact = copy.deepcopy(self.adapter_artifact)
        self._declared_capabilities = copy.deepcopy(self.resolved_capabilities)
        self._write_lock = threading.Lock()
        self._work: queue.Queue[tuple[str, dict[str, Any], bytes] | None] = queue.Queue(
            maxsize=MAX_QUEUED_FRAMES,
        )
        self._selected: dict[tuple[str, str], dict[str, Any]] = {}
        self._negotiated: dict[tuple[str, str], dict[str, Any]] = {}
        self._interrupts: set[str] = set()
        self._worker_failure = threading.Event()

    def send(self, message_type: str, payload: bytes = b"", **header: Any) -> None:
        with self._write_lock:
            write_message(self._output, message_type, payload, **header)

    def error(self, message: str, *, code: str = "", fatal: bool = False) -> None:
        self.send(MessageType.ERROR, text=message, code=code or None, fatal=fatal or None)

    def configure_element(self, config: Any) -> None:
        """Apply the exact Hello configuration before Ready is emitted."""

    def supports_wire_format(
        self, port: str, direction: str, value_type: dict[str, Any], wire_format: dict[str, Any],
    ) -> bool:
        """Return whether the concrete adapter supports a structurally valid offer.

        The generic framing layer supports every bounded v4 lane combination.
        A provider adapter may override this to restrict codecs or profiles.
        """
        return True

    def on_element_frame(self, port: str, envelope: dict[str, Any], payload: bytes) -> None:
        raise NotImplementedError

    def on_close(self) -> None:
        """Cancel provider work and release resources before worker join."""

    def send_element_frame(
        self, port: str, envelope: dict[str, Any], payload: bytes = b"",
    ) -> None:
        message = Message(
            MessageType.ELEMENT_FRAME,
            {"type": MessageType.ELEMENT_FRAME, "port": port, "envelope": copy.deepcopy(envelope)},
            bytes(payload),
        )
        if payload:
            message.header["payload_bytes"] = len(payload)
        self._validate_element_frame(message, "output")
        self.send(
            MessageType.ELEMENT_FRAME, bytes(payload), port=port,
            envelope=copy.deepcopy(envelope),
        )

    def run(self) -> None:
        try:
            hello = read_message(self._input)
        except Exception as failure:  # noqa: BLE001
            self.error(f"malformed hello: {failure}", code="bad_frame", fatal=True)
            return
        if hello is None or hello.type != MessageType.HELLO:
            self.error("the first frame must be hello", code="bad_hello", fatal=True)
            return
        try:
            ready = self._negotiate(hello)
            self.configure_element(copy.deepcopy(hello.get("element_config")))
        except Exception as failure:  # noqa: BLE001
            self.error(str(failure), code="unsupported_element", fatal=True)
            return
        self.send(MessageType.READY, **ready)

        worker = threading.Thread(target=self._work_loop, daemon=True)
        worker.start()
        try:
            self._read_loop()
        finally:
            try:
                self.on_close()
            except Exception:  # noqa: BLE001
                log(traceback.format_exc())
            try:
                self._work.put(None, timeout=1)
            except queue.Full:
                self.error("v4 worker queue did not quiesce", code="shutdown_timeout", fatal=True)
            worker.join(timeout=10)
            if worker.is_alive():
                self.error("v4 element worker ignored shutdown", code="shutdown_timeout", fatal=True)

    def _negotiate(self, hello: Message) -> dict[str, Any]:
        allowed = {
            "type", "version", "element_descriptor", "element_config",
            "selected_ports", "required_capabilities", "runtime_artifact", "payload_bytes",
        }
        unknown = set(hello.header) - allowed
        if unknown:
            raise ValueError(f"v4 hello contains unrelated fields {sorted(unknown)}")
        version = hello.get("version")
        if isinstance(version, bool) or version != ELEMENT_PROTOCOL_VERSION or hello.payload:
            raise ValueError("graph-native sidecar requires a payload-free protocol-v4 Hello")
        # Go's encoding/json currently emits a zero-valued non-pointer struct
        # despite omitempty. It carries no authority and must remain exactly
        # empty until a future wire revision makes the field a pointer.
        if hello.get("runtime_artifact", {}) not in ({}, {"id": ""}):
            raise ValueError("v4 Hello cannot assert a runtime artifact")
        descriptor = hello.get("element_descriptor")
        expected = copy.deepcopy(self._descriptor)
        self._validate_descriptor(expected)
        if descriptor != expected:
            raise ValueError("v4 Hello descriptor differs from this element implementation")
        config = hello.get("element_config")
        if "element_config" in hello.header:
            if hello.raw_header:
                found, config_bytes = _raw_object_field(hello.raw_header, "element_config")
                if not found:
                    raise ValueError("v4 Hello lost its raw element configuration")
            else:
                config_bytes = _compact_json_bytes(config)
        else:
            config_bytes = b""
        if len(config_bytes) > MAX_JSON_BYTES:
            raise ValueError("v4 element configuration exceeds its byte bound")
        selections = hello.get("selected_ports")
        if not isinstance(selections, list) or not 1 <= len(selections) <= MAX_ELEMENT_PORTS:
            raise ValueError("v4 Hello requires a bounded non-empty selected port set")
        declared = {
            (port["direction"], port["name"]): port
            for port in expected["ports"]
        }
        negotiated: list[dict[str, Any]] = []
        capabilities = [self._validate_capability(value) for value in self._declared_capabilities]
        seen: set[tuple[str, str]] = set()
        for selection in selections:
            if not isinstance(selection, dict):
                raise ValueError("selected port must be an object")
            unknown = set(selection) - {"name", "direction", "type", "formats"}
            if unknown:
                raise ValueError(f"selected port has unknown fields {sorted(unknown)}")
            name = _bounded_identifier("selected port name", selection.get("name", ""))
            direction = selection.get("direction")
            if direction not in _DIRECTIONS:
                raise ValueError(f"selected port {name} has invalid direction {direction!r}")
            key = (direction, name)
            if key in seen:
                raise ValueError(f"selected port {direction} {name} is repeated")
            seen.add(key)
            port = declared.get(key)
            if port is None or port.get("type") != selection.get("type"):
                raise ValueError(f"selected port {direction} {name} drifts from the descriptor")
            contract = type_string(selection.get("type"))
            offers = selection.get("formats")
            if not isinstance(offers, list) or not 1 <= len(offers) <= MAX_PORT_FORMATS:
                raise ValueError(f"selected port {name} requires a bounded format offer")
            selected_format = None
            canonical_offers: list[dict[str, Any]] = []
            for offer in offers:
                valid = _validate_wire_format(offer)
                if valid in canonical_offers:
                    raise ValueError(f"selected port {name} repeats a wire format")
                canonical_offers.append(valid)
                if selected_format is None and self.supports_wire_format(
                    name, direction, copy.deepcopy(selection["type"]), copy.deepcopy(valid),
                ):
                    selected_format = valid
            if selected_format is None:
                raise ValueError(f"selected port {name} offers no supported wire format")
            self._selected[key] = copy.deepcopy(selection)
            self._negotiated[key] = copy.deepcopy(selected_format)
            negotiated.append({
                "name": name, "direction": direction, "format": copy.deepcopy(selected_format),
            })
            capabilities.append({
                "name": f"port.{direction}.{name}", "contract": contract,
                "provider": _validate_artifact("port provider", self._provider_artifact),
                "adapter": _validate_artifact("port adapter", self._adapter_artifact),
            })
        if len(capabilities) > MAX_CAPABILITIES:
            raise ValueError("resolved capability set exceeds its bound")
        capabilities = self._canonical_capabilities(capabilities)
        requirements = hello.get("required_capabilities") or []
        if not isinstance(requirements, list) or len(requirements) > MAX_REQUIREMENTS:
            raise ValueError("required capability set exceeds its bound")
        seen_requirements: set[tuple[str, str]] = set()
        for requirement in requirements:
            if not isinstance(requirement, dict) or set(requirement) - {"name", "contract"}:
                raise ValueError("capability requirement is malformed")
            name = _bounded_identifier("capability requirement name", requirement.get("name", ""))
            contract = _bounded_identifier(
                "capability requirement contract", requirement.get("contract", ""), required=False,
            )
            if name.startswith("port."):
                raise ValueError("explicit requirements cannot use the derived port namespace")
            identity = (name, contract)
            if identity in seen_requirements:
                raise ValueError(f"capability requirement {name} is repeated")
            seen_requirements.add(identity)
            matches = [
                capability for capability in capabilities
                if capability["name"] == name and (
                    not contract or capability.get("contract", "") == contract
                )
            ]
            if len(matches) != 1:
                raise ValueError(f"required capability {name} has {len(matches)} live identities")
        reaction = expected.get("reaction") or {}
        self._interrupts = set(reaction.get("interrupts") or [])
        return {
            "version": ELEMENT_PROTOCOL_VERSION,
            "element_descriptor": expected,
            "applied_config_digest": "sha256:" + hashlib.sha256(config_bytes).hexdigest(),
            "runtime_artifact": _validate_artifact("runtime artifact", self._runtime_artifact),
            "resolved_capabilities": capabilities,
            "negotiated_ports": negotiated,
        }

    def _validate_descriptor(self, descriptor: Any) -> None:
        if not isinstance(descriptor, dict):
            raise ValueError("element descriptor must be an object")
        allowed = {
            "format_version", "name", "revision", "generics", "ports", "reaction",
            "state_schema", "config_schema", "dependencies", "effects",
            "composite_fingerprint",
        }
        unknown = set(descriptor) - allowed
        if unknown:
            raise ValueError(f"element descriptor has unknown fields {sorted(unknown)}")
        if _bounded_int(
            "element descriptor format_version", descriptor.get("format_version", 0), 1,
            positive=True,
        ) != 1:
            raise ValueError("element descriptor format_version must be 1")
        element_name = _bounded_identifier(
            "element descriptor name", descriptor.get("name", ""),
        )
        if not _ELEMENT_NAME.fullmatch(element_name):
            raise ValueError(f"invalid element descriptor name {element_name!r}")
        _bounded_int(
            "element descriptor revision", descriptor.get("revision", 0),
            2**64 - 1, positive=True,
        )
        generics = descriptor.get("generics") or []
        if not isinstance(generics, list) or len(generics) > MAX_ELEMENT_PORTS:
            raise ValueError("element descriptor generic set exceeds its bound")
        generic_set: set[str] = set()
        for generic in generics:
            name = _bounded_identifier("element generic", generic)
            if not _VARIABLE_NAME.fullmatch(name):
                raise ValueError(f"invalid element generic {name!r}")
            if name in generic_set:
                raise ValueError(f"element descriptor repeats generic {name!r}")
            generic_set.add(name)
        ports = descriptor.get("ports")
        if not isinstance(ports, list) or not 1 <= len(ports) <= MAX_ELEMENT_PORTS:
            raise ValueError("element descriptor needs a bounded non-empty port set")
        declared: dict[str, dict[str, Any]] = {}
        for port in ports:
            if not isinstance(port, dict):
                raise ValueError("descriptor port must be an object")
            unknown = set(port) - {
                "name", "direction", "type", "cardinality", "required",
                "min_connections", "loss_allowed", "default_depth",
            }
            if unknown:
                raise ValueError(f"descriptor port has unknown fields {sorted(unknown)}")
            name = _bounded_identifier("descriptor port name", port.get("name", ""))
            if not _PORT_NAME.fullmatch(name):
                raise ValueError(f"invalid descriptor port name {name!r}")
            if name in declared:
                raise ValueError(f"element descriptor repeats port {name!r}")
            direction = port.get("direction")
            if direction not in _DIRECTIONS:
                raise ValueError(f"descriptor port {name} has invalid direction")
            type_string(
                port.get("type"), _generics=frozenset(generic_set), _descriptor=True,
            )
            cardinality = port.get("cardinality")
            if cardinality not in _CARDINALITIES:
                raise ValueError(f"descriptor port {name} has invalid cardinality")
            for field in ("required", "loss_allowed"):
                if not isinstance(port.get(field, False), bool):
                    raise ValueError(f"descriptor port {name} {field} must be boolean")
            minimum = _bounded_int(
                f"descriptor port {name} min_connections",
                port.get("min_connections", 0), 2**31 - 1,
            )
            if cardinality == "one" and minimum > 1:
                raise ValueError(
                    f"singular descriptor port {name} cannot require {minimum} connections"
                )
            _bounded_int(
                f"descriptor port {name} default_depth",
                port.get("default_depth", 0), 2**31 - 1,
            )
            declared[name] = port

        reaction = descriptor.get("reaction") or {}
        if not isinstance(reaction, dict):
            raise ValueError("element descriptor reaction must be an object")
        unknown = set(reaction) - {
            "triggers", "sampled_state", "interrupts", "outcomes",
            "max_concurrency", "breaks_cycles",
        }
        if unknown:
            raise ValueError(f"element reaction has unknown fields {sorted(unknown)}")
        _bounded_int(
            "element reaction max_concurrency", reaction.get("max_concurrency", 0),
            2**31 - 1,
        )
        if not isinstance(reaction.get("breaks_cycles", False), bool):
            raise ValueError("element reaction breaks_cycles must be boolean")
        seen_reaction: dict[str, str] = {}
        for field, label, direction in (
            ("triggers", "trigger", "input"),
            ("sampled_state", "sampled state", "input"),
            ("interrupts", "interrupt", "input"),
            ("outcomes", "outcome", "output"),
        ):
            names = reaction.get(field) or []
            if not isinstance(names, list) or len(names) > MAX_ELEMENT_PORTS:
                raise ValueError(f"element reaction {label} set exceeds its bound")
            for value in names:
                name = _bounded_identifier(f"element reaction {label}", value)
                port = declared.get(name)
                if port is None:
                    raise ValueError(f"element reaction names unknown {label} port {name!r}")
                if port["direction"] != direction:
                    raise ValueError(
                        f"element reaction {label} port {name!r} has the wrong direction"
                    )
                if name in seen_reaction:
                    raise ValueError(
                        f"element reaction port {name!r} is both "
                        f"{seen_reaction[name]} and {label}"
                    )
                seen_reaction[name] = label

        for field in ("state_schema", "config_schema"):
            value = descriptor.get(field, "")
            _bounded_identifier(f"element descriptor {field}", value, required=False)

        dependencies = descriptor.get("dependencies") or []
        effects = descriptor.get("effects") or []
        if not isinstance(dependencies, list) or len(dependencies) > MAX_CAPABILITIES:
            raise ValueError("element dependency set exceeds its bound")
        if not isinstance(effects, list) or len(effects) > MAX_CAPABILITIES:
            raise ValueError("element effect set exceeds its bound")
        seen_dependencies: set[str] = set()
        for dependency in dependencies:
            if not isinstance(dependency, dict) or set(dependency) - {"name", "optional"}:
                raise ValueError("element dependency is malformed")
            name = _bounded_identifier("element dependency", dependency.get("name", ""))
            if not _CONTRACT_NAME.fullmatch(name) or name in seen_dependencies:
                raise ValueError(f"invalid or repeated element dependency {name!r}")
            if not isinstance(dependency.get("optional", False), bool):
                raise ValueError(f"element dependency {name} optional must be boolean")
            seen_dependencies.add(name)
        seen_effects: set[str] = set()
        for effect in effects:
            allowed_effect = {"name", "external", "authority", "reversible"}
            if not isinstance(effect, dict) or set(effect) - allowed_effect:
                raise ValueError("element effect is malformed")
            name = _bounded_identifier("element effect", effect.get("name", ""))
            if not _CONTRACT_NAME.fullmatch(name) or name in seen_effects:
                raise ValueError(f"invalid or repeated element effect {name!r}")
            for field in ("external", "reversible"):
                if not isinstance(effect.get(field, False), bool):
                    raise ValueError(f"element effect {name} {field} must be boolean")
            authority = _bounded_identifier(
                f"element effect {name} authority", effect.get("authority", ""),
                required=False,
            )
            if authority and not _CONTRACT_NAME.fullmatch(authority):
                raise ValueError(f"element effect {name} has invalid authority")
            if effect.get("external", False) and not authority:
                raise ValueError(f"external element effect {name} requires authority")
            if effect.get("external", False) and effect.get("reversible", False):
                raise ValueError(f"external element effect {name} cannot claim reversibility")
            seen_effects.add(name)

        fingerprint = descriptor.get("composite_fingerprint", "")
        _bounded_identifier(
            "element descriptor composite fingerprint", fingerprint, required=False,
        )
        if fingerprint:
            encoded = fingerprint.removeprefix("sha256:")
            if (
                not fingerprint.startswith("sha256:") or len(encoded) != 64
                or encoded != encoded.lower()
                or any(character not in "0123456789abcdef" for character in encoded)
            ):
                raise ValueError("element descriptor composite fingerprint is not canonical SHA-256")

    def _validate_capability(self, value: Any) -> dict[str, Any]:
        if not isinstance(value, dict):
            raise ValueError("resolved capability must be an object")
        unknown = set(value) - {"name", "contract", "provider", "adapter"}
        if unknown:
            raise ValueError(f"resolved capability has unknown fields {sorted(unknown)}")
        result = {
            "name": _bounded_identifier("capability name", value.get("name", "")),
            "provider": _validate_artifact("capability provider", value.get("provider")),
        }
        contract = _bounded_identifier(
            "capability contract", value.get("contract", ""), required=False,
        )
        if contract:
            result["contract"] = contract
        if value.get("adapter") is not None:
            result["adapter"] = _validate_artifact("capability adapter", value["adapter"])
        return result

    def _canonical_capabilities(self, values: list[dict[str, Any]]) -> list[dict[str, Any]]:
        result = [self._validate_capability(value) for value in values]
        result.sort(key=lambda item: (
            item["name"], item.get("contract", ""), item["provider"]["id"],
        ))
        seen: set[tuple[str, str, str]] = set()
        for capability in result:
            identity = (
                capability["name"], capability.get("contract", ""),
                capability["provider"]["id"],
            )
            if identity in seen:
                raise ValueError(f"resolved capability {capability['name']} is repeated")
            seen.add(identity)
        return result

    def _read_loop(self) -> None:
        while not self._worker_failure.is_set():
            try:
                message = read_message(self._input)
                if message is None:
                    return
                if message.type == MessageType.BYE:
                    if set(message.header) - {"type", "payload_bytes", "runtime_artifact"} or message.payload:
                        raise ValueError("v4 bye contains unrelated fields")
                    if message.get("runtime_artifact", {}) not in ({}, {"id": ""}):
                        raise ValueError("v4 bye cannot assert a runtime artifact")
                    return
                self._validate_element_frame(message, "input")
                port = message.get("port")
                envelope = copy.deepcopy(message.get("envelope"))
                payload = bytes(message.payload)
                if port in self._interrupts:
                    self.on_element_frame(port, envelope, payload)
                    continue
                try:
                    self._work.put_nowait((port, envelope, payload))
                except queue.Full as failure:
                    raise ValueError("v4 element worker queue is full") from failure
            except Exception as failure:  # noqa: BLE001
                log(traceback.format_exc())
                self.error(f"malformed v4 frame: {failure}", code="bad_frame", fatal=True)
                return

    def _work_loop(self) -> None:
        while True:
            item = self._work.get()
            if item is None:
                return
            port, envelope, payload = item
            try:
                self.on_element_frame(port, envelope, payload)
            except Exception as failure:  # noqa: BLE001
                log(traceback.format_exc())
                self.error(f"v4 element handler {port}: {failure}", code="handler_failed", fatal=True)
                self._worker_failure.set()
                return

    def _validate_element_frame(self, message: Message, direction: str) -> None:
        if message.type != MessageType.ELEMENT_FRAME:
            raise ValueError(f"v4 session received non-element frame {message.type!r}")
        unknown = set(message.header) - {
            "type", "payload_bytes", "port", "envelope", "runtime_artifact",
        }
        if unknown:
            raise ValueError(f"v4 element frame contains unrelated fields {sorted(unknown)}")
        if message.get("runtime_artifact", {}) not in ({}, {"id": ""}):
            raise ValueError("v4 element frame cannot assert a runtime artifact")
        port = _bounded_identifier("element frame port", message.get("port", ""))
        selection = self._selected.get((direction, port))
        if selection is None:
            raise ValueError(f"element frame uses unselected {direction} port {port!r}")
        envelope = message.get("envelope")
        if not isinstance(envelope, dict):
            raise ValueError("element frame requires an envelope object")
        unknown = set(envelope) - _ENVELOPE_FIELDS
        if unknown:
            raise ValueError(f"element envelope has unknown fields {sorted(unknown)}")
        if envelope.get("type") != selection.get("type"):
            raise ValueError(f"element frame port {port} has the wrong type")
        type_string(envelope.get("type"))
        _bounded_identifier("element envelope item_id", envelope.get("item_id", ""))
        for field in (
            "session_id", "source_id", "opportunity_id", "run_id", "trace_id",
            "cancellation_scope",
        ):
            _bounded_identifier(
                f"element envelope {field}", envelope.get(field, ""), required=False,
            )
        for field in ("sequence", "capture_ns", "receive_ns"):
            _bounded_int(f"element envelope {field}", envelope.get(field, 0), 2**64 - 1)
        parents = envelope.get("causal_parents") or []
        if not isinstance(parents, list) or len(parents) > MAX_CAUSAL_PARENTS:
            raise ValueError("element envelope causal parent set exceeds its bound")
        seen: set[str] = set()
        for parent in parents:
            value = _bounded_identifier("element envelope causal parent", parent)
            if value in seen:
                raise ValueError(f"element envelope repeats causal parent {value!r}")
            seen.add(value)

        wire_format = self._negotiated[(direction, port)]
        has_json = "json" in envelope
        if message.raw_header:
            found, raw_envelope = _raw_object_field(message.raw_header, "envelope")
            if not found:
                raise ValueError("element frame lost its raw envelope")
            raw_has_json, raw_json = _raw_object_field(raw_envelope, "json")
            if raw_has_json != has_json:
                raise ValueError("element frame JSON presence changed while parsing")
            json_size = len(raw_json)
        else:
            json_size = len(_compact_json_bytes(envelope.get("json"))) if has_json else 0
        binary_size = len(message.payload)
        mode = wire_format["payload_mode"]
        if mode == _PAYLOAD_JSON and (not has_json or binary_size):
            raise ValueError(f"element frame port {port} negotiated JSON-only payloads")
        if mode == _PAYLOAD_BINARY and (has_json or not binary_size):
            raise ValueError(f"element frame port {port} negotiated binary-only payloads")
        if mode == _PAYLOAD_JSON_BINARY and not has_json:
            raise ValueError(f"element frame port {port} requires JSON with its optional binary lane")
        if json_size > wire_format.get("max_json_bytes", 0) or binary_size > wire_format.get("max_binary_bytes", 0):
            raise ValueError(f"element frame port {port} exceeds its negotiated payload bounds")
        profile = wire_format.get("media")
        metadata = envelope.get("media")
        if profile is None and metadata is not None:
            raise ValueError(f"element frame port {port} did not negotiate media metadata")
        if metadata is not None and not binary_size:
            raise ValueError(f"element frame port {port} carries media metadata without binary data")
        if profile is not None and binary_size and metadata is None:
            raise ValueError(f"element frame port {port} requires media metadata for binary data")
        if profile is not None and binary_size:
            _validate_frame_media(metadata, profile)


def conformance_descriptor() -> dict[str, Any]:
    request_type = {"name": "Trigger", "arguments": [{"name": "sidecar.ConformanceRequest"}]}
    cancel_type = {"name": "Interrupt", "arguments": [{"name": "flow.RunID"}]}
    result_type = {"name": "Event", "arguments": [{"name": "sidecar.ConformanceResult"}]}
    return {
        "format_version": 1,
        "name": "sidecar.ConformanceProbe",
        "revision": 1,
        "ports": [
            {
                "name": "request", "direction": "input", "type": request_type,
                "cardinality": "one", "required": True, "min_connections": 1,
                "default_depth": 1,
            },
            {
                "name": "cancel", "direction": "input", "type": cancel_type,
                "cardinality": "one", "required": True, "min_connections": 1,
                "default_depth": 1,
            },
            {
                "name": "result", "direction": "output", "type": result_type,
                "cardinality": "one", "required": True, "min_connections": 1,
                "default_depth": 1,
            },
        ],
        "reaction": {
            "triggers": ["request"], "interrupts": ["cancel"], "outcomes": ["result"],
        },
    }


class ConformanceElementSidecar(ElementSidecar):
    """Bundled non-audio, cancellation-aware protocol-v4 probe."""

    element_descriptor = conformance_descriptor()
    runtime_artifact = {"id": "openrealtime/python-sidecar", "revision": "protocol-v4"}
    provider_artifact = {"id": "openrealtime/conformance-probe", "revision": "1"}
    adapter_artifact = {"id": "openrealtime/python-element-wire", "revision": "4"}

    def __init__(self, input_stream: BinaryIO, output_stream: BinaryIO) -> None:
        super().__init__(input_stream, output_stream)
        self._pending: dict[str, tuple[dict[str, Any], dict[str, Any]]] = {}
        self._pre_canceled: dict[str, dict[str, Any]] = {}
        self._state_lock = threading.Lock()

    def configure_element(self, config: Any) -> None:
        if config != {"mode": "conformance"}:
            raise ValueError("v4 conformance probe requires mode=conformance")

    def on_element_frame(self, port: str, envelope: dict[str, Any], payload: bytes) -> None:
        if payload:
            raise ValueError("conformance ports use JSON-only payloads")
        value = envelope.get("json")
        if port == "request":
            if not isinstance(value, dict) or set(value) - {"challenge", "wait_for_cancel", "hold_ms"}:
                raise ValueError("v4 conformance request has invalid fields")
            challenge = value.get("challenge")
            if not isinstance(challenge, str) or not challenge.strip():
                raise ValueError("v4 conformance request requires a challenge")
            if not value.get("wait_for_cancel", False):
                hold = value.get("hold_ms", 0)
                if not isinstance(hold, int) or isinstance(hold, bool) or not 0 <= hold <= 10_000:
                    raise ValueError("v4 conformance hold_ms must be an integer from 0 to 10000")
                # Occupies the work queue, so the suite can show that an
                # interrupt port is still served on the reader thread.
                time.sleep(hold / 1000)
                self._send_result(envelope, challenge, "ok", [envelope["item_id"]])
                return
            scope = envelope.get("cancellation_scope") or envelope.get("run_id")
            _bounded_identifier("pending cancellation scope", scope)
            with self._state_lock:
                if scope in self._pending:
                    raise ValueError(f"v4 conformance scope {scope} is already pending")
                if len(self._pending) >= MAX_QUEUED_FRAMES:
                    raise ValueError("v4 conformance pending set is full")
                canceled = self._pre_canceled.pop(scope, None)
                if canceled is None:
                    self._pending[scope] = (copy.deepcopy(envelope), copy.deepcopy(value))
                    return
            self._send_result(
                envelope, challenge, "canceled", [envelope["item_id"], canceled["item_id"]],
            )
            return
        if port == "cancel":
            if not isinstance(value, dict) or set(value) != {"reason"} or not isinstance(value["reason"], str) or not value["reason"].strip():
                raise ValueError("v4 conformance cancellation requires one reason")
            scope = envelope.get("cancellation_scope") or envelope.get("run_id")
            _bounded_identifier("cancellation scope", scope)
            with self._state_lock:
                pending = self._pending.pop(scope, None)
                if pending is None:
                    if len(self._pre_canceled) >= MAX_QUEUED_FRAMES:
                        raise ValueError("v4 conformance pre-cancellation set is full")
                    self._pre_canceled[scope] = copy.deepcopy(envelope)
                    return
            request, request_value = pending
            self._send_result(
                request, request_value["challenge"], "canceled",
                [request["item_id"], envelope["item_id"]],
            )
            return
        raise ValueError(f"v4 conformance received unexpected port {port!r}")

    def _send_result(
        self, request: dict[str, Any], challenge: str, status: str, parents: list[str],
    ) -> None:
        envelope = {
            "type": {"name": "Event", "arguments": [{"name": "sidecar.ConformanceResult"}]},
            "item_id": "python-conformance-result-" + request["item_id"],
            "run_id": request.get("run_id", ""),
            "sequence": 1,
            "causal_parents": parents,
            "json": {"challenge": challenge, "status": status},
        }
        for field in ("session_id", "trace_id", "cancellation_scope"):
            if request.get(field):
                envelope[field] = request[field]
        self.send_element_frame("result", envelope)

    def on_close(self) -> None:
        # The base class invokes this before draining its bounded worker so a
        # provider can cancel blocking work.  Keep the tiny conformance maps
        # intact until queued requests observe any pre-cancellation; the
        # entire fixture object is discarded immediately after the join.
        pass


def run_element(element_class: type[ElementSidecar], **kwargs: Any) -> None:
    """Serve one graph-native element over stdin/stdout."""
    element = element_class(sys.stdin.buffer, sys.stdout.buffer, **kwargs)
    element.run()
