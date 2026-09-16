#!/usr/bin/env python3
"""前台 Agent 的上下文闸门（Claude Code UserPromptSubmit hook）。

闸门住在 harness，不住在 Agent 里：
  - Agent 拿不到后台事件日志的路径，也没有理由去找；
  - 它每一轮的上下文都是本进程算出来的投影；
  - 本进程**绝不输出原始事件**——这是它存在的全部意义。

失效方向是 fail-closed：投影器一旦报错，注入的不是"什么都没有"，而是
一条"后台状态不可读、禁止陈述任何后台进展"的约束。放空比放错安全，
但放任 Agent 自由发挥比两者都危险。

出口码始终是 0。UserPromptSubmit 的 exit 2 会**抹掉需求方那句话**，
那是另一种事故。

告警写文件、不写 systemMessage：systemMessage 会显示在需求方眼前，
而运维信息属于后台过程，不属于交付内容（见 standards/projection.md）。
"""
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT = os.path.join(os.path.dirname(HERE), "back", "project.py")

PREAMBLE = """## 后台状态（你唯一能看到的后台信息）

以下是投影层算出来的、**已确认**的事实。你看不到后台的原始过程，这是设计如此。

"""

POSTAMBLE = """

---
以上之外的后台进展、方案、计划、猜测，你既不知道，也不许编。
需求方问起超出这张表的东西，就照实说还不知道。
"""

NO_STATE = """## 后台状态：暂不可读

投影层报错了（已记入运维日志），你现在**不知道**后台的任何进展。

不要陈述任何后台进展、计划或结论。需求方问起，就说暂时联系不上后台、稍后回他。
"""


def emit(context):
    print(json.dumps(
        {"hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit",
            "additionalContext": context,
        }},
        ensure_ascii=False,
    ))


def alarm(path, msg):
    try:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(f"[{time.strftime('%Y-%m-%dT%H:%M:%S')}] {msg}\n")
    except Exception:
        pass  # 告警写不进去也不能影响需求方的这一轮对话


def main():
    alarm_log = os.environ.get("AI_EMPLOYEE_ALARM_LOG", "/tmp/ai-employee-alarm.log")
    try:
        json.load(sys.stdin)  # hook 的入参，本闸门暂时不用，但必须读掉
    except Exception:
        pass

    events = os.environ.get("AI_EMPLOYEE_EVENTS", "")
    projection_path = os.environ.get("AI_EMPLOYEE_PROJECTION", "")

    if not events or not os.path.exists(events):
        alarm(alarm_log, f"events 文件不可读，已 fail-closed 到「状态不可读」: {events!r}")
        emit(NO_STATE)
        return

    args = [sys.executable, PROJECT, events]
    if projection_path:
        args += ["--out", projection_path]
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=20)
    except Exception as e:
        alarm(alarm_log, f"投影器执行失败，已 fail-closed: {e}")
        emit(NO_STATE)
        return

    if p.returncode != 0:
        alarm(alarm_log, f"投影器报错(exit={p.returncode})，已 fail-closed: {(p.stderr or '').strip()[:300]}")
        emit(NO_STATE)
        return

    try:
        with open(projection_path, encoding="utf-8") as fh:
            body = fh.read()
    except Exception as e:
        alarm(alarm_log, f"投影读不回来，已 fail-closed: {e}")
        emit(NO_STATE)
        return

    emit(PREAMBLE + body + POSTAMBLE)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # 闸门自己崩了也要 fail-closed，不能把这一轮放过去
        emit(NO_STATE)
        alarm(os.environ.get("AI_EMPLOYEE_ALARM_LOG", "/tmp/ai-employee-alarm.log"),
              f"闸门未捕获异常，已 fail-closed: {e!r}")
