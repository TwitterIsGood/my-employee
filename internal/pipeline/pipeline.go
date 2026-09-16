// Package pipeline 是后台流水线的状态机。非 Agent。
//
// 阶段定义**不在这里**——它住在 standards/stages/*.md 的机器可读契约块里。
// 这个包只负责加载、校验、判定。理由和 delivery.md 一样：规则只有一个事实源，
// 散文给 Agent 读，谓词给闸门判，两者写在同一个文件里，不许各写一份。
//
// 核心是驳回权：**阶段 N 的入口条件，就是阶段 N-1 交付物的验收标准。**
// 下游拿上游的产物逐条对入口条件，对不上就打回。判断标准是写死的谓词，
// 不是"这份交付够不够好"这种没法复现的感觉。
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Condition 是一条可判定的条件。
type Condition struct {
	From  string      `json:"from,omitempty"` // 针对哪个上游交付物；出口条件留空
	When  []Condition `json:"when,omitempty"` // 前置：全部成立本条才生效
	Field string      `json:"field"`
	Check string      `json:"check"`
	Value any         `json:"value,omitempty"`
}

// Stage 是一个阶段的机器可读契约，附在对应的 spec 文件里。
type Stage struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Produces string      `json:"produces"`
	Entry    []Condition `json:"entry"`
	Exit     []Condition `json:"exit"`
	RejectTo string      `json:"reject_to,omitempty"`
	// 入口条件来自不同上游时，各自该打回的地方不一样：06 的变更卡缺字段打回 03
	// （05 复审），而审查记录的问题打回 05。这里按交付物名覆盖 RejectTo。
	RejectUpstream map[string]string `json:"reject_upstream,omitempty"`
	// PauseOn 指向交付物里的一个字段：它非空（且形如决策项）时，本阶段是在**等人**，
	// 不是在失败。例：01 的口径定不下来，就该把取舍摆给需求方然后停住——
	// 停住是一次正常的中断，不是撞南墙；重跑多少次都问不出需求方的答案。
	PauseOn string `json:"pause_on,omitempty"`
}

// Artifact 是一个交付物：字段名 -> 值。
type Artifact map[string]any

// Violation 是一条没通过的条件。驳回时逐条列出来，附证据。
type Violation struct {
	Stage string
	Cond  Condition
	Why   string
}

func (v Violation) String() string {
	target := v.Cond.Field
	if v.Cond.From != "" {
		target = v.Cond.From + "." + v.Cond.Field
	}
	return fmt.Sprintf("%s: %s（%s）", target, v.Why, describe(v.Cond.Check))
}

func describe(check string) string {
	switch check {
	case "present":
		return "必须显式给出"
	case "nonempty":
		return "不能为空"
	case "empty":
		return "必须为空"
	case "equals":
		return "值必须相等"
	case "one_of":
		return "值必须落在允许集合里"
	case "is_true":
		return "必须为 true"
	case "min_items":
		return "条数不足"
	}
	return check
}

const contractHeading = "## 契约（机器可读）"

// Load 从目录里读所有阶段 spec，取出机器可读契约块。
// 读不到契约块的文件直接报错——没有谓词的阶段视为未落地（delivery.md 的同一条纪律）。
func Load(dir string) ([]Stage, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "[0-9][0-9]-*.md"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("在 %s 里没找到阶段 spec", dir)
	}
	sort.Strings(matches)

	var stages []Stage
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		block, err := contractBlock(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		var st Stage
		if err := json.Unmarshal([]byte(block), &st); err != nil {
			return nil, fmt.Errorf("%s: 契约块不是合法 JSON: %w", filepath.Base(p), err)
		}
		stages = append(stages, st)
	}
	if err := validateChain(stages); err != nil {
		return nil, err
	}
	return stages, nil
}

// contractBlock 取出 "## 契约（机器可读）" 之后的第一个 ```json 围栏。
func contractBlock(doc string) (string, error) {
	i := strings.Index(doc, contractHeading)
	if i < 0 {
		return "", fmt.Errorf("没有 %q 小节", contractHeading)
	}
	rest := doc[i:]
	start := strings.Index(rest, "```json")
	if start < 0 {
		return "", fmt.Errorf("%q 小节里没有 ```json 块", contractHeading)
	}
	rest = rest[start+len("```json"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", fmt.Errorf("```json 块没有闭合")
	}
	return strings.TrimSpace(rest[:end]), nil
}

// validateChain 保证阶段树是一条首尾相接的链：编号连续，
// 每个阶段的 reject_to 指向上一个阶段。
func validateChain(stages []Stage) error {
	for i, s := range stages {
		want := fmt.Sprintf("%02d", i+1)
		if s.ID != want {
			return fmt.Errorf("阶段编号不连续：第 %d 个是 %q，应为 %q", i+1, s.ID, want)
		}
		if i == 0 {
			if s.RejectTo != "" {
				return fmt.Errorf("阶段 %s 是链头，不该有 reject_to", s.ID)
			}
			continue
		}
		if s.RejectTo != stages[i-1].ID {
			return fmt.Errorf("阶段 %s 的 reject_to=%q，但上游是 %s——链条断了",
				s.ID, s.RejectTo, stages[i-1].ID)
		}
	}
	// reject_upstream 写错阶段号，驳回就会寄到没人收的地方——那比不驳回更坏。
	for _, s := range stages {
		for art, to := range s.RejectUpstream {
			if _, ok := ByID(stages, to); !ok {
				return fmt.Errorf("阶段 %s 把 %q 的打回目标写成 %q，但没有这个阶段",
					s.ID, art, to)
			}
		}
	}
	return nil
}

// SpecPath 找某个阶段的 spec 文件。散文形态与谓词形态住在同一个文件里，
// 所以唤醒一个阶段的 Agent 时给它读的就是这一份，避免两处各说各话。
func SpecPath(dir, id string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, id+"-*.md"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("在 %s 里没有阶段 %s 的 spec", dir, id)
	}
	return matches[0], nil
}

// ByID 取某个阶段。
func ByID(stages []Stage, id string) (Stage, bool) {
	for _, s := range stages {
		if s.ID == id {
			return s, true
		}
	}
	return Stage{}, false
}

// holds 判定一条已取值条件的真假。字段是否存在由调用方先判。
func holds(c Condition, got any) bool {
	switch c.Check {
	case "present":
		return true
	case "nonempty":
		return !isEmpty(got)
	case "empty":
		return isEmpty(got)
	case "is_true":
		b, ok := got.(bool)
		return ok && b
	case "equals":
		return fmt.Sprint(got) == fmt.Sprint(c.Value)
	case "one_of":
		want, ok := c.Value.([]any)
		if !ok {
			return false
		}
		for _, w := range want {
			if fmt.Sprint(got) == fmt.Sprint(w) {
				return true
			}
		}
		return false
	case "min_items":
		n, ok := c.Value.(float64)
		items, ok2 := got.([]any)
		return ok && ok2 && float64(len(items)) >= n
	}
	return false
}

// why 给出不通过的原因；返回空字符串表示通过。
func why(c Condition, got any, present bool) string {
	if !present {
		if c.Check == "present" {
			return "交付物里没有这个字段——本阶段要求显式给出，哪怕为空"
		}
		return "交付物里没有这个字段"
	}
	switch c.Check {
	case "present":
		return ""
	case "nonempty":
		if isEmpty(got) {
			return "是空的"
		}
	case "empty":
		if !isEmpty(got) {
			return "非空，但这一步不该带着未澄清的东西往下走"
		}
	case "is_true":
		if !holds(c, got) {
			return "不是 true"
		}
	case "equals":
		if !holds(c, got) {
			return fmt.Sprintf("是 %v，要求 %v", got, c.Value)
		}
	case "one_of":
		if !holds(c, got) {
			return fmt.Sprintf("是 %v，只接受 %v", got, c.Value)
		}
	case "min_items":
		if !holds(c, got) {
			items, ok := got.([]any)
			if !ok {
				return "不是数组"
			}
			n, _ := c.Value.(float64)
			return fmt.Sprintf("只有 %d 条，要求至少 %d 条", len(items), int(n))
		}
	default:
		return fmt.Sprintf("未知的判定 %q——写不出谓词的条件不该被批准上线", c.Check)
	}
	return ""
}

// evaluate 判定一条条件。
//
// 前置条件（When）里任何一个字段缺失，本条按**不通过**处理——一条说不清前置的
// 条件等于一条会被静默跳过闸门，那正是这个项目要避免的东西。前置成立与否可以判定、
// 但判定为否时，本条不适用（例如"通过"才要求有未覆盖范围）。
func evaluate(c Condition, a Artifact) (violated bool, reason string) {
	for _, w := range c.When {
		got, present := a[w.Field]
		if !present {
			return true, fmt.Sprintf("前置条件 %s 缺失，无法判定本条", w.Field)
		}
		if !holds(w, got) {
			return false, ""
		}
	}
	if r := why(c, a[c.Field], has(a, c.Field)); r != "" {
		return true, r
	}
	return false, ""
}

func has(a Artifact, field string) bool {
	_, ok := a[field]
	return ok
}

// CheckExit 判定本阶段的出口条件：这份交付，下游收不收。
func (s Stage) CheckExit(a Artifact) []Violation {
	var vs []Violation
	for _, c := range s.Exit {
		if v, r := evaluate(c, a); v {
			vs = append(vs, Violation{Stage: s.ID, Cond: c, Why: r})
		}
	}
	return vs
}

// CheckEntry 判定入口条件：上游的交付物够不够我开工。
// arts 的键是交付物名字（对应 Condition.From）。
func (s Stage) CheckEntry(arts map[string]Artifact) []Violation {
	var vs []Violation
	for _, c := range s.Entry {
		a, ok := arts[c.From]
		if !ok {
			vs = append(vs, Violation{Stage: s.ID, Cond: c, Why: "上游交付物缺失: " + c.From})
			continue
		}
		if v, r := evaluate(c, a); v {
			vs = append(vs, Violation{Stage: s.ID, Cond: c, Why: r})
		}
	}
	return vs
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Rejection 是一次驳回：谁把谁的活打回去了，因为哪几条。
type Rejection struct {
	From    string   `json:"from"`    // 打回给谁（上游阶段）
	By      string   `json:"by"`      // 谁打回的
	Item    string   `json:"item"`    // 哪个需求
	Reasons []string `json:"reasons"` // 逐条不通过的原因
}

func reasons(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.String())
	}
	return out
}

// target 是这条不通过项该打回给谁。
func (s Stage) target(c Condition) string {
	if to, ok := s.RejectUpstream[c.From]; ok {
		return to
	}
	return s.RejectTo
}

// Reject 依据**入口条件**的不通过项产出驳回，按打回目标分组——一组一条。
//
// 一次入口判定可能同时退回两段：06 的变更卡缺字段回 03，审查记录本身的问题回 05。
// 合成一条"退回 05"会让 03 不知道要补东西，所以分组是必须的，不是讲究。
//
// 组按阶段号升序，不按驳回产生的先后：这一份要经投影摆到前台，也要被驱动整条链路的
// 一层拿去算回退点，两种用途要的都是链路上的顺序，不是内部遍历的顺序。
func Reject(stage Stage, item string, vs []Violation) []Rejection {
	grouped := map[string][]Violation{}
	for _, v := range vs {
		t := stage.target(v.Cond)
		grouped[t] = append(grouped[t], v)
	}
	order := make([]string, 0, len(grouped))
	for t := range grouped {
		order = append(order, t)
	}
	sort.Strings(order)

	out := make([]Rejection, 0, len(order))
	for _, t := range order {
		out = append(out, Rejection{
			From: t, By: stage.ID, Item: item, Reasons: reasons(grouped[t]),
		})
	}
	return out
}

// SelfReject 是**出口条件**不通过：这份交付达不到下游的门槛，退回本阶段重做。
// 它不打回上游——上游没错，是这一段自己的活没干完。
func SelfReject(stage Stage, item string, vs []Violation) Rejection {
	return Rejection{From: stage.ID, By: stage.ID, Item: item, Reasons: reasons(vs)}
}

// Event 把驳回变成一条后台事件，类型 blocker。
//
// 这一条是后台与前台的接缝：驳回在这里产出，经 cmd/gate 投影后，需求方看到的
// 是"卡在哪、因为哪几条"——看不到后台来回打了几轮、谁跟谁有分歧。
func (r Rejection) Event() map[string]any {
	// From == By 是出口不通过的自己退自己，措辞不能写成"被自己打回"。
	summary := fmt.Sprintf("阶段 %s 的交付被 %s 打回（%d 条不通过）", r.From, r.By, len(r.Reasons))
	evidence := fmt.Sprintf("pipeline: 阶段 %s 入口判定，需求 %q", r.By, r.Item)
	if r.From == r.By {
		summary = fmt.Sprintf("阶段 %s 的交付未达出口条件（%d 条）", r.From, len(r.Reasons))
		evidence = fmt.Sprintf("pipeline: 阶段 %s 出口判定，需求 %q", r.By, r.Item)
	}
	return map[string]any{
		"type":      "blocker",
		"stage":     r.From,
		"summary":   summary,
		"on":        strings.Join(r.Reasons, "；"),
		"confirmed": true,
		"evidence":  evidence,
	}
}
