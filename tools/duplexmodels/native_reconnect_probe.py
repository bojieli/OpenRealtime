#!/usr/bin/env python3
"""Verify native session release after disconnect during real audio output.

Uses the sidecar wire protocol and recorded input paced at wall-clock speed.
Packet arrival is not rendered playback. A reconnect is successful only when
both fresh sessions handshake and emit at least 100 ms of consecutive audio
above -40 dBFS RMS, not merely when TCP accepts or silent PCM arrives.
This energy criterion does not establish intelligibility or answer correctness.
"""
import argparse
import json
import socket
import shlex
import subprocess
import threading
import time
from pathlib import Path

import numpy as np

from native_probe import load_audio, pcm16, RATE, PACKET
from openrealtime_sidecar.protocol import read_message, write_message


class AudioActivity:
    """Packet-independent 20 ms RMS windows; require five active windows."""
    def __init__(self, rate=RATE):
        if rate <= 0 or rate % 50:
            raise ValueError('output rate must support integral 20 ms windows')
        self.window_bytes = rate // 50 * 2
        self.pending = b''
        self.consecutive = 0
        self.detected = False

    def push(self, payload):
        self.pending += payload
        window_bytes = self.window_bytes
        while len(self.pending) >= window_bytes:
            samples = np.frombuffer(self.pending[:window_bytes], dtype='<i2').astype(np.float64) / 32768
            self.pending = self.pending[window_bytes:]
            active = np.sqrt(np.mean(samples * samples)) >= 0.01
            self.consecutive = self.consecutive + 1 if active else 0
            self.detected |= self.consecutive >= 5
        return self.detected


def attempt(address, question, timeout, *, sidecar=None, stderr=None):
    started = time.monotonic()
    process = None
    if sidecar:
        connection, child = socket.socketpair()
        try:
            process = subprocess.Popen(shlex.split(sidecar), stdin=child, stdout=child,
                                       stderr=stderr)
        except BaseException:
            connection.close()
            raise
        finally:
            child.close()
        connection.settimeout(timeout)
    else:
        host, port = address.removeprefix('tcp:').rsplit(':', 1)
        connection = socket.create_connection((host, int(port)), timeout=timeout)
    reader = connection.makefile('rb')
    writer = connection.makefile('wb', buffering=0)
    received = threading.Event()
    errors = []
    audio_bytes = 0
    activity = AudioActivity()
    thread = None
    result = None
    try:
        write_message(writer, 'hello', version=1, sample_rate=RATE)
        ready = read_message(reader)
        if ready is None or ready.type != 'ready':
            raise RuntimeError(f'handshake failed: {ready}')
        output_rate = int(ready.header.get('output_rate', RATE))
        activity = AudioActivity(output_rate)
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
                        if activity.push(message.payload):
                            received.set()
            except OSError as error:
                if not received.is_set():
                    errors.append(str(error))
                    received.set()
        thread = threading.Thread(target=receive, daemon=True)
        thread.start()
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
                  'active_audio_detected': activity.detected, 'output_rate': output_rate,
                  'passed':activity.detected and not errors}
        # Abrupt peer disconnect exercises EOF, not a cooperative model stop.
        return result
    finally:
        try:
            connection.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass  # The peer may have already disconnected.
        if thread is not None:
            thread.join(2)
        for handle in (reader, writer, connection):
            handle.close()
        if process is not None:
            try:
                process.wait(timeout=5)
                if result is not None:
                    result['sidecar_exit_code'] = process.returncode
                    if process.returncode != 0:
                        result['passed'] = False
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                if result is not None:
                    result['errors'].append('sidecar did not exit within 5 s of disconnect')
                    result['passed'] = False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    transport = parser.add_mutually_exclusive_group()
    transport.add_argument('--address')
    transport.add_argument('--sidecar', help='spawn this stdio sidecar command for each session')
    parser.add_argument('--question', required=True)
    parser.add_argument('--timeout', type=float, default=30)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    if not args.address and not args.sidecar:
        args.address = 'tcp:127.0.0.1:9147'
    question = load_audio(args.question)
    result = {'address':args.address, 'sidecar':args.sidecar, 'question':args.question,
              'timing_basis':'received PCM packets, not rendered playback',
              'activity_criterion': {'rms_dbfs': -40, 'consecutive_ms': 100,
                                     'window_ms': 20, 'sample_rate': 'negotiated output_rate'},
              'attempts':[]}
    try:
        for _ in range(2):
            row = attempt(args.address, question, args.timeout, sidecar=args.sidecar)
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
