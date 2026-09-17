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

// VerdictRoute 把"本阶段的交付里判出了一个结论"翻译成"该打回给谁"。
//
// 有的阶段交出来的不是"行不行"，而是"这份变更本身有问题"——05 对抗式审查就是。
// 它的出口条件**合法地允许** `结论 = 驳回`；可驳回不是一个终态：那份交付必须有地方可去。
//
// 少了这一段，05 会带着 `驳回` 通过自己的出口、被交给 06，而 06 的入口要求
// `结论 = 通过`，于是 06 把 05 退回来、05 照证据不改结论、再退回来——一直撞到返工上限。
// 这不是假设：真实跑一趟七段树时，05 与 06 就这么空转了四轮，一个字节都没往前挪。
//
// 触发条件复用 Condition，去向由交付物自己点名（`target_field`），
// 理由也取自交付物（`reasons_field`）——这样前台读到的是审查者写的那句人话，
// 而不是机器拼出来的字段名。
type VerdictRoute struct {
	When         []Condition `json:"when"`
	TargetField  string      `json:"target_field"`
	ReasonsField string      `json:"reasons_field,omitempty"`
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
	// Verdicts 是"出口过了、但这份交付本身判出该往回走"的去向。
	// 与 RejectUpstream 的区别是触发点：那个管入口（上游没交够），这个管裁决（交够了，但结论是打回）。
	Verdicts []VerdictRoute `json:"reject_on,omitempty"`
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

// Verdict 是一条命中的裁决路由：这份交付该退回给谁、附什么理由。
type Verdict struct {
	Target  string
	Reasons []string
}

// Route 看这份交付里有没有"该退回上游"的裁决。没命中就照常往下走。
//
// 只在出口条件**过了之后**才有意义：出口没过是这一段自己的活没干完，
// 走的是重做；出口过了却判出上游有问题，才是这里管的事。
//
// 命中但说不清退回给谁时**报错**，不是放行——一条说不清去向的驳回，
// 和一条说不清前置的闸门是同一种东西：它会被静默跳过。
func (s Stage) Route(a Artifact) (Verdict, bool, error) {
	for _, r := range s.Verdicts {
		fired, err := fires(r.When, a)
		if err != nil {
			return Verdict{}, false, fmt.Errorf("阶段 %s 的裁决条件判不了: %w", s.ID, err)
		}
		if !fired {
			continue
		}
		target := strings.TrimSpace(str(a[r.TargetField]))
		if target == "" {
			return Verdict{}, false, fmt.Errorf(
				"阶段 %s 判出这份交付该退回上游，但 %s 里没写退回给谁", s.ID, r.TargetField)
		}
		return Verdict{Target: target, Reasons: asReasons(a[r.ReasonsField])}, true, nil
	}
	return Verdict{}, false, nil
}

// fires 判一组前置是否全部成立。前置字段缺失即报错：
// 判不出来的时候说"不成立"，等于让一条闸门悄悄失效。
func fires(when []Condition, a Artifact) (bool, error) {
	for _, w := range when {
		v, ok := a[w.Field]
		if !ok {
			return false, fmt.Errorf("前置字段 %s 缺失", w.Field)
		}
		if !holds(w, v) {
			return false, nil
		}
	}
	return true, nil
}

// asReasons 把交付物里的理由字段收成一组。字符串算一条；
// 没写就返回一条占位——前台的卡点必须说得出"因为什么"，空着等于没说。
func asReasons(v any) []string {
	switch t := v.(type) {
	case nil:
		return []string{"审查者未附具体理由"}
	case string:
		if strings.TrimSpace(t) == "" {
			return []string{"审查者未附具体理由"}
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			out = append(out, str(x))
		}
		if len(out) == 0 {
			return []string{"审查者未附具体理由"}
		}
		return out
	case []string:
		if len(t) == 0 {
			return []string{"审查者未附具体理由"}
		}
		return t
	}
	return []string{str(v)}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
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
//
// 名字和编号都留着：编号是链路上的位置（判定、回退点要它），名字是给前台读的
// （"卡在本地开发"而不是"卡在 03"）。前台那行「阶段」显示的是名字，
// 卡点里却写编号，读的人就得自己去对一张表。
type Rejection struct {
	From     string   `json:"from"`      // 打回给谁（上游阶段号）
	FromName string   `json:"from_name"` // 那一段叫什么
	By       string   `json:"by"`        // 谁打回的（阶段号）
	ByName   string   `json:"by_name"`   // 那一段叫什么
	Item     string   `json:"item"`      // 哪个需求
	Reasons  []string `json:"reasons"`   // 逐条不通过的原因
	// Verdict 标出这条不是"上游没交够"，而是"交够了、但审出来有问题"。
	// 措辞和证据行都要跟着变，不然前台读到的是"06 的入口判定"——
	// 而 06 根本没参与这件事。
	Verdict bool `json:"verdict,omitempty"`
}

// label 是这一段的显示名：有名字用名字，没有就退回编号（判定不依赖名字）。
func (r Rejection) label() string { return pick(r.FromName, r.From) }

func (r Rejection) byLabel() string { return pick(r.ByName, r.By) }

func pick(name, id string) string {
	if name != "" {
		return name
	}
	return id
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
func Reject(stages []Stage, stage Stage, item string, vs []Violation) []Rejection {
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
			From: t, FromName: nameOf(stages, t),
			By: stage.ID, ByName: stage.Name,
			Item: item, Reasons: reasons(grouped[t]),
		})
	}
	return out
}

// SelfReject 是**出口条件**不通过：这份交付达不到下游的门槛，退回本阶段重做。
// 它不打回上游——上游没错，是这一段自己的活没干完。
func SelfReject(stage Stage, item string, vs []Violation) Rejection {
	return Rejection{
		From: stage.ID, FromName: stage.Name,
		By: stage.ID, ByName: stage.Name,
		Item: item, Reasons: reasons(vs),
	}
}

// RejectVerdict 依据**本阶段自己的裁决**产出驳回：出口过了，但这份交付
// 审出来上游有问题，被退回给交付物点名的那一段（05 的 `驳回至`）。
//
// 和 Reject 的区别在证据上：Reject 是"上游没交够"，那是流水线判的；
// 这条是"交够了、审出问题"，那是审查者判的，所以理由用审查者写的那句话，
// 前台读到的才是"看板会被一个请求打坏"，而不是一串字段名。
func RejectVerdict(stages []Stage, stage Stage, v Verdict, item string) Rejection {
	return Rejection{
		From: v.Target, FromName: nameOf(stages, v.Target),
		By: stage.ID, ByName: stage.Name,
		Item: item, Reasons: v.Reasons, Verdict: true,
	}
}

func nameOf(stages []Stage, id string) string {
	if s, ok := ByID(stages, id); ok {
		return s.Name
	}
	return ""
}

// Event 把驳回变成一条后台事件，类型 blocker。
//
// 这一条是后台与前台的接缝：驳回在这里产出，经 cmd/gate 投影后，需求方看到的
// 是"卡在哪、因为哪几条"——看不到后台来回打了几轮、谁跟谁有分歧。
func (r Rejection) Event() map[string]any {
	// From == By 是出口不通过的自己退自己，措辞不能写成"被自己打回"。
	// 名字之间不留空格：空格原本是「阶段 」这个前缀之后的断句，前缀去掉了，
	// 留着就成了"本地开发 的交付被 部署验证 打回"——纯中文词中间多出的空档。
	summary := fmt.Sprintf("%s的交付被%s打回（%d 条不通过）", r.label(), r.byLabel(), len(r.Reasons))
	evidence := fmt.Sprintf("pipeline: 阶段 %s 入口判定，需求 %q", r.By, r.Item)
	switch {
	case r.Verdict:
		// 出口过了、是这一段自己审出来的问题。写"入口判定"会把责任说错段：
		// 上游交够了，是审出来的毛病。
		summary = fmt.Sprintf("%s审出%s的交付有问题，退回（%d 条）", r.byLabel(), r.label(), len(r.Reasons))
		evidence = fmt.Sprintf("pipeline: 阶段 %s 的裁决，需求 %q", r.By, r.Item)
	case r.From == r.By:
		summary = fmt.Sprintf("%s的交付未达出口条件（%d 条）", r.label(), len(r.Reasons))
		evidence = fmt.Sprintf("pipeline: 阶段 %s 出口判定，需求 %q", r.By, r.Item)
	}
	// stage 写名字：卡点要能跟别处的「阶段：<名字>」对上，同一段才认得出来是同一段。
	return map[string]any{
		"type":      "blocker",
		"stage":     r.label(),
		"summary":   summary,
		"on":        strings.Join(r.Reasons, "；"),
		"confirmed": true,
		"evidence":  evidence,
	}
}
