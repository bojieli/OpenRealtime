"""Candidate causal integer resampling for the 48 kHz filter path.

Requires NumPy/SciPy. Each direction adds exactly 48/48000 = 1 ms
group delay, including at native 48 kHz, so a future service can declare
one rate-independent delay. No lookahead, packet padding, or end flush.
This module is not yet selected by the production filter service.
"""
import numpy as np
from scipy.signal import firwin, lfilter


class FilterResampler:
    def __init__(self, rate):
        if rate not in (16000, 24000, 48000):
            raise ValueError("rate must be 16000, 24000, or 48000")
        self.factor = 48000 // rate
        self.delay_48k_samples = 48
        if self.factor == 1:
            self.kernel = np.zeros(97)
            self.kernel[48] = 1
        else:
            self.kernel = firwin(97, 0.94 / self.factor, window=("kaiser", 8.6))
        self.up_state = np.zeros(96)
        self.down_state = np.zeros(96)
        self.down_position = 0

    def up(self, samples):
        samples = np.asarray(samples, dtype=np.float64)
        if not len(samples):
            return samples.copy()
        expanded = np.zeros(len(samples) * self.factor)
        expanded[::self.factor] = samples
        output, self.up_state = lfilter(
            self.kernel * self.factor, [1.0], expanded, zi=self.up_state)
        return output

    def down(self, samples):
        samples = np.asarray(samples, dtype=np.float64)
        if not len(samples):
            return samples.copy()
        output, self.down_state = lfilter(self.kernel, [1.0], samples, zi=self.down_state)
        first = (-self.down_position) % self.factor
        self.down_position += len(samples)
        return output[first::self.factor]
