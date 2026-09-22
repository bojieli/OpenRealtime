"""Guard offline X2-Turn arrays from being mistaken for causal observations."""
import json
import math
from pathlib import Path
import warnings


def causal_timing(path):
    data = json.loads(Path(path).read_text())
    delay = data.get('available_after_frames')
    difference = data.get('max_abs_diff_prefix_vs_full')
    # Frame count alone does not establish availability. The offline decoder
    # pads/aligned-decodes a whole buffer, and earlier posteriors can change.
    valid = (data.get('causal_prefix_verified') is True
             and isinstance(delay, int) and not isinstance(delay, bool) and delay >= 0
             and isinstance(difference, (int, float)) and math.isfinite(difference)
             and 0 <= difference <= 1e-3)
    if not valid:
        warnings.warn(f'Excluding X2-Turn from causal scoring: unverified prefix timing in {path}',
                      RuntimeWarning, stacklevel=2)
    return valid
