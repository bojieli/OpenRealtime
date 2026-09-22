import json
import pytest
from x2_evidence import causal_timing


def test_negative_delay_and_revised_prefix_cannot_be_scored_as_causal(tmp_path):
    path = tmp_path/'timing.json'
    for data in [
        {'available_after_frames': -10, 'max_abs_diff_prefix_vs_full': .99096},
        {'causal_prefix_verified': True, 'available_after_frames': -10,
         'max_abs_diff_prefix_vs_full': 0},
        {'causal_prefix_verified': True, 'available_after_frames': 6,
         'max_abs_diff_prefix_vs_full': .1},
        {'available_after_frames': 6, 'max_abs_diff_prefix_vs_full': 0},
    ]:
        path.write_text(json.dumps(data))
        with pytest.warns(RuntimeWarning):
            assert not causal_timing(path)
