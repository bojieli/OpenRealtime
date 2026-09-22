#!/usr/bin/env python3
"""Audit upstream VoiceChat response boundaries without inventing speech ends.

Consumes --event-log JSONL from voicechat_sidecar.py, not the native probe's
engine-facing event trace. Audio energy is diagnostic; it never closes a turn.
"""
import argparse
import base64
import json
import math
import struct
from pathlib import Path


def audit(path):
    responses = {}
    for line in path.read_text().splitlines():
        record = json.loads(line)
        if record.get('dir') != 'in':
            continue
        event = record.get('event', {})
        identity = event.get('response_id') or event.get('response', {}).get('id')
        if not identity:
            continue
        row = responses.setdefault(identity, {'text': '', 'audio_packets': 0, 'audio_seconds': 0.0,
                                               'done_at': None, 'last_audible_at': None})
        time = record['t']
        kind = event.get('type')
        if kind == 'response.created':
            row['created_at'] = time
        elif kind == 'response.done':
            row['done_at'] = time
        elif kind == 'response.output_audio_transcript.delta':
            row['text'] += event.get('delta', '')
            if event.get('delta'):
                row['last_text_at'] = time
        elif kind == 'response.output_audio.delta':
            row['audio_packets'] += 1
            row['last_audio_at'] = time
            if event['delta'].startswith('<'):
                row['audio_payload_redacted'] = True
                row['audio_seconds'] = None
                continue
            pcm = base64.b64decode(event['delta'], validate=True)
            samples = [sample[0] for sample in struct.iter_unpack('<h', pcm)]
            rms = math.sqrt(sum(sample * sample for sample in samples) / max(1, len(samples))) / 32768
            row['audio_seconds'] = (row['audio_seconds'] or 0) + len(samples) / int(event.get('sample_rate_hz') or 22050)
            if rms >= 0.01:
                row['last_audible_at'] = time
    return {'source': str(path), 'timing_basis': 'upstream packet arrival; RMS threshold 0.01 is diagnostic only',
            'responses': responses, 'unterminated': [key for key, row in responses.items() if row['done_at'] is None]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('trace', type=Path)
    parser.add_argument('--out', type=Path)
    args = parser.parse_args()
    output = json.dumps(audit(args.trace), indent=2) + '\n'
    if args.out:
        args.out.write_text(output)
    else:
        print(output, end='')


if __name__ == '__main__':
    main()
