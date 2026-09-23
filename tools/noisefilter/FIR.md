# DeepFilterNet with causal FIR conversion

Use `--model deepfilternet-fir` to select the FIR path explicitly. It requires
NumPy and SciPy in addition to the existing libDF model/library. For example:

```bash
.runtime/duplex-plan/venvs/perception/bin/python tools/noisefilter/server.py \
  --model deepfilternet-fir \
  --library .runtime/deepfilter-filter/libdf.so \
  --deepfilter-model .runtime/deepfilter-filter/DeepFilterNet3_onnx.tar.gz \
  --port 9166
```

Set the Go audio-filter configuration's model to `deepfilternet-fir` as well.
The service and client enforce 42 ms waveform delay: 30 ms model, 10 ms
packet FIFO, and two 1 ms causal FIR delays. The original `deepfilternet`
option retains its 40 ms contract for matched comparisons.

The new conversion removes the pronounced high-frequency attenuation of the
linear-interpolation/block-average path. Isolated tests establish less than
0.1 dB loss at 6 kHz and more than 70 dB rejection of a 12 kHz input when
downsampling to 16 kHz. These are resampler measurements, not recognition or
denoising quality claims. Real-model tests establish packet independence and
42 ms delay at 16, 24, and 48 kHz; HTTP tests check sample counts, sequence,
model/delay headers, and session release.

Run the optional real-model checks with:

```bash
DEEPFILTER_LIBRARY="$PWD/.runtime/deepfilter-filter/libdf.so" \
DEEPFILTER_MODEL="$PWD/.runtime/deepfilter-filter/DeepFilterNet3_onnx.tar.gz" \
PYTHONPATH=tools/noisefilter \
  .runtime/duplex-plan/venvs/minicpm-duplex/bin/python -m pytest -q \
  tools/noisefilter/test_causal_resample.py
```

Downstream ASR quality, quiet-backchannel preservation, and concurrent HTTP
latency remain to be compared before promoting this option into default profiles.
