#!/usr/bin/env python3
"""Replay captured Gemini request bodies with randomized model/thinking settings.

GEMINI_API_KEY must be set. Cases are [{name, expected, body}]. Keep captured
conversation context private. This test does not change the deployed room.

A thinking setting is either a legacy numeric ``thinkingBudget`` (--budgets) or a
native ``thinkingLevel`` (--levels); a request never carries both. --include-thoughts
selects whether thought summaries are returned, which is what the room needs for
observability; ``both`` runs each cell under either setting so the cost of turning
summaries on is measured rather than assumed.
"""

import argparse
import copy
import itertools
import json
import os
from pathlib import Path
import random
import time

import requests


def measure(session, model, thinking, case, include_thoughts=False):
    """Time one replay. ``thinking`` is an int budget or a str thinking level."""
    body = copy.deepcopy(case["body"])
    config = body.setdefault("generationConfig", {})
    row = {"model": model, "case": case["name"], "include_thoughts": include_thoughts}
    if isinstance(thinking, str):
        config["thinkingConfig"] = {"thinkingLevel": thinking}
        row["thinking_level"] = thinking
    else:
        config["thinkingConfig"] = {"thinkingBudget": thinking}
        row["thinking_budget"] = thinking
    config["thinkingConfig"]["includeThoughts"] = include_thoughts
    config["maxOutputTokens"] = 1024
    config["temperature"] = 0
    started = time.perf_counter()
    pieces = []
    thoughts = []
    try:
        with session.post(
            f"https://generativelanguage.googleapis.com/v1beta/models/{model}:streamGenerateContent",
            params={"alt": "sse"},
            json=body,
            stream=True,
            timeout=(15, 45),
        ) as response:
            row["headers_ms"] = (time.perf_counter() - started) * 1000
            row["http_status"] = response.status_code
            if response.status_code != 200:
                row["error"] = response.text[:1200]
                return row
            for line in response.iter_lines(chunk_size=1):
                if not line.startswith(b"data:"):
                    continue
                payload = line[5:].strip()
                if payload == b"[DONE]":
                    continue
                event = json.loads(payload)
                if "usageMetadata" in event:
                    row["usage"] = event["usageMetadata"]
                for candidate in event.get("candidates", []):
                    if "finishReason" in candidate:
                        row["finish_reason"] = candidate["finishReason"]
                    for part in candidate.get("content", {}).get("parts", []):
                        text = part.get("text", "")
                        if not text:
                            continue
                        if part.get("thought"):
                            if "first_thought_ms" not in row and text.strip():
                                row["first_thought_ms"] = (
                                    time.perf_counter() - started
                                ) * 1000
                            thoughts.append(text)
                            continue
                        if "first_text_ms" not in row and text.strip():
                            row["first_text_ms"] = (time.perf_counter() - started) * 1000
                        pieces.append(text)
            row["complete_ms"] = (time.perf_counter() - started) * 1000
    except (requests.RequestException, ValueError) as error:
        row["error"] = str(error)
    row["text"] = "".join(pieces).strip()
    row["thought_summary_chars"] = len("".join(thoughts).strip())
    row["basic_pass"] = (
        not row.get("error")
        and row.get("finish_reason") == "STOP"
        and bool(row["text"])
        and case.get("expected", "").lower() in row["text"].lower()
        and "<wait>" not in row["text"]
    )
    return row


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--models",
        nargs="+",
        default=[
            "gemini-3-flash-preview",
            "gemini-3.5-flash",
            "gemini-3.6-flash",
            "gemini-3.7-flash",
            "gemini-3.8-flash",
        ],
    )
    parser.add_argument("--budgets", nargs="+", type=int, default=[0, 128, 512])
    parser.add_argument(
        "--levels",
        nargs="+",
        default=[],
        help="native thinkingLevel values; requested instead of a numeric budget",
    )
    parser.add_argument(
        "--include-thoughts",
        choices=["false", "true", "both"],
        default="false",
        help="request thought summaries; 'both' measures each cell either way",
    )
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--seed", type=int, default=20260909)
    args = parser.parse_args()
    cases = json.loads(args.cases.read_text())
    settings = list(args.budgets) + list(args.levels)
    thought_flags = {"false": [False], "true": [True], "both": [False, True]}[
        args.include_thoughts
    ]
    schedule = list(
        itertools.product(
            range(args.repeats), args.models, settings, thought_flags, cases
        )
    )
    random.Random(args.seed).shuffle(schedule)
    session = requests.Session()
    session.headers["x-goog-api-key"] = os.environ["GEMINI_API_KEY"]
    with args.output.open("x") as output:
        for index, (repeat, model, setting, include_thoughts, case) in enumerate(
            schedule
        ):
            row = measure(session, model, setting, case, include_thoughts)
            row["repeat"] = repeat + 1
            output.write(json.dumps(row) + "\n")
            output.flush()
            print(
                index + 1,
                model,
                setting,
                f"thoughts={include_thoughts}",
                case["name"],
                row.get("http_status"),
                round(row.get("first_text_ms", 0)),
                row.get("basic_pass", False),
                flush=True,
            )
