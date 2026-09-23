# X2-Turn causality: padding-aware prefix check

`endpoint_eval.py x2turn-timing`: X2-Turn-4B-0812 through the turn service
(offline re-decode), FDB v1.5 `user_interruption` 1–3, prefixes at nine points
from 1.6 to 9.6 s. Run 2026-09-23 about 10:25 UTC.

The retained `../20260922-x2-causality/timing.json` compared every frame a
prefix call returned. The offline runtime pads each buffer with 2.56 s of
silence on the left and 1.36 s on the right, and it returns frames computed
over that right padding. The earlier probe therefore mostly measured padding.
This check admits frame f of a t-second prefix only if (f + 1 + A) × 80 ms ≤ t,
and searches the delay A. It adds two controls:

| delay A (frames) | prefix vs full buffer | same length, audio after t silenced |
| ---: | ---: | ---: |
| 0 | 0.99996 | 0.99994 |
| 5 | 0.54649 | 0.50281 |
| 6 | 0.12329 | 0.00032 |
| 7–15 | 0.12329 | 0.0 |

Decoding the full buffer twice gives identical output (0.0) for all three files.

- **No future information leaks.** At a fixed buffer length, frame f is
  unchanged when all audio after (f + 7) × 80 ms is replaced by silence. At
  A = 6 (the model's declared 480 ms delay) the difference is 3.2e-4, within
  the 1e-3 tolerance. At A = 7 it is exact.
- **Values depend on the buffer length.** A shorter buffer changes early
  frames by up to 0.123, deterministically, whatever the delay. At the worst
  frame of every row, the argmax label is unchanged. The behavior is
  consistent with numerics that depend on the decode shape; the mechanism is
  not isolated here.

`causal_prefix_verified` stays false under the existing rule (prefix equality
within 1e-3). The offline full-buffer arrays use no future audio at delay 6,
but a true streaming decoder would produce probabilities up to 0.12 different.
Whether that fidelity gap is acceptable for endpoint/overlap scoring is a
methodological decision. The guard in `x2_evidence.py` was not changed.
