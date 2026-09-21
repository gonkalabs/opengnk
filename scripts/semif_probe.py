#!/usr/bin/env python3
"""Probe GET/POST /v1/systemone against a gonka-proxy instance."""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request

FIXTURE = {
    "id": "route-1",
    "state": "Customer cannot access an account after a password reset.",
    "question": "Which queue should handle this request?",
    "options": [
        {"id": "access", "description": "Account access support."},
        {"id": "billing", "description": "Billing support."},
    ],
}


def get(url: str, key: str | None = None) -> tuple[int, dict]:
    headers = {"Accept": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode()
        try:
            return exc.code, json.loads(body)
        except json.JSONDecodeError:
            return exc.code, {"error": body}


def post(url: str, key: str, payload: dict) -> tuple[int, dict]:
    data = json.dumps(payload).encode()
    headers = {
        "Accept": "application/json",
        "Content-Type": "application/json",
    }
    if key:
        headers["Authorization"] = f"Bearer {key}"
    req = urllib.request.Request(
        url,
        data=data,
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode()
        try:
            return exc.code, json.loads(body)
        except json.JSONDecodeError:
            return exc.code, {"error": body}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True, help="Proxy origin, e.g. http://127.0.0.1:8080")
    parser.add_argument("--key", default="", help="API key for POST /v1/systemone")
    args = parser.parse_args()
    base = args.base.rstrip("/")

    status, cap = get(f"{base}/v1/systemone")
    print(f"GET /v1/systemone {status}")
    print(json.dumps(cap, indent=2))
    if status != 200:
        return 1
    if not cap.get("enabled"):
        print("scorer disabled — set SEMIF_BASE_URL (see docs/semif-plan-ru.md)")
        return 2

    status, decision = post(f"{base}/v1/systemone", args.key, FIXTURE)
    print(f"POST /v1/systemone {status}")
    print(json.dumps(decision, indent=2))
    if status != 200:
        return 1
    probs = decision.get("probabilities") or []
    if len(probs) != 2:
        print("expected 2 probabilities")
        return 1
    if abs(sum(probs) - 1.0) > 1e-5:
        print(f"probabilities do not sum to 1: {probs}")
        return 1
    if not decision.get("chosen_id"):
        print("missing chosen_id")
        return 1
    print("ok", decision.get("chosen_id"), decision.get("readout"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
