import socket
import threading

import numpy as np
import pytest

from native_reconnect_probe import AudioActivity, attempt
from native_probe import PACKET, pcm16
from openrealtime_sidecar.protocol import read_message, write_message


def test_activity_ignores_silence_and_is_independent_of_packet_boundaries():
    silence = pcm16(np.zeros(PACKET * 10))
    tone = pcm16(.1 * np.sin(np.arange(PACKET * 5) * 2 * np.pi / 120))
    for size in (1, 137, len(tone)):
        activity = AudioActivity()
        assert not activity.push(silence)
        for start in range(0, len(tone), size):
            activity.push(tone[start:start + size])
        assert activity.detected
    activity = AudioActivity()
    assert not activity.push(tone[:PACKET * 8])  # 80 ms
    assert not activity.push(silence)
    assert not activity.push(tone[:PACKET * 8])


@pytest.mark.parametrize('active', [False, True])
def test_attempt_requires_activity_after_handshake(active):
    listener = socket.socket()
    listener.bind(('127.0.0.1', 0))
    listener.listen()
    failures = []

    def serve():
        try:
            with listener, listener.accept()[0] as connection:
                with connection.makefile('rb') as reader, connection.makefile('wb', buffering=0) as writer:
                    assert read_message(reader).type == 'hello'
                    write_message(writer, 'ready', version=1, sample_rate=24000)
                    assert read_message(reader).type == 'audio'
                    samples = np.zeros(PACKET * 5)
                    if active:
                        samples = .1 * np.sin(np.arange(PACKET * 5) * 2 * np.pi / 120)
                    write_message(writer, 'output_audio', pcm16(samples))
                    while read_message(reader) is not None:
                        pass
        except Exception as error:
            failures.append(error)

    thread = threading.Thread(target=serve, daemon=True)
    address = f'tcp:127.0.0.1:{listener.getsockname()[1]}'
    thread.start()
    result = attempt(address, np.zeros(PACKET), .2)
    thread.join(2)
    assert not thread.is_alive()
    assert not failures
    assert result['audio_bytes'] > 0
    assert result['passed'] is active
