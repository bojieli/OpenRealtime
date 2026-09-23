"""Guard offline X2-Turn arrays from being mistaken for causal observations."""
import json
import math
from pathlib import Path
import warnings

TOLERANCE = 1e-3
# Accepted 2026-09-23: at a fixed buffer length X2-Turn frames use no audio after
# their availability point, but a shorter buffer shifts early probabilities by up
# to 0.123 (deterministic, argmax unchanged). Offline arrays are therefore causal
# in information, within this fidelity gap of what a streaming decoder would emit.
ACCEPTED_BUFFER_LENGTH_GAP = 0.13


def _bounded(value, limit):
    return (isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(value) and 0 <= value <= limit)


def causal_timing(path):
    data = json.loads(Path(path).read_text())
    delay = data.get('available_after_frames')
    # Frame count alone does not establish availability. The offline decoder
    # pads/aligned-decodes a whole buffer, and earlier posteriors can change.
    prefix_equal = (data.get('causal_prefix_verified') is True
                    and _bounded(data.get('max_abs_diff_prefix_vs_full'), TOLERANCE))
    future_independent = (data.get('future_independent_verified') is True
                          and _bounded(data.get('max_abs_diff_future_silenced'), TOLERANCE)
                          and _bounded(data.get('buffer_length_max_abs_diff'), ACCEPTED_BUFFER_LENGTH_GAP))
    valid = (isinstance(delay, int) and not isinstance(delay, bool) and delay >= 0
             and (prefix_equal or future_independent))
    if not valid:
        warnings.warn(f'Excluding X2-Turn from causal scoring: unverified prefix timing in {path}',
                      RuntimeWarning, stacklevel=2)
    return valid
