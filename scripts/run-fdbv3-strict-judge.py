#!/usr/bin/env python3
"""Run the pinned FDB v3 GPT-4o evaluator with fail-closed call evidence."""

from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
from types import SimpleNamespace
from typing import Any


OPENAI_API_ORIGIN = "https://api.openai.com/v1"


class StrictJudgeError(RuntimeError):
    """The official judge did not complete every expected valid call."""


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evaluator", required=True, type=Path)
    parser.add_argument("--evaluator-sha256", required=True)
    parser.add_argument("--benchmark", required=True, type=Path)
    parser.add_argument("--results-dir", required=True, type=Path)
    parser.add_argument("--provider", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--expected-scenarios", required=True, type=int)
    return parser.parse_args()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="utf-8") as target:
        json.dump(value, target, indent=2, ensure_ascii=False, allow_nan=False)
        target.write("\n")
        target.flush()
        os.fsync(target.fileno())
    os.replace(temporary, path)


def load_evaluator(path: Path, expected_hash: str) -> Any:
    actual_hash = sha256_file(path)
    if actual_hash != expected_hash:
        raise StrictJudgeError(
            f"official evaluator hash is {actual_hash}, expected {expected_hash}"
        )
    specification = importlib.util.spec_from_file_location(
        "pinned_fdbv3_evaluator", path
    )
    if specification is None or specification.loader is None:
        raise StrictJudgeError(f"cannot load official evaluator {path}")
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


def build_entries(
    benchmark: dict[str, Any], results_dir: Path, provider: str
) -> tuple[list[dict[str, Any]], dict[str, int]]:
    scenarios = {scenario["id"]: scenario for scenario in benchmark["scenarios"]}
    judged: dict[Any, Path] = {}
    entries: list[dict[str, Any]] = []
    argument_calls = 0
    response_calls = 0
    for result_path in sorted(results_dir.rglob(f"result_{provider}.json")):
        result = json.loads(result_path.read_text(encoding="utf-8"))
        # A result whose example_id names no preregistered scenario is
        # evidence the run does not match the benchmark it claims to answer.
        # Skipping it dropped that sample from the judged population, and the
        # count check below then compared the survivors against
        # --expected-scenarios -- so an unknown id paired with a duplicated
        # one balanced the ledger exactly while leaving a real scenario
        # unjudged. Both halves are refused by identity, not by count.
        example_id = result.get("example_id")
        scenario = scenarios.get(example_id)
        if scenario is None:
            raise StrictJudgeError(
                f"{result_path} names scenario {example_id!r}, which the "
                f"benchmark does not preregister"
            )
        if example_id in judged:
            raise StrictJudgeError(
                f"{result_path} repeats scenario {example_id!r}, already "
                f"judged from {judged[example_id]}"
            )
        judged[example_id] = result_path
        # The ledger below predicts how many judge calls this population must
        # open, and the run is refused unless exactly that many succeed. That
        # reconciliation is only meaningful if it is computed over evidence
        # the runner actually recorded: defaulting an absent tool-call list to
        # `[]` or an absent transcript to `""` predicts zero calls for the
        # sample, which the judge then trivially satisfies by never scoring it.
        actual_calls = result.get("actual_tool_calls")
        if not isinstance(actual_calls, list) or not all(
            isinstance(call, dict) for call in actual_calls
        ):
            raise StrictJudgeError(
                f"{result_path} recorded no tool calls for scenario "
                f"{scenario['id']}"
            )
        remaining = Counter(call.get("function") for call in actual_calls)
        for expected in scenario["expected_tool_calls"]:
            function = expected["function"]
            if remaining[function] > 0:
                argument_calls += 1
                remaining[function] -= 1
        transcript = result.get("transcript")
        if not isinstance(transcript, str):
            raise StrictJudgeError(
                f"{result_path} recorded no transcript for scenario "
                f"{scenario['id']}"
            )
        if transcript.strip():
            response_calls += 1
        entries.append(
            {
                "scenario": scenario,
                "calls": actual_calls,
                "transcript": transcript,
                "result_data": result,
            }
        )
    unjudged = sorted(set(scenarios) - set(judged), key=str)
    if unjudged:
        raise StrictJudgeError(
            f"{len(unjudged)} preregistered scenarios produced no result: "
            f"{', '.join(str(item) for item in unjudged[:5])}"
        )
    return entries, {
        "argument": argument_calls,
        "response": response_calls,
        "total": argument_calls + response_calls,
    }


class StrictCompletions:
    def __init__(self, completions: Any, records: list[dict[str, Any]]) -> None:
        self.completions = completions
        self.records = records

    def create(self, **kwargs: Any) -> Any:
        response = self.completions.create(**kwargs)
        try:
            raw_content = response.choices[0].message.content
            if not isinstance(raw_content, str):
                raise ValueError("judge response content is not text")
            content = raw_content.strip()
            if content.startswith("```"):
                lines = content.splitlines()[1:]
                if lines and lines[-1].strip().startswith("```"):
                    lines = lines[:-1]
                content = "\n".join(lines).strip()
            parsed = json.loads(content)
            if not isinstance(parsed.get("correct"), bool) or not isinstance(
                parsed.get("explanation"), str
            ) or not parsed["explanation"].strip():
                raise ValueError(
                    "judge response lacks boolean correct and nonempty explanation fields"
                )
        except (AttributeError, IndexError, TypeError, json.JSONDecodeError, ValueError) as error:
            raise StrictJudgeError(f"invalid GPT-4o judge response: {error}") from error
        request_json = json.dumps(
            kwargs,
            sort_keys=True,
            ensure_ascii=False,
            separators=(",", ":"),
            allow_nan=False,
        )
        usage = getattr(response, "usage", None)
        if hasattr(usage, "model_dump"):
            usage = usage.model_dump(mode="json")
        elif usage is not None and not isinstance(usage, dict):
            usage = None
        response_id = getattr(response, "id", None)
        response_model = getattr(response, "model", None)
        if not isinstance(response_id, str) or not response_id:
            raise StrictJudgeError("GPT-4o judge response has no response ID")
        if not isinstance(response_model, str) or not (
            response_model == "gpt-4o" or response_model.startswith("gpt-4o-")
        ):
            raise StrictJudgeError(
                f"GPT-4o judge returned unexpected model {response_model!r}"
            )
        if (
            not isinstance(usage, dict)
            or not isinstance(usage.get("total_tokens"), int)
            or isinstance(usage["total_tokens"], bool)
            or usage["total_tokens"] <= 0
        ):
            raise StrictJudgeError("GPT-4o judge response has no valid token usage")
        self.records.append(
            {
                "sequence": len(self.records),
                "requested_model": kwargs.get("model"),
                "response_id": response_id,
                "response_model": response_model,
                "request_sha256": sha256_text(request_json),
                "response_sha256": sha256_text(raw_content),
                "parsed_response_sha256": sha256_text(
                    json.dumps(
                        parsed,
                        sort_keys=True,
                        ensure_ascii=False,
                        separators=(",", ":"),
                        allow_nan=False,
                    )
                ),
                "usage": usage,
            }
        )
        return response


def strict_client(client: Any, records: list[dict[str, Any]]) -> Any:
    return SimpleNamespace(
        chat=SimpleNamespace(
            completions=StrictCompletions(client.chat.completions, records)
        )
    )


def main() -> int:
    args = arguments()
    if args.expected_scenarios < 1:
        raise StrictJudgeError("--expected-scenarios must be positive")
    evaluator_path = args.evaluator.resolve()
    module = load_evaluator(evaluator_path, args.evaluator_sha256)
    benchmark = json.loads(args.benchmark.read_text(encoding="utf-8"))
    entries, expected_calls = build_entries(
        benchmark, args.results_dir.resolve(), args.provider
    )
    if len(entries) != args.expected_scenarios:
        raise StrictJudgeError(
            f"found {len(entries)} scenarios, expected {args.expected_scenarios}"
        )
    if expected_calls["total"] < 1:
        raise StrictJudgeError("the population opened no GPT-4o judge calls")

    try:
        from openai import OpenAI
    except ImportError as error:
        raise StrictJudgeError("the OpenAI SDK is required for the strict judge") from error
    records: list[dict[str, Any]] = []
    module._openai_client = strict_client(
        OpenAI(base_url=OPENAI_API_ORIGIN), records
    )
    report = module.evaluate_all_v2(benchmark, entries, use_llm=True)
    if len(records) != expected_calls["total"]:
        raise StrictJudgeError(
            f"completed {len(records)}/{expected_calls['total']} valid judge calls"
        )
    atomic_json(args.output.resolve(), report)
    evidence = {
        "schema_version": "1.1.0",
        "status": "complete",
        "generated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "scenarios": len(entries),
        "expected_calls": expected_calls,
        "successful_valid_calls": len(records),
        "api_origin": OPENAI_API_ORIGIN,
        "evaluator": {
            "path": str(evaluator_path),
            "sha256": args.evaluator_sha256,
            "entrypoint": "evaluate_all_v2(use_llm=True)",
        },
        "evaluation": {
            "path": str(args.output.resolve()),
            "sha256": sha256_file(args.output.resolve()),
        },
        "calls": records,
    }
    atomic_json(args.evidence.resolve(), evidence)
    print(
        f"strict FDBv3 judge complete: {len(entries)} scenarios, "
        f"{len(records)} valid GPT-4o calls"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except StrictJudgeError as error:
        print(f"strict FDBv3 judge failed: {error}", file=os.sys.stderr)
        raise SystemExit(1)
