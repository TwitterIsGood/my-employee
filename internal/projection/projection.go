// Package projection 把后台事件日志压成前台唯一可见的状态文档。
//
// 非 Agent、纯函数、不含模型调用。规则见 standards/projection.md。
//
// 两道门：
//  1. type 必须在白名单里（黑名单/未知类型的处理见下）
//  2. 白名单类型还必须显式标 "confirmed": true —— 类型对不代表事实已成立，
//     一条 type=change 也可以是"我们准备改"。缺省不等于已确认。
//
// 任何违反规则的记录都让 Build 报错，不静默丢弃——静默丢弃会让后台
// 以为自己上报了，而前台其实什么都没收到。只有两种"不出"是正常的：
// 黑名单类型、和显式标了 confirmed=false 的在途事项。
package projection

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 白名单：只有这些类型能出口，且各自必须带齐这些字段。
var allowedRequired = map[string][]string{
	"failure":         {"where", "cause"},
	"change":          {"scope"},
	"observation":     {"metric", "window", "value"},
	"blocker":         {"on"},
	"decision_needed": {"options"},
}

// 黑名单：一律不出。判定标准是确定性，不是好坏。
var forbidden = map[string]bool{
	"retry": true, "hypothesis": true, "debate": true,
	"progress": true, "intent": true, "plan": true, "draft": true,
}

// 类型对不代表事实对。一条记录可以类型是 change、却还只是"我们准备改"，
// 所以白名单类型必须再显式声明一次：这件事到底发生了没有。
const confirmedKey = "confirmed"

var headings = []struct{ typ, title string }{
	{"blocker", "卡点"},
	{"decision_needed", "需要你决定"},
	{"failure", "已确认的失败"},
	{"change", "已发生的变更"},
	{"observation", "观测到的数字"},
}

// Result 是投影的结果，带上统计供 ops 观察。
type Result struct {
	Text        string
	Total       int
	Kept        int
	Blacklist   int
	Unconfirmed int
	Resolved    int // 因为那一段后来被接受了，已从「卡点 / 需要你决定」里撤下来的条数
}

// Parse 读 JSONL。任何一行不是合法 JSON 都直接报错，不静默跳过。
func Parse(data []byte) ([]map[string]any, error) {
	var events []map[string]any
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			return nil, fmt.Errorf("第 %d 行不是合法 JSON: %v", i+1, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

type outcome int

const (
	keep outcome = iota
	drop
)

// classify 决定一条记录的命运。返回 error 即"判不出来"，一律 fail-closed。
func classify(ev map[string]any, lineno int) (outcome, error) {
	tv, present := ev["type"]
	if !present || tv == nil {
		return drop, fmt.Errorf("第 %d 条缺 type", lineno)
	}
	t := str(tv)

	if forbidden[t] {
		return drop, nil // 黑名单：不出，且不算错（后台本来就该记过程）
	}
	if _, ok := allowedRequired[t]; !ok {
		return drop, fmt.Errorf("第 %d 条 type=%q 既不在白名单也不在黑名单——无法判定，请显式归类", lineno, t)
	}

	// 类型对不代表事实对。标了 false → 还在飞，不出；没标 → 判不出来，报错。
	cv, hasConfirmed := ev[confirmedKey]
	if !hasConfirmed {
		return drop, fmt.Errorf("第 %d 条 type=%s 没有 %s 标记——类型是白名单不等于事实已成立，请显式标 true/false",
			lineno, t, confirmedKey)
	}
	if cb, ok := cv.(bool); !ok || !cb {
		return drop, nil
	}

	// 已确认的事实：必须带证据
	if str(ev["evidence"]) == "" {
		return drop, fmt.Errorf("第 %d 条 type=%s 是白名单类型但没有 evidence——没有可复现证据的'事实'只是另一种猜测", lineno, t)
	}
	for _, f := range allowedRequired[t] {
		if isEmpty(ev[f]) {
			return drop, fmt.Errorf("第 %d 条 type=%s 缺必备字段 %q", lineno, t, f)
		}
	}
	if t == "decision_needed" {
		opts, _ := ev["options"].([]any)
		if len(opts) < 2 {
			return drop, fmt.Errorf("第 %d 条 decision_needed 只有 %d 个选项——不带选项的'需要你决定'等于把后台的纠结原样倒给需求方",
				lineno, len(opts))
		}
	}
	return keep, nil
}

// Build 校验并渲染。第一条判不出来的记录就让整次投影失败。
func Build(events []map[string]any) (Result, error) {
	res := Result{Total: len(events)}
	var kept []map[string]any
	var keptAt []int

	for i, ev := range events {
		lineno := i + 1
		oc, err := classify(ev, lineno)
		if err != nil {
			return res, err
		}
		if oc == drop {
			if forbidden[str(ev["type"])] {
				res.Blacklist++
			} else {
				res.Unconfirmed++
			}
			continue
		}
		if str(ev["summary"]) == "" {
			return res, fmt.Errorf("第 %d 条缺 summary——前台需要一句人话，不是字段堆", lineno)
		}
		kept = append(kept, ev)
		keptAt = append(keptAt, i)
	}

	live := resolve(kept, keptAt, events, &res)
	res.Kept = len(live)
	res.Text = render(live, lastStage(events), lastConfirmed(live))
	return res, nil
}

// resolve 撤下已经被解决的「卡点」与「需要你决定」。
//
// 这两类记的是**某一段当时的处境**，不是历史事实。一段被打回过、后来又补交通过，
// 那条卡点就该跟着消；一个问题被答过、那一段接着往下走了，那条"需要你决定"
// 就不该再摆在需求方面前——同一件事问两遍比不问还坏。
//
// 消解的依据是同一个 stage 上有一条更晚的 accepted：一段的交付被接受了，
// 它之前关于这一段的处境就都过去了。没有这条事件，前台会在写着
// "全部阶段已完成"的同时，把几小时前的卡点继续挂在墙上。
func resolve(kept []map[string]any, keptAt []int, events []map[string]any, res *Result) []map[string]any {
	accepted := map[string]int{}
	for i, ev := range events {
		if str(ev["outcome"]) == "accepted" {
			accepted[str(ev["stage"])] = i
		}
	}
	if len(accepted) == 0 {
		return kept
	}

	live := make([]map[string]any, 0, len(kept))
	for j, ev := range kept {
		if isSituational(str(ev["type"])) {
			if at, ok := accepted[str(ev["stage"])]; ok && at > keptAt[j] {
				res.Resolved++
				continue
			}
		}
		live = append(live, ev)
	}
	return live
}

// isSituational 是"这一段现在的处境"类的事件：过去了就该撤。
// 已确认的失败/变更/观测是**发生过的事**，撤不掉，也不该撤。
func isSituational(t string) bool { return t == "blocker" || t == "decision_needed" }

func render(kept []map[string]any, stage, last string) string {
	var b strings.Builder
	b.WriteString("# 当前状态\n\n")
	if stage == "" {
		stage = "未开始"
	}
	if last == "" {
		last = "无"
	}
	b.WriteString("- 阶段：" + stage + "\n")
	b.WriteString("- 上一个已确认的完成点：" + last + "\n\n")

	if len(kept) == 0 {
		b.WriteString("暂无已确认的事实。\n")
		return b.String()
	}

	var blocks []string
	for _, h := range headings {
		var rows []map[string]any
		for _, e := range kept {
			if str(e["type"]) == h.typ {
				rows = append(rows, e)
			}
		}
		if len(rows) == 0 {
			continue
		}
		var blk strings.Builder
		blk.WriteString("## " + h.title + "\n\n")
		for _, e := range rows {
			blk.WriteString("- **" + str(e["summary"]) + "**\n")
			for _, key := range allowedRequired[h.typ] {
				if key == "options" {
					opts, _ := e["options"].([]any)
					for i, o := range opts {
						blk.WriteString(fmt.Sprintf("  %d. %s\n", i+1, str(o)))
					}
				} else {
					blk.WriteString("  - " + key + ": " + str(e[key]) + "\n")
				}
			}
			blk.WriteString("  - 证据: " + str(e["evidence"]) + "\n")
		}
		blocks = append(blocks, strings.TrimSuffix(blk.String(), "\n"))
	}
	b.WriteString(strings.Join(blocks, "\n\n"))
	b.WriteString("\n")
	return b.String()
}

func lastStage(events []map[string]any) string {
	for i := len(events) - 1; i >= 0; i-- {
		if s := str(events[i]["stage"]); s != "" {
			return s
		}
	}
	return ""
}

func lastConfirmed(kept []map[string]any) string {
	for i := len(kept) - 1; i >= 0; i-- {
		if str(kept[i]["type"]) == "change" {
			return str(kept[i]["summary"])
		}
	}
	return ""
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

// str 把任意 JSON 值渲染成人话。json.Number 原样输出，避免 0 变成 0.0。
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return fmt.Sprintf("%v", x)
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, str(item))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%v", v)
}
