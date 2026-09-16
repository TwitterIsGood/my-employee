#!/usr/bin/env python3
"""投影层的回归闸。

跑法：python3 back/test_projection.py   （退出码非 0 即失败）

为什么要有这个文件：投影规则是靠一条一条实测踩出来的（验证 A 里前台确实把
"准备交给开发团队"这种没发生的事讲给了需求方）。规则写进文档会腐，写成断言不会。
standards/delivery.md 里"每个 [闸门] 谓词都要有对应的可执行检查"说的就是这个。
"""
import json
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT = os.path.join(HERE, "project.py")
FIXTURE = os.path.join(HERE, "fixtures", "events-active-users.jsonl")

failures = []


def run(events):
    """跑一次投影器，返回 (exit_code, stderr)。"""
    with tempfile.TemporaryDirectory() as d:
        src = os.path.join(d, "events.jsonl")
        with open(src, "w", encoding="utf-8") as fh:
            for ev in events:
                fh.write(json.dumps(ev, ensure_ascii=False) + "\n")
        p = subprocess.run(
            [sys.executable, PROJECT, src, "--out", os.path.join(d, "p.md")],
            capture_output=True, text=True,
        )
        out = ""
        ppath = os.path.join(d, "p.md")
        if os.path.exists(ppath):
            out = open(ppath, encoding="utf-8").read()
        return p.returncode, p.stderr, out


def check(name, cond, detail=""):
    if cond:
        print(f"  PASS  {name}")
    else:
        print(f"  FAIL  {name}")
        if detail:
            print(f"        {detail}")
        failures.append(name)


def ev(**kw):
    base = {"seq": 1, "actor": "开发", "stage": "本地开发", "summary": "一句话"}
    base.update(kw)
    return base


print("投影层回归：")

# 1. 真实泄漏样本 + 构造的验收用例
rc, err, out = run([json.loads(l) for l in open(FIXTURE, encoding="utf-8") if l.strip()])
check("主 fixture 正常出口", rc == 0, f"exit={rc} stderr={err[:200]}")
check("已确认的事实出口了", "活跃度数据源定不下来" in out and "users 表没有 department 字段" in out)
check("黑名单的意图没出口", "准备交给开发团队" not in out)
check("黑名单的假设没出口", "可能已经在 users 表里" not in out)
# —— 这一条就是大总管定的验收标准 ——
check(
    "「我们准备…」（类型合法但未确认）被拦下",
    "我们准备把活跃度改成只统计" not in out,
    "未确认的 change 漏到了前台",
)

# 2. fail-closed：判不出来的一律报错退出，不静默放行、也不静默丢弃
rc, err, _ = run([ev(type="change", scope="users 表", evidence="x")])
check("白名单类型缺 confirmed 标记 -> 报错退出", rc == 2, f"exit={rc}")

rc, err, _ = run([ev(type="改了点东西")])
check("未知 type -> 报错退出", rc == 2, f"exit={rc}")

rc, err, _ = run([ev(type="failure", where="x", cause="y", confirmed=True)])
check("已确认但没有 evidence -> 报错退出", rc == 2, f"exit={rc}")

rc, err, _ = run([ev(type="decision_needed", options=["就一个"], confirmed=True, evidence="x")])
check("decision_needed 少于 2 个选项 -> 报错退出", rc == 2, f"exit={rc}")

rc, err, _ = run([ev(type="observation", metric="m", window="w", value="v", confirmed=True, evidence="x")])
check("字段齐全的已确认事实 -> 正常出口", rc == 0, f"exit={rc} stderr={err[:200]}")

print()
if failures:
    print(f"FAILED: {len(failures)} 项 -> {', '.join(failures)}")
    sys.exit(1)
print("全部通过。")
