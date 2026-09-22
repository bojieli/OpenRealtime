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


def test_full_selected_zero_limit_still_requires_fdbench(tmp_path):
    from e2e_summary import CATEGORIES, staleness
    from e2e_run import digest

    expected = [f"fdb-{category}.json" for category in CATEGORIES] + ["fdbench.json"]
    run = {
        "schema": 2,
        "campaign_scope": "all FDB categories and complete cosyvoice2-single-round-combine-med",
        "fdbench_conversations": 0,
        "expected_results": expected,
    }
    (tmp_path / "run.json").write_text(json.dumps(run))
    (tmp_path / "profile.yaml").write_text("profile: test\n")
    for name in expected:
        (tmp_path / name).write_text(json.dumps({"tasks": []}))
    completion = {
        "exit_code": 0, "errors": [],
        "commands": {name: {"exit_code": 0} for name in expected},
        "sha256": {name: digest(tmp_path / name)
                   for name in ["run.json", "profile.yaml", *expected]},
    }
    (tmp_path / "finished.json").write_text(json.dumps(completion))
    assert staleness(tmp_path) == ""
    (tmp_path / "fdbench.json").unlink()
    assert staleness(tmp_path) != ""


def test_coverage_requires_complete_unique_error_free_population(tmp_path):
    from e2e_summary import coverage

    path = tmp_path / "fdbench.json"
    data = {"expected_tasks": 2, "summary": {"complete": True},
            "tasks": [{"id": "one"}, {"id": "two"}]}
    path.write_text(json.dumps(data))
    assert coverage(tmp_path, [path.name]).startswith("complete selected")
    for tasks in [[{"id": "one"}], [{"id": "one"}, {"id": "one"}],
                  [{"id": "one"}, {"id": "two", "error": "timeout"}]]:
        data["tasks"] = tasks
        path.write_text(json.dumps(data))
        assert "NOT REPORTABLE" in coverage(tmp_path, [path.name])
