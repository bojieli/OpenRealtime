#!/usr/bin/env python3
"""Verify native session release after disconnect during real audio output.

Uses the sidecar wire protocol and recorded input paced at wall-clock speed.
Packet arrival is not rendered playback. A reconnect is successful only when
both fresh sessions handshake and emit audio, not merely when TCP accepts.
"""
import argparse
import json
import socket
import threading
import time
from pathlib import Path

from native_probe import load_audio, pcm16, RATE, PACKET
from openrealtime_sidecar.protocol import read_message, write_message


def attempt(address, question, timeout):
    host, port = address.removeprefix('tcp:').rsplit(':', 1)
    started = time.monotonic()
    connection = socket.create_connection((host, int(port)), timeout=timeout)
    reader = connection.makefile('rb')
    writer = connection.makefile('wb', buffering=0)
    received = threading.Event()
    errors = []
    audio_bytes = 0
    thread = None
    try:
        write_message(writer, 'hello', version=1, sample_rate=RATE)
        ready = read_message(reader)
        if ready is None or ready.type != 'ready':
            raise RuntimeError(f'handshake failed: {ready}')
        handshake = time.monotonic()-started
        first_audio = None
        def receive():
            nonlocal audio_bytes, first_audio
            try:
                while True:
                    message = read_message(reader)
                    if message is None:
                        return
                    if message.type == 'error':
                        errors.append(message.header)
                        received.set()
                        return
                    if message.type == 'output_audio' and message.payload:
                        audio_bytes += len(message.payload)
                        if first_audio is None:
                            first_audio = time.monotonic()
                        received.set()
            except OSError as error:
                if not received.is_set():
                    errors.append(str(error))
                    received.set()
        thread = threading.Thread(target=receive, daemon=True)
        thread.start()
        import numpy as np
        began = time.monotonic()
        sent = 0
        while not received.is_set() and time.monotonic()-began < timeout:
            frame = question[sent:sent+PACKET]
            if len(frame) < PACKET:
                frame = np.pad(frame, (0, PACKET-len(frame)))
            write_message(writer, 'audio', pcm16(frame))
            sent += PACKET
            time.sleep(max(0, began+sent/RATE-time.monotonic()))
        result = {'handshake_s':handshake, 'input_sent_s':sent/RATE,
                  'first_audio_s':first_audio-began if first_audio else None,
                  'audio_bytes':audio_bytes, 'errors':errors,
                  'passed':bool(audio_bytes) and not errors}
        # Abrupt peer disconnect exercises EOF, not a cooperative model stop.
        return result
    finally:
        connection.shutdown(socket.SHUT_RDWR)
        if thread is not None:
            thread.join(2)
        for handle in (reader, writer, connection):
            handle.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--address', default='tcp:127.0.0.1:9147')
    parser.add_argument('--question', required=True)
    parser.add_argument('--timeout', type=float, default=30)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    question = load_audio(args.question)
    result = {'address':args.address, 'question':args.question,
              'timing_basis':'received PCM packets, not rendered playback', 'attempts':[]}
    try:
        for _ in range(2):
            row = attempt(args.address, question, args.timeout)
            result['attempts'].append(row)
            if not row['passed']:
                break
    except Exception as error:
        result['error'] = f'{type(error).__name__}: {error}'
    result['passed'] = len(result['attempts']) == 2 and all(r['passed'] for r in result['attempts']) and 'error' not in result
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(result, indent=2)+'\n')
    print(json.dumps(result, indent=2))
    raise SystemExit(0 if result['passed'] else 1)


if __name__ == '__main__':
    main()
