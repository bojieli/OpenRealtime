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


@pytest.mark.skipif(not os.getenv("DEEPFILTER_LIBRARY") or not os.getenv("DEEPFILTER_MODEL"),
                    reason="real DeepFilterNet library and model required")
def test_real_filter_http_contract():
    import http.client
    import json
    import threading
    from http.server import ThreadingHTTPServer
    from server import DeepFilterNet, FIRDeepFilterStream, Sessions, handler_for

    model = DeepFilterNet(os.environ["DEEPFILTER_LIBRARY"], os.environ["DEEPFILTER_MODEL"], pool=0)
    sessions = Sessions(model, stream_factory=FIRDeepFilterStream)
    server = ThreadingHTTPServer(('127.0.0.1', 0), handler_for(
        sessions, model_name='deepfilternet-fir', audio_delay_ms=42))
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    connection = http.client.HTTPConnection(*server.server_address, timeout=5)
    try:
        connection.request('GET', '/health')
        response = connection.getresponse()
        assert response.status == 200
        assert json.loads(response.read())['audio_delay_ms'] == 42
        for rate in (16000, 24000, 48000):
            path = '/v1/filter/' + f'{rate:032x}'
            for seq, count in enumerate([1, rate//10, 73, rate//100, 17]):
                pcm = np.zeros(count, dtype='<i2').tobytes()
                connection.request('POST', path, pcm, {
                    'X-Sample-Rate': str(rate), 'X-Sequence': str(seq)})
                response = connection.getresponse()
                assert response.status == 200
                assert response.getheader('X-Audio-Delay-MS') == '42'
                assert response.getheader('X-Filter-Model') == 'deepfilternet-fir'
                assert response.getheader('X-Sequence') == str(seq)
                assert len(response.read()) == len(pcm)
            connection.request('DELETE', path)
            response = connection.getresponse()
            assert response.status == 200
            response.read()
        assert not sessions.streams
    finally:
        connection.close()
        server.shutdown()
        server.server_close()
        thread.join(2)
