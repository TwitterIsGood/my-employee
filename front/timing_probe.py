#!/usr/bin/env python3
"""Measure where a multica chat turn's wall-clock goes.

Splits one turn into:
  t_send  -> t_claim : queue/dispatch latency (daemon poll interval dominates)
  t_claim -> t_reply : actual agent work
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

BASE = os.environ.get("MULTICA_BASE", "http://localhost:13000")
WS = os.environ.get("MULTICA_WS", "<workspace-id>")
TOK = json.load(open(os.path.expanduser("~/.multica/config.json")))["token"]


def api(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + TOK)
    req.add_header("X-Workspace-ID", WS)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req) as r:
        raw = r.read()
        return json.loads(raw) if raw.strip() else {}


def msgs(sid):
    d = api("GET", f"/api/chat/sessions/{sid}/messages?limit=200")
    items = d if isinstance(d, list) else d.get("messages", [])
    return [m for m in items if (m.get("author_type") or m.get("sender_type") or m.get("role")) == "assistant"]


def main():
    sid, text = sys.argv[1], sys.argv[2]
    before = len(msgs(sid))

    t0 = time.monotonic()
    api("POST", f"/api/chat/sessions/{sid}/messages", {"content": text})

    t_claim = None
    while time.monotonic() - t0 < 600:
        p = api("GET", "/api/chat/pending-tasks")
        if p.get("tasks") and t_claim is None:
            t_claim = time.monotonic() - t0
        m = msgs(sid)
        if len(m) > before and m[-1].get("content"):
            t_reply = time.monotonic() - t0
            break
        time.sleep(0.5)
    else:
        print("TIMEOUT")
        return

    print(f"queue/dispatch (send -> claimed) : {t_claim:.1f}s" if t_claim else "claim not observed")
    print(f"agent work    (claimed -> reply) : {t_reply - (t_claim or 0):.1f}s")
    print(f"TOTAL                            : {t_reply:.1f}s")
    print(f"\nreply: {m[-1]['content'][:300]}")


main()
