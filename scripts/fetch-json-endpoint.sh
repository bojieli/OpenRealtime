#!/usr/bin/env bash

set -euo pipefail

if [[ $# -lt 1 || $# -gt 3 ]]; then
  echo "usage: scripts/fetch-json-endpoint.sh url [description] [max-seconds]" >&2
  exit 2
fi
url="$1"
description="${2:-${url}}"
# A caller that had its own deadline keeps it: a hung endpoint must fail rather
# than stall a scored benchmark indefinitely.
maximum_seconds="${3:-}"
curl_arguments=(--fail --silent --show-error)
if [[ -n "${maximum_seconds}" ]]; then
  curl_arguments+=(--max-time "${maximum_seconds}")
fi

# Fetch one JSON document, or refuse and name what was wrong.
#
# `curl --fail` only rejects an HTTP error status, so a 200 with an empty body
# succeeds and leaves the caller holding an empty string. Every preregistration
# assertion in this repository is written as `jq -e '<must be true>' <<<"${body}"`,
# and jq exits 0 on empty input for ANY filter -- the filter never runs, so jq
# reports "no output produced" rather than false. A gateway that answered
# /healthz with a blank 200 would therefore satisfy every runtime requirement a
# matrix preregisters, and a scored benchmark would proceed against an
# unverified runtime. Refuse the empty and the non-object body here, once, so
# each caller's assertions are reached with a document that can fail them.
if ! body="$(curl "${curl_arguments[@]}" "${url}")"; then
  echo "endpoint fetch failed: ${description}" >&2
  exit 1
fi
if [[ -z "${body}" ]]; then
  echo "endpoint returned an empty body: ${description}" >&2
  exit 1
fi
if ! jq -e 'type == "object" and (keys | length) > 0' <<<"${body}" >/dev/null 2>&1; then
  echo "endpoint did not return a JSON object: ${description}" >&2
  exit 1
fi
printf '%s\n' "${body}"
