import numpy as np
import pytest
import os

from causal_resample import FilterResampler


@pytest.mark.parametrize("rate", [16000, 24000, 48000])
def test_packet_boundaries_and_impulse_delay(rate):
    signal = np.random.default_rng(4).normal(size=rate)
    whole, split = FilterResampler(rate), FilterResampler(rate)
    expected = whole.down(whole.up(signal))
    expanded = np.concatenate([split.up(x) for x in np.array_split(signal, 137)])
    actual = np.concatenate([split.down(x) for x in np.array_split(expanded, 311)])
    np.testing.assert_allclose(actual, expected, atol=1e-12, rtol=0)
    impulse = np.zeros(rate // 10)
    impulse[0] = 1
    resampler = FilterResampler(rate)
    result = resampler.down(resampler.up(impulse))
    assert np.argmax(abs(result)) == rate // 500  # exactly 2 ms round trip
    assert len(result) == len(impulse)


@pytest.mark.parametrize("rate", [16000, 24000, 48000])
def test_six_khz_passband_and_causality(rate):
    signal = np.sin(2 * np.pi * 6000 * np.arange(rate) / rate)
    resampler = FilterResampler(rate)
    output = resampler.down(resampler.up(signal))
    gain_db = 20 * np.log10(np.sqrt(np.mean(output[rate//10:] ** 2)) * np.sqrt(2))
    assert abs(gain_db) < 0.1
    prefix = FilterResampler(rate)
    prefix_output = prefix.down(prefix.up(signal[:rate//2]))
    np.testing.assert_allclose(prefix_output, output[:len(prefix_output)], atol=1e-12, rtol=0)


def test_downsampling_rejects_out_of_band_tone():
    resampler = FilterResampler(16000)
    signal = np.sin(2 * np.pi * 12000 * np.arange(48000) / 48000)
    output = resampler.down(signal)[1600:]
    assert 20 * np.log10(np.sqrt(np.mean(output ** 2)) * np.sqrt(2)) < -70


@pytest.mark.skipif(not os.getenv("DEEPFILTER_LIBRARY") or not os.getenv("DEEPFILTER_MODEL"),
                    reason="real DeepFilterNet library and model required")
@pytest.mark.parametrize("rate", [16000, 24000, 48000])
def test_real_filter_packet_invariance_and_42_ms_delay(rate):
    from scipy.signal import correlate, correlation_lags
    from server import DeepFilterNet, FIRDeepFilterStream
    from test_server import voiced
    model = DeepFilterNet(os.environ["DEEPFILTER_LIBRARY"], os.environ["DEEPFILTER_MODEL"], pool=0)
    whole, split = FIRDeepFilterStream(model, rate), FIRDeepFilterStream(model, rate)
    try:
        audio = np.asarray(voiced(rate, 2), dtype="<i2")
        expected = whole.process(audio.tobytes())
        actual = b"".join(split.process(x.tobytes()) for x in np.array_split(audio, 137))
        assert actual == expected
        assert len(actual) == audio.nbytes
        output = np.frombuffer(actual, dtype="<i2").astype(float)
        correlation = correlate(output, audio.astype(float), method="fft")
        lag = correlation_lags(len(output), len(audio))[np.argmax(correlation)]
        assert abs(lag / rate * 1000 - 42) < 0.2
    finally:
        whole.close()
        split.close()
