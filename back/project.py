#!/usr/bin/env python3
"""白名单投影：后台事件日志 -> 前台唯一可见的状态文档。

非 Agent、纯函数、不含模型调用。规则见 standards/projection.md。

用法:
    python3 project.py <events.jsonl> [--out projection.md]

任何违反规则的记录都会让脚本**报错退出**（不静默丢弃）——
静默丢弃会让后台以为自己上报了，而前台其实什么都没收到。
"""
import json
import sys
from collections import OrderedDict

ALLOWED = {"failure", "change", "observation", "blocker", "decision_needed"}
REQUIRED = {
    "failure": ("where", "cause"),
    "change": ("scope",),
    "observation": ("metric", "window", "value"),
    "blocker": ("on",),
    "decision_needed": ("options",),
}
FORBIDDEN = {"retry", "hypothesis", "debate", "progress", "intent", "plan", "draft"}

HEADINGS = OrderedDict([
    ("blocker", "卡点"),
    ("decision_needed", "需要你决定"),
    ("failure", "已确认的失败"),
    ("change", "已发生的变更"),
    ("observation", "观测到的数字"),
])


def die(msg):
    print(f"project.py: {msg}", file=sys.stderr)
    sys.exit(2)


def load(path):
    events = []
    with open(path, encoding="utf-8") as fh:
        for lineno, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError as e:
                die(f"{path}:{lineno} 不是合法 JSON: {e}")
    return events


def validate(ev, lineno):
    t = ev.get("type")
    if t is None:
        die(f"第 {lineno} 条缺 type")
    if t in FORBIDDEN:
        return False  # 黑名单：不出，且不算错（后台本来就该记过程）
    if t not in ALLOWED:
        die(f"第 {lineno} 条 type={t!r} 既不在白名单也不在黑名单——无法判定，请显式归类")

    # 白名单记录：必须带证据
    if not ev.get("evidence"):
        die(f"第 {lineno} 条 type={t} 是白名单类型但没有 evidence——"
            f"没有可复现证据的'事实'只是另一种猜测")
    for field in REQUIRED[t]:
        if not ev.get(field):
            die(f"第 {lineno} 条 type={t} 缺必备字段 {field!r}")
    if t == "decision_needed" and len(ev["options"]) < 2:
        die(f"第 {lineno} 条 decision_needed 只有 {len(ev['options'])} 个选项——"
            f"不带选项的'需要你决定'等于把后台的纠结原样倒给需求方")
    return True


def render(kept, stage, last_confirmed):
    out = ["# 当前状态", ""]
    out.append(f"- 阶段：{stage or '未开始'}")
    out.append(f"- 上一个已确认的完成点：{last_confirmed or '无'}")
    out.append("")
    if not kept:
        out.append("暂无已确认的事实。")
        return "\n".join(out) + "\n"

    for t, heading in HEADINGS.items():
        rows = [e for e in kept if e["type"] == t]
        if not rows:
            continue
        out.append(f"## {heading}")
        out.append("")
        for e in rows:
            out.append(f"- **{e['summary']}**")
            for key in REQUIRED[t]:
                if key == "options":
                    for i, opt in enumerate(e["options"], 1):
                        out.append(f"  {i}. {opt}")
                else:
                    out.append(f"  - {key}: {e[key]}")
            out.append(f"  - 证据: {e['evidence']}")
        out.append("")
    return "\n".join(out)


def main():
    if len(sys.argv) < 2:
        die("用法: project.py <events.jsonl> [--out projection.md]")
    src = sys.argv[1]
    dst = "projection.md"
    if "--out" in sys.argv:
        dst = sys.argv[sys.argv.index("--out") + 1]

    events = load(src)
    kept, dropped = [], 0
    for lineno, ev in enumerate(events, 1):
        if validate(ev, lineno):
            if not ev.get("summary"):
                die(f"第 {lineno} 条缺 summary——前台需要一句人话，不是字段堆")
            kept.append(ev)
        else:
            dropped += 1

    stage = next((e.get("stage") for e in reversed(events) if e.get("stage")), None)
    last = next((e["summary"] for e in reversed(kept) if e["type"] == "change"), None)

    with open(dst, "w", encoding="utf-8") as fh:
        fh.write(render(kept, stage, last))

    print(f"{len(events)} 条事件 -> 出口 {len(kept)} 条，拦下 {dropped} 条 -> {dst}")


main()
