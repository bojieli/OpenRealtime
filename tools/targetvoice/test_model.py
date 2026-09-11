"""Opt-in comparison to full-waveform inference with real pretrained weights.

TARGETVOICE_RUNTIME=.runtime/targetvoice python3 -m unittest tools.targetvoice.test_model
"""

import os
import unittest
import numpy as np
import torch
from tools.targetvoice.model import Model, Extractor


@unittest.skipUnless(
    os.environ.get("TARGETVOICE_RUNTIME"), "requires installed real checkpoint"
)
class ModelTest(unittest.TestCase):
    def test_streaming_matches_offline_across_different_packet_sizes(self):
        model = Model(os.environ["TARGETVOICE_RUNTIME"])
        x = np.random.default_rng(42).normal(0, 0.03, 16000).astype(np.float32)
        cue = torch.ones(1, 1, 128, 1, device=model.device)
        with torch.inference_mode():
            spectrum = model.separator.stft(
                torch.from_numpy(x).to(model.device).view(1, -1)
            )[-1]
            full = (
                model.separator.istft(model.spectral(spectrum, cue, [None] * 6))
                .squeeze()
                .cpu()
                .numpy()
            )
            for size in [512, 1024, 1536]:
                stream = Extractor(model, cue)
                output = np.concatenate(
                    [
                        stream.process(x[i : i + size])
                        for i in range(0, len(x) - size + 1, size)
                    ]
                )
                np.testing.assert_allclose(
                    output, full[: len(output)], atol=5e-4, rtol=0
                )


if __name__ == "__main__":
    unittest.main()
