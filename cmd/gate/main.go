// Command gate 是前台 Agent 的上下文闸门（Claude Code UserPromptSubmit hook）。
//
// 闸门住在 harness，不住在 Agent 里：
//   - Agent 拿不到后台事件日志的路径，也没有理由去找；
//   - 它每一轮的上下文都是本进程算出来的投影；
//   - 本进程**绝不输出原始事件**——这是它存在的全部意义。
//
// 失效方向是 fail-closed：投影一旦报错，注入的不是"什么都没有"，而是
// 一条"后台状态不可读、禁止陈述任何后台进展"的约束。放空比放错安全，
// 但放任 Agent 自由发挥比两者都危险。
//
// 出口码始终是 0。UserPromptSubmit 的 exit 2 会**抹掉需求方那句话**，
// 那是另一种事故。
//
// 告警写文件、不写 systemMessage：systemMessage 会显示在需求方眼前，
// 而运维信息属于后台过程，不属于交付内容（见 standards/projection.md）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/TwitterIsGood/my-employee/internal/projection"
)

const preamble = `## 后台状态（你唯一能看到的后台信息）

以下是投影层算出来的、**已确认**的事实。你看不到后台的原始过程，这是设计如此。

`

const postamble = `

---
以上之外的后台进展、方案、计划、猜测，你既不知道，也不许编。
需求方问起超出这张表的东西，就照实说还不知道。
`

const noState = `## 后台状态：暂不可读

投影层报错了（已记入运维日志），你现在**不知道**后台的任何进展。

不要陈述任何后台进展、计划或结论。需求方问起，就说暂时联系不上后台、稍后回他。
`

func main() {
	alarmLog := envOr("AI_EMPLOYEE_ALARM_LOG", "/tmp/ai-employee-alarm.log")
	io.Copy(io.Discard, os.Stdin) // hook 入参，本闸门暂时不用，但必须读掉

	eventsPath := os.Getenv("AI_EMPLOYEE_EVENTS")
	if eventsPath == "" {
		alarm(alarmLog, "AI_EMPLOYEE_EVENTS 未设置，已 fail-closed 到「状态不可读」")
		emit(noState)
		return
	}

	data, err := os.ReadFile(eventsPath)
	if err != nil {
		alarm(alarmLog, fmt.Sprintf("events 文件不可读，已 fail-closed: %s", eventsPath))
		emit(noState)
		return
	}

	events, err := projection.Parse(data)
	if err != nil {
		alarm(alarmLog, fmt.Sprintf("events 解析失败，已 fail-closed: %v", err))
		emit(noState)
		return
	}

	res, err := projection.Build(events)
	if err != nil {
		alarm(alarmLog, fmt.Sprintf("投影器报错，已 fail-closed: %v", err))
		emit(noState)
		return
	}

	if projPath := os.Getenv("AI_EMPLOYEE_PROJECTION"); projPath != "" {
		if err := os.WriteFile(projPath, []byte(res.Text), 0o644); err != nil {
			alarm(alarmLog, fmt.Sprintf("投影写不出去，已 fail-closed: %v", err))
			emit(noState)
			return
		}
	}

	emit(preamble + res.Text + postamble)
}

// emit 输出 hook 协议要求的 JSON。Go 的 Encoder 默认转义 HTML 字符，
// 关掉它，免得中文和引号变成 \u 转义影响可读性。
func emit(context string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": context,
		},
	})
}

func alarm(path, msg string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // 告警写不进去也不能影响需求方的这一轮对话
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02T15:04:05"), msg)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
