"""What this server promises about the audio it hands back.

These are here because the failure they cover is invisible. A sample rate the
caller has to guess, or a RIFF header where a caller expected samples, does
not raise anything anywhere: the far end plays the bytes at the wrong speed,
a speech model hears an accent it cannot place, and the transcript is empty
while every layer reports success. Measured once already, on tau2-bench,
where an agent sat silent for sixty-eight seconds against speech delivered at
1.84x too slow.
"""

import math
import struct
import sys
from pathlib import Path

import numpy
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))

from audio_contract import (  # noqa: E402
    SAMPLE_RATE,
    Resampler,
    requested_audio,
    wav_file,
)


def tone(rate, seconds, hertz=440.0):
    """A pure tone, which survives a resample and does not survive a wrong one."""
    t = numpy.arange(int(rate * seconds)) / rate
    return (numpy.sin(2 * math.pi * hertz * t) * 16000).astype(numpy.int16)


def dominant_hertz(samples, rate):
    window = samples.astype(numpy.float64) * numpy.hanning(len(samples))
    spectrum = numpy.abs(numpy.fft.rfft(window))
    return numpy.fft.rfftfreq(len(samples), 1.0 / rate)[spectrum.argmax()]


class TestFormatNegotiation:
    def test_pcm_is_asked_for_by_name(self):
        assert requested_audio({"response_format": "pcm"}, {}) == ("pcm", SAMPLE_RATE)

    def test_the_go_adapter_still_gets_wav(self):
        # It names wav in an Accept header and sends no response_format.
        headers = {"Accept": "audio/wav, application/octet-stream"}
        assert requested_audio({}, headers) == ("wav", SAMPLE_RATE)

    def test_an_accept_header_naming_only_pcm_is_honoured(self):
        assert requested_audio({}, {"Accept": "audio/pcm"})[0] == "pcm"

    def test_wav_is_the_default_for_a_caller_that_says_nothing(self):
        assert requested_audio({}, {}) == ("wav", SAMPLE_RATE)

    def test_a_rate_may_be_asked_for(self):
        body = {"response_format": "pcm", "sample_rate": 24000}
        assert requested_audio(body, {}) == ("pcm", 24000)

    def test_a_format_this_server_cannot_produce_is_refused(self):
        # Refused rather than silently answered as wav: a caller expecting mp3
        # and handed a RIFF file gets noise, which is the failure mode these
        # tests exist to stop.
        with pytest.raises(ValueError):
            requested_audio({"response_format": "mp3"}, {})

    def test_a_nonsense_rate_is_refused(self):
        with pytest.raises(ValueError):
            requested_audio({"sample_rate": 0}, {})
        with pytest.raises(ValueError):
            requested_audio({"sample_rate": "soon"}, {})


class TestConversion:
    def test_the_wire_rate_is_reached_and_the_pitch_survives(self):
        # 44.1kHz in, 24kHz out: the rate this path actually needed, and the
        # one it was silently not doing.
        source = tone(SAMPLE_RATE, 0.5)
        converted = Resampler(SAMPLE_RATE, 24_000).push(source.tobytes(), final=True)
        samples = numpy.frombuffer(converted, dtype=numpy.int16)
        expected = round(len(source) * 24_000 / SAMPLE_RATE)
        assert abs(len(samples) - expected) <= 1, "duration must survive the conversion"
        assert abs(dominant_hertz(samples, 24_000) - 440.0) < 5.0, (
            "a tone that changes pitch means the audio is playing at the wrong speed, "
            "which is exactly what a mis-declared rate does to a speech model")

    def test_an_unconverted_stream_is_handed_straight_back(self):
        source = tone(SAMPLE_RATE, 0.1).tobytes()
        assert Resampler(SAMPLE_RATE, SAMPLE_RATE).push(source, final=True) == source

    def test_chunks_convert_to_the_same_audio_as_one_pass(self):
        # The seam this guards is a click at every chunk boundary: a filter
        # given no neighbours at the edge of a chunk invents them.
        source = tone(SAMPLE_RATE, 0.4)
        whole = Resampler(SAMPLE_RATE, 24_000).push(source.tobytes(), final=True)

        streamed, resampler = b"", Resampler(SAMPLE_RATE, 24_000)
        for start in range(0, len(source), 1024):
            streamed += resampler.push(source[start:start + 1024].tobytes())
        streamed += resampler.push(b"", final=True)

        assert len(streamed) == len(whole), "streaming must not change the duration"
        a = numpy.frombuffer(streamed, dtype=numpy.int16).astype(numpy.float64)
        b = numpy.frombuffer(whole, dtype=numpy.int16).astype(numpy.float64)
        assert numpy.max(numpy.abs(a - b)) < 0.02 * 16000, (
            "a chunked conversion must be the same audio as one pass")

    def test_a_stream_holds_back_what_it_cannot_yet_convert(self):
        # Nothing is emitted from a chunk too short to have neighbours, and
        # nothing is lost either: the final flush accounts for all of it.
        resampler = Resampler(SAMPLE_RATE, 24_000)
        assert resampler.push(tone(SAMPLE_RATE, 0.001).tobytes()) == b""
        assert len(resampler.push(b"", final=True)) > 0

    def test_an_empty_utterance_converts_to_nothing(self):
        assert Resampler(SAMPLE_RATE, 24_000).push(b"", final=True) == b""


class TestContainer:
    def test_a_riff_file_declares_the_rate_it_was_written_at(self):
        # The header is what a caller reads to find the rate; writing the
        # model's rate into a file converted to another is the same lie told
        # in a different place.
        payload = wav_file(b"\x00\x00" * 100, 24_000)
        assert payload[:4] == b"RIFF" and payload[8:12] == b"WAVE"
        assert struct.unpack("<I", payload[24:28])[0] == 24_000
        assert struct.unpack("<I", payload[40:44])[0] == 200
