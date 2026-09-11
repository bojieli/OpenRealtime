"""Stateful PCM inference for the pinned REAL-TSE causal speaker-embedding model.

The upstream separator is causal but its public API processes complete WAVs.
This adapter preserves temporal LSTM states and STFT overlap across packets.
It uses the checkpoint's own WeSpeaker encoder, not the unrelated speaker-ID
service. Run setup.py before importing this module from a service.
"""

import sys
from pathlib import Path

import numpy as np
import torch
import torchaudio.compliance.kaldi as kaldi


class Model:
    def __init__(self, runtime, device="cuda"):
        runtime = Path(runtime).resolve()
        sys.path.insert(0, str(runtime / "python"))
        from wesep.modules.separator.bsrnn import BSRNN
        from wespeaker.models.ecapa_tdnn import ECAPA_TDNN_GLOB_c512

        torch.set_num_threads(1)
        self.device = device
        state = torch.load(
            runtime / "avg_model.pt", map_location="cpu", weights_only=True
        )["models"][0]
        self.separator = BSRNN(causal=True).eval().to(device)
        self.separator.load_state_dict(
            {
                k.removeprefix("sep_model."): v
                for k, v in state.items()
                if k.startswith("sep_model.")
            },
            strict=True,
        )
        self.encoder = ECAPA_TDNN_GLOB_c512(
            feat_dim=80, embed_dim=192, pooling_func="ASTP"
        ).eval()
        self.encoder.load_state_dict(
            {
                k.removeprefix("spk_ft.encoder.spk_model."): v
                for k, v in state.items()
                if k.startswith("spk_ft.encoder.spk_model.")
            },
            strict=True,
        )
        self.weight = state["spk_ft.spkemb.fusionLayer.fc.linear.weight"]
        self.bias = state["spk_ft.spkemb.fusionLayer.fc.linear.bias"]
        self.window = torch.hann_window(512, device=device)

    @torch.inference_mode()
    def enroll(self, reference):
        # CPU worker: a one-time reference encoding cannot occupy the GPU's
        # audio worker or cause a new per-utterance speaker-comparison wait.
        x = torch.from_numpy(reference.copy()).view(1, -1)
        f = kaldi.fbank(
            x * 32768,
            num_mel_bins=80,
            dither=0,
            sample_frequency=16000,
            window_type="hamming",
            use_energy=False,
        )
        e = self.encoder((f - f.mean(0)).unsqueeze(0))
        if isinstance(e, tuple):
            e = e[-1]
        cue = torch.nn.functional.linear(e, self.weight, self.bias).view(1, 1, 128, 1)
        if not torch.isfinite(cue).all():
            raise ValueError("invalid target voice reference")
        return cue

    @torch.inference_mode()
    def spectral(self, spec, cue, states):
        m = self.separator
        x = m.subband_norm(m.band_split(torch.stack([spec.real, spec.imag], 1))) * cue
        batch, bands, features, frames = x.shape
        x = x.reshape(batch, bands * features, frames)
        for index, block in enumerate(m.separator.separation):
            a = x.reshape(batch * bands, features, frames)
            rnn = block.band_rnn
            y, states[index] = rnn.rnn(
                rnn.norm(a).transpose(1, 2).contiguous(), states[index]
            )
            y = a + rnn.proj(y).transpose(1, 2)
            y = (
                y.reshape(batch, bands, features, frames)
                .permute(0, 3, 2, 1)
                .contiguous()
                .reshape(batch * frames, features, bands)
            )
            x = (
                block.band_comm(y)
                .reshape(batch, frames, features, bands)
                .permute(0, 3, 2, 1)
                .contiguous()
                .reshape(batch, bands * features, frames)
            )
        mask = m.band_masker(
            x.reshape(batch, bands, features, frames), m.band_split(spec)
        )
        return torch.complex(mask[:, 0], mask[:, 1]).squeeze(1)


class Extractor:
    """16kHz float PCM in/out; caller supplies 512-sample (32ms) blocks.

    Released samples are undelayed, but available only after analysis/synthesis
    overlap. The service adds a fixed FIFO to support arbitrary packet sizes.
    """

    def __init__(self, model, cue):
        self.model = model
        self.cue = cue.to(model.device)
        self.states = [None] * 6
        self.pending = torch.empty(0, device=model.device)
        self.ola = torch.zeros(512, device=model.device)
        self.norm = torch.zeros(512, device=model.device)
        self.started = False
        self.discard = 256

    @torch.inference_mode()
    def process(self, samples):
        m = self.model
        samples = torch.as_tensor(samples, device=m.device)
        if not self.started:
            # Match upstream centered STFT's initial reflection exactly.
            self.pending = torch.cat([samples[1:257].flip(0), self.pending])
            self.started = True
        self.pending = torch.cat([self.pending, samples])
        frames = self.pending.unfold(0, 512, 128)
        count = frames.shape[0]
        spec = torch.fft.rfft(frames * m.window, dim=-1).transpose(0, 1).unsqueeze(0)
        enhanced = m.spectral(spec, self.cue, self.states)
        waves = torch.fft.irfft(enhanced.squeeze(0).transpose(0, 1), n=512) * m.window
        out = []
        for wave in waves:
            self.ola += wave
            self.norm += m.window.square()
            out.append((self.ola[:128] / self.norm[:128].clamp_min(1e-8)).clone())
            self.ola = torch.cat([self.ola[128:], torch.zeros(128, device=m.device)])
            self.norm = torch.cat([self.norm[128:], torch.zeros(128, device=m.device)])
        self.pending = self.pending[count * 128 :].clone()
        result = torch.cat(out)
        skipped = min(self.discard, len(result))
        self.discard -= skipped
        result = result[skipped:].cpu().numpy()
        if not np.isfinite(result).all():
            raise ValueError("non-finite extracted audio")
        return result
