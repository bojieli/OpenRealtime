#!/usr/bin/env python3
"""Replay captured Gemini request bodies with randomized model/thinking settings.

GEMINI_API_KEY must be set. Cases are [{name, expected, body}]. Keep captured
conversation context private. This test does not change the deployed room.
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


def measure(session, model, budget, case):
    body = copy.deepcopy(case["body"])
    config = body.setdefault("generationConfig", {})
    config["thinkingConfig"] = {"thinkingBudget": budget, "includeThoughts": False}
    config["maxOutputTokens"] = 1024
    config["temperature"] = 0
    row = {"model": model, "thinking_budget": budget, "case": case["name"]}
    started = time.perf_counter()
    pieces = []
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
                        if text and not part.get("thought"):
                            if "first_text_ms" not in row and text.strip():
                                row["first_text_ms"] = (
                                    time.perf_counter() - started
                                ) * 1000
                            pieces.append(text)
            row["complete_ms"] = (time.perf_counter() - started) * 1000
    except (requests.RequestException, ValueError) as error:
        row["error"] = str(error)
    row["text"] = "".join(pieces).strip()
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
    parser.add_argument("--repeats", type=int, default=3)
    args = parser.parse_args()
    cases = json.loads(args.cases.read_text())
    schedule = list(
        itertools.product(range(args.repeats), args.models, args.budgets, cases)
    )
    random.Random(20260909).shuffle(schedule)
    session = requests.Session()
    session.headers["x-goog-api-key"] = os.environ["GEMINI_API_KEY"]
    with args.output.open("x") as output:
        for index, (repeat, model, budget, case) in enumerate(schedule):
            row = measure(session, model, budget, case)
            row["repeat"] = repeat + 1
            output.write(json.dumps(row) + "\n")
            output.flush()
            print(
                index + 1,
                model,
                budget,
                case["name"],
                row.get("http_status"),
                round(row.get("first_text_ms", 0)),
                row.get("basic_pass", False),
                flush=True,
            )
