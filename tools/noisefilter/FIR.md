# DeepFilterNet with causal FIR conversion

Use `--model deepfilternet-fir` to select the FIR path explicitly. It requires
NumPy and SciPy in addition to the existing libDF model/library. For example:

```bash
.runtime/duplex-plan/venvs/perception/bin/python tools/noisefilter/server.py \
  --model deepfilternet-fir \
  --library .runtime/deepfilter-filter/libdf.so \
  --deepfilter-model .runtime/deepfilter-filter/DeepFilterNet3_onnx.tar.gz \
  --port 9166 --state-pool 4
```

Set the Go audio-filter configuration's model to `deepfilternet-fir` as well.
`--state-pool` controls how many fresh model states are prepared before the
service declares readiness (default two). A burst beyond the available pool
builds additional states on the request path and can miss its first-packet
deadline. Four prebuilt states removed startup misses in one four-session,
30-second comparison; this is not an unlimited admission guarantee.
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

Downstream ASR quality and concurrent HTTP latency remain to be compared before
promoting this option into default profiles. Quiet backchannels are not
preserved: at -30 dB the model's local-SNR gate zeroes many clips, and a clean
offline 48 kHz conversion fails the same way. `quiet_diagnose.py` reproduces
this with per-frame local SNR; see the evidence README for measurements.
