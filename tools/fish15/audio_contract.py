"""The audio contract this server answers on: container, rate, and conversion.

It is separate from the server because it is the part that can be wrong
silently. A model that fails to load says so; a rate that is off by a factor
of 1.8 says nothing at all - the far end hears speech at the wrong speed,
transcribes none of it, and every layer reports success. So this half is kept
free of the checkpoint and the GPU, and is tested on its own.
"""

import math
import struct

import numpy
import torch
import torchaudio

# What the model decodes at. Everything else is a conversion away from it.
SAMPLE_RATE = 44_100
AMPLITUDE = 32768


def wav_file(pcm, sample_rate=SAMPLE_RATE, channels=1, bits=16):
    """A complete RIFF file, for a caller that wanted the whole utterance."""
    block_align = channels * bits // 8
    return (
        b"RIFF"
        + struct.pack("<I", 36 + len(pcm))
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 1, channels, sample_rate,
                      sample_rate * block_align, block_align, bits)
        + b"data"
        + struct.pack("<I", len(pcm))
        + pcm
    )


def wav_header(sample_rate=SAMPLE_RATE, channels=1, bits=16):
    """A RIFF header for a stream of unknown length.

    Streaming servers cannot know the total size in advance, so the two length
    fields carry the conventional placeholder.  The Go adapter ignores them.
    """
    block_align = channels * bits // 8
    return (
        b"RIFF"
        + struct.pack("<I", 0xFFFFFFFF)
        + b"WAVEfmt "
        + struct.pack("<IHHIIHH", 16, 1, channels, sample_rate,
                      sample_rate * block_align, block_align, bits)
        + b"data"
        + struct.pack("<I", 0xFFFFFFFF)
    )


class Resampler:
    """Converts a stream of PCM16 from one rate to another, without seams.

    The model decodes at one rate and callers need another. A caller that is
    told the wrong rate does not fail: it plays the same samples at the wrong
    speed, which a speech model on the far end hears as an accent it cannot
    transcribe, and nothing anywhere reports an error. So the conversion is
    done here, where the true rate is known, rather than inferred downstream.

    torchaudio's resample is the standard band-limited sinc converter and is
    stateless, which is what makes streaming it delicate: a filter needs
    samples on both sides of the ones it is producing, and a chunk resampled
    on its own has neither at its edges. Splicing those pieces gives a click
    at every boundary. This carries `context` input samples of history and
    holds back the same amount of lookahead, so each output sample is computed
    from the same neighbourhood it would have had in one whole pass. The
    context is a whole number of down-steps, which keeps the output offset an
    exact sample count and stops the two rates drifting apart over a long
    utterance.
    """

    def __init__(self, source_rate, target_rate):
        self.source_rate = source_rate
        self.target_rate = target_rate
        divisor = math.gcd(source_rate, target_rate)
        self.up = target_rate // divisor
        self.down = source_rate // divisor
        # Enough input either side to cover the filter, rounded up to a whole
        # number of down-steps so the matching output offset is exact.
        self.context = self.down * max(1, -(-256 // self.down))
        self.history = numpy.zeros(0, dtype=numpy.int16)
        self.pending = numpy.zeros(0, dtype=numpy.int16)

    def push(self, pcm, final=False):
        """Convert what can be converted now; return PCM16 bytes."""
        if self.up == self.down:
            return pcm
        self.pending = numpy.concatenate(
            [self.pending, numpy.frombuffer(pcm, dtype=numpy.int16)])
        if final:
            take = len(self.pending)
        else:
            # Hold back `context` samples: they are this block's lookahead.
            take = max(0, len(self.pending) - self.context) // self.down * self.down
        if take == 0 and not (final and len(self.pending)):
            return b""
        block = self.pending[:take]
        lookahead = self.pending[take:take + self.context]
        buffer = numpy.concatenate([self.history, block, lookahead])
        converted = self.convert(buffer)
        head = len(self.history) // self.down * self.up
        # Mid-stream, emit whole down-steps only, so the next block starts on
        # a boundary and the two rates never drift. At the end there is no
        # next block, and the remainder - a fraction of a step, but a real
        # fraction of a short utterance - is all that is left to hand over.
        want = len(converted) - head if final else take // self.down * self.up
        self.history = numpy.concatenate([self.history, block])[-self.context:]
        self.pending = self.pending[take:]
        return converted[head:head + want].tobytes()

    def convert(self, samples):
        waveform = torch.from_numpy(samples.astype("float32") / AMPLITUDE)
        converted = torchaudio.functional.resample(
            waveform, self.source_rate, self.target_rate)
        return (converted.clamp(-1.0, 1.0) * AMPLITUDE).round().to(
            torch.int16).numpy()


def requested_audio(body, headers):
    """What the caller asked to be given: a container, and a rate.

    OpenAI's speech API names the container `response_format`; the Go adapter
    here says it in an Accept header instead. Either is answered. The rate is
    this server's own field, and it exists because the alternative - letting
    the caller assume one - is the failure this whole path was written to
    stop.
    """
    container = str(body.get("response_format") or body.get("format") or "").lower()
    if not container:
        accept = (headers.get("Accept") or "").lower()
        # audio/wav is also what a browser sends as */*, so only a caller
        # naming PCM and nothing else is taken to mean raw.
        container = "pcm" if "audio/pcm" in accept and "audio/wav" not in accept else "wav"
    if container in ("pcm", "raw", "pcm16", "audio/pcm"):
        container = "pcm"
    elif container in ("wav", "wave", "audio/wav"):
        container = "wav"
    else:
        raise ValueError(f"unsupported response_format {container!r}; use wav or pcm")

    # Asked for by presence, not by truth: `or` would read a zero as absent
    # and quietly answer at the model's rate, which is the class of mistake
    # this function exists to refuse.
    rate = body.get("sample_rate", body.get("sample_rate_hz"))
    if rate is None:
        rate = SAMPLE_RATE
    try:
        rate = int(rate)
    except (TypeError, ValueError) as error:
        raise ValueError(f"sample_rate must be a whole number of hertz") from error
    if rate <= 0:
        raise ValueError("sample_rate must be positive")
    return container, rate
