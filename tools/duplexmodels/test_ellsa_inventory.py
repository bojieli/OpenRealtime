import json
import pytest
from ellsa_eval import select_items, save_report


def test_requested_population_is_not_silently_replaced_by_available_files(tmp_path):
    (tmp_path/'json').mkdir()
    (tmp_path/'llama_questions').mkdir()
    items = [{'path': ['upstream/one.wav']}, {'path': ['upstream/two.wav']}]
    (tmp_path/'json/llama_questions.json').write_text(json.dumps({'annotation': items}))
    (tmp_path/'llama_questions/two.wav').touch()
    with pytest.raises(ValueError, match='missing selected'):
        select_items(tmp_path, 1)
    (tmp_path/'llama_questions/one.wav').touch()
    assert select_items(tmp_path, 2) == items
    for count in (0, -1, 3):
        with pytest.raises(ValueError):
            select_items(tmp_path, count)


def test_partial_checkpoint_remains_explicitly_incomplete(tmp_path):
    path = tmp_path/'result.json'
    report = {'expected_items': 2, 'complete': False, 'items': [{'answer': 'one'}]}
    save_report(path, report)
    assert json.loads(path.read_text()) == report
    assert not path.with_suffix('.json.tmp').exists()
