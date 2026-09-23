"""Configuration gating must not mistake socket acceptance for readiness."""
import json
from unittest.mock import patch

import pytest

from realtime_probe import wait_configured


class Socket:
    def __init__(self, events):
        self.events = iter(events)
        self.timeouts = []

    def recv(self, *, timeout):
        self.timeouts.append(timeout)
        return json.dumps(next(self.events))


def test_created_is_not_configuration_and_deadline_does_not_reset():
    ws = Socket([{"type": "session.created"}, {"type": "session.updated"}])
    with patch("realtime_probe.time.monotonic", side_effect=[0, 1, 2, 3, 4]):
        events = wait_configured(ws, 10)
    assert [e["event"]["type"] for e in events] == ["session.created", "session.updated"]
    assert ws.timeouts == [9, 7]


def test_configuration_error_does_not_start_replay():
    with pytest.raises(RuntimeError, match="configuration failed"):
        wait_configured(Socket([{"type": "error", "error": {"message": "rejected"}}]), 10)


def test_unrelated_events_cannot_extend_deadline():
    ws = Socket([{"type": "session.created"}])
    with patch("realtime_probe.time.monotonic", side_effect=[0, 1, 2, 11]):
        with pytest.raises(TimeoutError, match="readiness deadline"):
            wait_configured(ws, 10)
    assert len(ws.timeouts) == 1
