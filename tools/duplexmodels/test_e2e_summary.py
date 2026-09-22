import json
from e2e_summary import fdb


def test_typed_and_legacy_applicability_remain_distinct_from_unknown(tmp_path):
    tasks = [
        {'applicability':'applicable','passed':True},
        {'applicability':'not_applicable'},
        {'notes':{'applicable':'true'},'passed':True},
        {'notes':{'applicable':'false'}},
        {},
        {'applicability':'applicable','notes':{'applicable':'false'}},
        {'applicability':'applicable','error':'connection failed'},
    ]
    (tmp_path/'fdb-user_interruption.json').write_text(json.dumps({'tasks':tasks}))
    result = fdb(tmp_path)['user_interruption']
    assert result['tasks'] == 7
    assert result['applicable'] == result['passed'] == 2
    assert result['not_applicable'] == 2
    assert result['unknown_applicability'] == 2
    assert result['errors'] == 1
