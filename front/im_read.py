#!/usr/bin/env python3
"""Render a multica chat session's messages as a readable transcript."""
import json
import sys

raw = sys.stdin.read()
if not raw.strip():
    sys.exit(0)
data = json.loads(raw)
items = data if isinstance(data, list) else data.get("messages") or data.get("data") or []

if not items:
    print("(no messages)")
    for k in ("keys",):
        pass
    if isinstance(data, dict):
        print("response keys:", list(data.keys()))
    sys.exit(0)

for m in items:
    who = m.get("author_type") or m.get("sender_type") or m.get("role") or "?"
    name = m.get("author_name") or m.get("sender_name") or m.get("agent_name") or ""
    ts = (m.get("created_at") or "")[:19]
    body = m.get("content") or m.get("text") or ""
    label = f"{who}:{name}" if name else who
    print(f"--- [{ts}] {label} ---")
    print(body)
    print()
