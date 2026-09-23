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


def test_future_independence_is_accepted_within_the_recorded_length_gap(tmp_path):
    path = tmp_path/'timing.json'
    good = {'future_independent_verified': True, 'available_after_frames': 6,
            'max_abs_diff_future_silenced': 0.00032, 'buffer_length_max_abs_diff': 0.12329}
    path.write_text(json.dumps(good))
    assert causal_timing(path)
    for change in [{'future_independent_verified': False}, {'max_abs_diff_future_silenced': 0.01},
                   {'buffer_length_max_abs_diff': 0.5}, {'available_after_frames': -1},
                   {'buffer_length_max_abs_diff': None}]:
        path.write_text(json.dumps({**good, **change}))
        with pytest.warns(RuntimeWarning):
            assert not causal_timing(path)
