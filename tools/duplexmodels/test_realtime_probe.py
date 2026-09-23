"""Configuration gating must not mistake socket acceptance for readiness."""
import json
import sys
import threading
import wave
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


@pytest.mark.parametrize("gated", [False, True])
def test_replay_gate_over_real_websocket(tmp_path, monkeypatch, gated):
    from websockets.exceptions import ConnectionClosed
    from websockets.sync.server import serve
    from realtime_probe import main

    observed = []
    completed = threading.Event()

    def handler(ws):
        try:
            observed.append(json.loads(ws.recv())["type"])
            ws.send(json.dumps({"type": "session.created"}))
            try:
                observed.append(json.loads(ws.recv(timeout=0.15))["type"])
            except TimeoutError:
                observed.append("no_audio_before_ack")
            ws.send(json.dumps({"type": "session.updated"}))
            for message in ws:
                observed.append(json.loads(message)["type"])
        except ConnectionClosed:
            pass
        finally:
            completed.set()

    wav = tmp_path / "input.wav"
    with wave.open(str(wav), "wb") as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(24000)
        audio.writeframes(b"\0\0" * 960)
    output = tmp_path / "events.jsonl"
    with serve(handler, "127.0.0.1", 0) as server:
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            port = server.socket.getsockname()[1]
            arguments = ["probe", "--endpoint", f"ws://127.0.0.1:{port}",
                         "--wav", str(wav), "--lead", "0", "--duration", "0.3",
                         "--out", str(output)]
            if gated:
                arguments.append("--wait-configured")
            monkeypatch.setattr(sys, "argv", arguments)
            main()
            assert completed.wait(2)
        finally:
            server.shutdown()
            thread.join(2)
    assert observed[0] == "session.update"
    assert observed[1] == ("no_audio_before_ack" if gated else "input_audio_buffer.append")
    assert "input_audio_buffer.append" in observed[2:]
    events = [json.loads(line) for line in output.read_text().splitlines()]
    start = events[0]
    assert start["wait_configured"] == gated
    if gated:
        assert start["connected_to_replay_ms"] >= 140
        assert any(e["type"] == "session.updated" and e["phase"] == "configuration"
                   and e["t"] <= 0 for e in events)
