"""Measure where the embedder itself separates one voice from another.

The threshold that decides whether two utterances are the same person has to
come from somewhere. Taking it from the benchmark's own voices makes the
benchmark's answer a property of the benchmark, which is the one thing a
measurement must not be.

This takes it from the embedder instead, on whatever audio it is pointed at,
using a property that needs no labels: the two halves of one recording are the
same speaker by construction, and two different recordings from a corpus of
unrelated ones usually are not. That gives a same-speaker distribution and a
different-speaker distribution without anybody labelling anything, and the
threshold is the midpoint of the gap between them.

Run it on any corpus. The number it prints is a fact about the embedder and
that corpus, and re-running it on a different corpus is how you find out
whether the number travels.
"""

import argparse
import itertools
import json
import statistics
import struct
import sys
import urllib.request
import wave


def embed(endpoint, pcm, rate):
    request = urllib.request.Request(
        endpoint, data=pcm,
        headers={"Content-Type": "application/octet-stream", "X-Sample-Rate": str(rate)},
    )
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read())["embedding"]


def read(path):
    with wave.open(path, "rb") as handle:
        if handle.getnchannels() != 1 or handle.getsampwidth() != 2:
            raise ValueError(f"{path}: expected 16-bit mono")
        return handle.readframes(handle.getnframes()), handle.getframerate()


def cosine(left, right):
    return sum(a * b for a, b in zip(left, right))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("audio", nargs="+", help="mono 16-bit WAV files, one speaker each")
    parser.add_argument("--endpoint", default="http://127.0.0.1:8124/embed")
    arguments = parser.parse_args()

    halves, wholes = {}, {}
    for path in arguments.audio:
        pcm, rate = read(path)
        middle = len(pcm) // 4 * 2  # keep the split on a sample boundary
        if middle < rate:  # under a second a side is not a voice, it is a vowel
            print(f"  skipping {path}: too short to halve", file=sys.stderr)
            continue
        halves[path] = (embed(arguments.endpoint, pcm[:middle], rate),
                        embed(arguments.endpoint, pcm[middle:], rate))
        wholes[path] = embed(arguments.endpoint, pcm, rate)

    same = [cosine(first, second) for first, second in halves.values()]
    different = [cosine(wholes[left], wholes[right])
                 for left, right in itertools.combinations(wholes, 2)]
    if not same or not different:
        print("not enough audio to calibrate", file=sys.stderr)
        return 1

    print(f"same speaker      n={len(same):3d}  min {min(same):+.3f}  "
          f"median {statistics.median(same):+.3f}  max {max(same):+.3f}")
    print(f"different speakers n={len(different):3d}  min {min(different):+.3f}  "
          f"median {statistics.median(different):+.3f}  max {max(different):+.3f}")
    gap = min(same) - max(different)
    if gap <= 0:
        if statistics.median(different) > statistics.median(same):
            # Not a failure of the embedder. Cross-recording similarity above
            # within-recording similarity means the recordings are one person,
            # and the halves score lower only because they are shorter and a
            # shorter segment embeds more noisily. The corpus can calibrate the
            # same-speaker side and says nothing about the other one.
            print("\nevery recording here looks like the same speaker: "
                  "cross-recording similarity is above within-recording.")
            print("the same-speaker side is usable; find a corpus with more "
                  "than one voice in it for the other side.")
            return 1
        print("\nthe distributions overlap: no threshold separates them on this corpus")
        return 1
    print(f"\ngap {gap:+.3f}; midpoint threshold {(min(same) + max(different)) / 2:.2f}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
