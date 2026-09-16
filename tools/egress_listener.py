#!/usr/bin/env python3
"""Egress 观测器 —— 验证「降级出口真的生效了」。

存在的理由：2026-09-16 实测发现 multica agent 的 custom_env 是**静默失效**的
（~/.claude/settings.json 的 env 会 Object.assign 覆盖进程环境），`agent env get`
回显正常但请求根本没走那条出口。只有让请求打到自己的监听器上才看得见。

用法:
    python3 tools/egress_listener.py --port 18444 [--log /tmp/egress_probe.log]
    然后把出口指到 http://127.0.0.1:18444，发一条消息，
    日志里出现 POST 才算这个出口真的在生效。

配合 agent 的 custom_args: ["--settings", "<settings.json>"]，
因为 --settings 的优先级高于 user settings（env 的覆盖顺序见 standards/resilience.md）。
"""
import argparse, http.server, json, time

def make_handler(path):
    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a): pass
        def _record(self, body):
            with open(path, "a") as f:
                f.write(f"[{time.strftime('%H:%M:%S')}] {self.command} {self.path}\n")
                for k, v in self.headers.items():
                    if k.lower() in ("authorization", "x-api-key", "anthropic-version", "user-agent"):
                        f.write(f"    {k}: {v[:100]}\n")
                f.write(f"    body[:300]: {body[:300]!r}\n")
        def do_POST(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n)
            self._record(body)
            try:
                model = json.loads(body).get("model", "unknown")
            except Exception:
                model = "unknown"
            out = json.dumps({
                "id": "msg_egress_probe", "type": "message", "role": "assistant", "model": model,
                "content": [{"type": "text", "text": "EGRESS-PROBE-OK"}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)
    return H

if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18444)
    ap.add_argument("--log", default="/tmp/egress_probe.log")
    a = ap.parse_args()
    print(f"listening on 127.0.0.1:{a.port}, logging to {a.log}", flush=True)
    http.server.HTTPServer(("127.0.0.1", a.port), make_handler(a.log)).serve_forever()
