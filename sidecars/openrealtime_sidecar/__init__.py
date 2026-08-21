"""The OpenRealtime sidecar protocol, for Python model servers.

A sidecar is a model that is not written in Go, running behind a process
boundary with a documented, versioned protocol between it and the engine.
Keeping it there is what stops Python from entering the engine's build, test,
and analysis path.

This package is the framing and a small base class. Everything model-specific
lives in the sidecar that subclasses it, and the protocol is small enough that
a new one is a few hundred lines.
"""

from .protocol import (
    VERSION,
    Capability,
    Message,
    MessageType,
    log,
    read_message,
    write_message,
)
from .sidecar import Sidecar, run

__all__ = [
    "VERSION",
    "Capability",
    "Message",
    "MessageType",
    "Sidecar",
    "log",
    "read_message",
    "run",
    "write_message",
]
