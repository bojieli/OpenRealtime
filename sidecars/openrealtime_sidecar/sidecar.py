"""The base class every reference sidecar builds on."""

from __future__ import annotations

import queue
import sys
import threading
import traceback
from typing import Any, BinaryIO

from .protocol import (
    VERSION,
    Message,
    MessageType,
    log,
    read_message,
    write_message,
)


class Sidecar:
    """A model server that speaks the sidecar protocol.

    Subclasses implement the model. This class owns the protocol: reading
    frames, the handshake, the write lock, and turning an exception into an
    error frame rather than a dead process.

    Reading and generating run on separate threads because they must: the
    engine can interrupt a turn while the model is still producing it, and a
    sidecar that only checked for messages between turns could not be
    interrupted at all.
    """

    #: Model identity, reported at the handshake so evidence can name what
    #: produced a result.
    model_name = "unnamed"
    #: Sample rate of the audio this sidecar emits.
    output_rate = 24_000
    #: What this sidecar can do. The engine supplies whatever is missing.
    capabilities: tuple[str, ...] = ()

    def __init__(self, input_stream: BinaryIO, output_stream: BinaryIO) -> None:
        self._input = input_stream
        self._output = output_stream
        self._write_lock = threading.Lock()
        self._interrupted = threading.Event()
        self._work: queue.Queue[Message | None] = queue.Queue()
        self.instructions = ""
        self.voice = ""
        self.input_rate = 24_000
        self.tools: list[dict[str, Any]] = []

    # --- protocol -----------------------------------------------------------

    def send(self, message_type: str, payload: bytes = b"", **header: Any) -> None:
        """Send one frame. Safe to call from any thread."""
        with self._write_lock:
            write_message(self._output, message_type, payload, **header)

    def transcript(self, text: str, *, final: bool = True) -> None:
        self.send(MessageType.TRANSCRIPT, text=text, final=final)

    def text_delta(self, text: str) -> None:
        self.send(MessageType.TEXT_DELTA, text=text)

    def text_done(self, text: str) -> None:
        self.send(MessageType.TEXT_DONE, text=text)

    def audio(self, pcm16: bytes) -> None:
        if pcm16:
            self.send(MessageType.OUTPUT_AUDIO, pcm16)

    def turn_done(self) -> None:
        self.send(MessageType.TURN_DONE)

    def error(self, message: str, *, code: str = "", fatal: bool = False) -> None:
        self.send(MessageType.ERROR, text=message, code=code or None, fatal=fatal or None)

    def interrupted(self) -> bool:
        """Whether the engine asked this turn to stop.

        Generation loops should check it between chunks. Cooperative stopping
        is the only kind available: nothing here can safely kill a model
        mid-forward-pass.
        """
        return self._interrupted.is_set()

    # --- lifecycle ----------------------------------------------------------

    def configure(self, hello: Message) -> None:
        """Apply the session configuration. Override to load a model."""

    def on_audio(self, pcm16: bytes) -> None:
        """Receive input audio. Override in a sidecar that listens."""

    def on_commit(self) -> None:
        """The client declared its turn over rather than waiting for silence.

        Only meaningful to a model that owns its own floor: it is a signal that
        the person has stopped, from the side that knows. A model whose floor
        the engine keeps has nothing to do here - the engine ended the turn
        before sending this - so the default is to ignore it.
        """

    def on_text(self, text: str, role: str) -> None:
        """Receive injected text.

        This is how a background reasoner's completed answer reaches a model
        that owns its own voice. A sidecar that declares no text-injection
        capability may ignore it.
        """

    def on_respond(self) -> None:
        """Produce one turn. Override in every sidecar."""
        raise NotImplementedError

    def on_tool_result(self, message: Message) -> None:
        """Receive the outcome of a call this model requested."""

    def on_close(self) -> None:
        """Release the model."""

    # --- run loop -----------------------------------------------------------

    def run(self) -> None:
        """Serve until the stream ends."""
        hello = read_message(self._input)
        if hello is None or hello.type != MessageType.HELLO:
            self.error("the first frame must be hello", fatal=True)
            return
        if int(hello.get("version", 0)) != VERSION:
            # A model that half-understands the protocol is worse than one
            # that does not start.
            self.error(
                f"this sidecar speaks protocol version {VERSION}, "
                f"the engine speaks {hello.get('version')}",
                code="version_mismatch",
                fatal=True,
            )
            return
        self.input_rate = int(hello.get("sample_rate", 24_000))
        self.instructions = str(hello.get("instructions", ""))
        self.voice = str(hello.get("voice", ""))
        self.tools = list(hello.get("tools") or [])
        try:
            self.configure(hello)
        except Exception as failure:  # noqa: BLE001 - reported, not swallowed
            log(traceback.format_exc())
            self.error(f"configure: {failure}", code="configure_failed", fatal=True)
            return
        self.send(
            MessageType.READY,
            version=VERSION,
            model=self.model_name,
            output_rate=self.output_rate,
            capabilities=list(self.capabilities) or None,
        )

        worker = threading.Thread(target=self._work_loop, daemon=True)
        worker.start()
        try:
            self._read_loop()
        finally:
            self._work.put(None)
            worker.join(timeout=10)
            try:
                self.on_close()
            except Exception:  # noqa: BLE001
                log(traceback.format_exc())

    def _read_loop(self) -> None:
        while True:
            try:
                message = read_message(self._input)
            except Exception as failure:  # noqa: BLE001
                log(traceback.format_exc())
                self.error(f"malformed frame: {failure}", code="bad_frame", fatal=True)
                return
            if message is None or message.type == MessageType.BYE:
                return
            if message.type == MessageType.AUDIO:
                # Audio is handled inline: it must not queue behind a turn that
                # is still generating, or the model would hear the past.
                try:
                    self.on_audio(message.payload)
                except Exception as failure:  # noqa: BLE001
                    log(traceback.format_exc())
                    self.error(f"audio: {failure}", code="audio_failed")
                continue
            if message.type == MessageType.INTERRUPT:
                self._interrupted.set()
                continue
            self._work.put(message)

    def _work_loop(self) -> None:
        while True:
            message = self._work.get()
            if message is None:
                return
            try:
                if message.type == MessageType.RESPOND:
                    self._interrupted.clear()
                    self.on_respond()
                    if not self.interrupted():
                        self.turn_done()
                    else:
                        # An interrupted turn still ends: the engine is waiting
                        # for a boundary, not for completion.
                        self.turn_done()
                elif message.type == MessageType.COMMIT:
                    self.on_commit()
                elif message.type == MessageType.TEXT:
                    self.on_text(message.text, str(message.get("role", "user")))
                elif message.type == MessageType.TOOL_RESULT:
                    self.on_tool_result(message)
            except Exception as failure:  # noqa: BLE001
                log(traceback.format_exc())
                self.error(f"{message.type}: {failure}", code="turn_failed")
                if message.type == MessageType.RESPOND:
                    self.turn_done()


def run(sidecar_class: type[Sidecar], **kwargs: Any) -> None:
    """Serve one sidecar over stdin and stdout."""
    sidecar = sidecar_class(sys.stdin.buffer, sys.stdout.buffer, **kwargs)
    sidecar.run()
