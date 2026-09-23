import numpy as np
import pytest

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
