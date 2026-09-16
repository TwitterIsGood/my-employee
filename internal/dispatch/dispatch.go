// Package dispatch 把"一个阶段"派给"一次唤醒"。非 Agent。
//
// 阶段之间**不靠 Agent 互相喊话**：上游的产物落成文件，下游拿文件逐条对入口条件，
// 对不上就打回。这样状态不会被两个 Agent 的对话悄悄改掉——它对得上就是一份可复现的
// 判定，对不上就是一条写明了原因的驳回。
//
// 判定一律在 Agent 之外发生。Agent 看不到判定过程，只看到"没过，因为这几条"。
// 这就是 delivery.md 说的：阀门住在 harness，不住在 Agent 里。
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/TwitterIsGood/my-employee/internal/events"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

// ErrRejectedUpstream 上游的交付不够开工，本阶段一次都没启动。
var ErrRejectedUpstream = errors.New("上游交付被打回，本阶段未启动")

// ErrBudgetExhausted 重跑预算用完仍未达出口条件——该升级给人工指导者，不是继续撞。
var ErrBudgetExhausted = errors.New("重跑预算用尽，仍未达出口条件")

// Worker 是一次唤醒的抽象。Agent 阶段与非 Agent 阶段（07 回归）只是命令不同，
// 判定逻辑完全一样。
type Worker interface {
	Run(ctx context.Context, prompt string) error
}

// CmdWorker 把唤醒实现成一条 shell 命令，prompt 从 stdin 进。
// 默认命令是 claude CLI；非 Agent 阶段换成"跑回归并写出报告"的脚本。
type CmdWorker struct {
	Cmd string
	Dir string
	Log io.Writer
}

func (w CmdWorker) Run(ctx context.Context, prompt string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", w.Cmd)
	cmd.Dir = w.Dir
	cmd.Stdin = strings.NewReader(prompt)
	if w.Log != nil {
		cmd.Stdout, cmd.Stderr = w.Log, w.Log
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	return cmd.Run()
}

// WorkerFunc 让测试能注入一个确定性的"唤醒"，不必真去调模型。
type WorkerFunc func(ctx context.Context, prompt string) error

func (f WorkerFunc) Run(ctx context.Context, prompt string) error { return f(ctx, prompt) }

// Runner 一次唤醒跑一个阶段。
type Runner struct {
	Stages   []pipeline.Stage
	SpecsDir string
	Dir      string // 交付物目录：上游产物读这里，本阶段产物写这里
	Events   string // 事件日志路径；空则不记
	Budget   int    // 出口未过时的重跑上限
	Worker   Worker
	Item     string // 需求名，进 Agent 的上下文与驳回记录
	Log      io.Writer
}

// Run 跑一个阶段：先判入口（不够开工就打回上游），再唤醒，再判出口。
func (r Runner) Run(ctx context.Context, id string) error {
	st, ok := pipeline.ByID(r.Stages, id)
	if !ok {
		return fmt.Errorf("没有阶段 %q", id)
	}
	// 交付物路径要绝对：唤醒的命令有它自己的工作目录，prompt 里给相对路径
	// 会让它写到别处去，而判定会找不到文件——那时看上去像"Agent 没交活"。
	if abs, err := filepath.Abs(r.Dir); err == nil {
		r.Dir = abs
	}

	arts, err := r.upstream(st)
	if err != nil {
		return err
	}
	if vs := st.CheckEntry(arts); len(vs) > 0 {
		if err := r.emit(pipeline.Reject(st, r.Item, vs)); err != nil {
			return err
		}
		return fmt.Errorf("%w：阶段 %s 的入口条件不满足", ErrRejectedUpstream, st.ID)
	}

	spec, err := r.spec(st)
	if err != nil {
		return err
	}
	if err := r.mark(st); err != nil {
		return err
	}

	out := r.path(st.Produces)
	var last []pipeline.Violation
	for attempt := 1; attempt <= r.Budget; attempt++ {
		r.logf("—— 阶段 %s（%s）第 %d/%d 次唤醒 ——\n", st.ID, st.Name, attempt, r.Budget)
		if err := r.Worker.Run(ctx, buildPrompt(st, spec, arts, out, r.Item, last)); err != nil {
			return fmt.Errorf("阶段 %s 的唤醒失败: %w", st.ID, err)
		}

		a, err := r.readArtifact(st)
		if err != nil {
			// 交付物读不出来也是"没达到出口条件"，走同一条打回与重跑的路，
			// 免得这里变成一个能被绕过的旁路。
			last = []pipeline.Violation{{
				Stage: st.ID,
				Cond:  pipeline.Condition{Field: st.Produces},
				Why:   err.Error(),
			}}
			r.retry(st, attempt, last)
			continue
		}

		last = st.CheckExit(a)
		if len(last) == 0 {
			r.logf("阶段 %s 的交付通过出口条件：%s\n", st.ID, st.Produces)
			return nil
		}
		r.retry(st, attempt, last)
	}

	if err := r.escalate(st, last); err != nil {
		return err
	}
	return fmt.Errorf("%w：阶段 %s 连续 %d 次未达出口条件", ErrBudgetExhausted, st.ID, r.Budget)
}

// upstream 读出本阶段入口条件指名要的交付物。缺文件不算错——那是"上游没交"，
// 由 CheckEntry 判出来并打回；但文件在而内容不是合法 JSON 是错，必须报出来。
func (r Runner) upstream(st pipeline.Stage) (map[string]pipeline.Artifact, error) {
	arts := map[string]pipeline.Artifact{}
	for _, c := range st.Entry {
		if _, seen := arts[c.From]; seen {
			continue
		}
		p := r.path(c.From)
		raw, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var a pipeline.Artifact
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("上游交付物 %s 不是合法 JSON: %w", p, err)
		}
		arts[c.From] = a
	}
	return arts, nil
}

func (r Runner) spec(st pipeline.Stage) (string, error) {
	p, err := pipeline.SpecPath(r.SpecsDir, st.ID)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r Runner) readArtifact(st pipeline.Stage) (pipeline.Artifact, error) {
	p := r.path(st.Produces)
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("没有写出交付物 %s", filepath.Base(p))
		}
		return nil, err
	}
	var a pipeline.Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("交付物 %s 不是合法 JSON：%v", filepath.Base(p), err)
	}
	return a, nil
}

func (r Runner) path(name string) string {
	return filepath.Join(r.Dir, name+".json")
}

// mark 记一句"流水线走到哪一段"。
//
// 用黑名单类型是**故意的**：阶段名是确定性事实（不猜、不报进展、不含未定论过程，
// 所以它确实没有可出口的内容），而投影层的 lastStage 读的是原始事件，
// 于是前台能答"现在在部署验证阶段"，问不出任何后台正在试什么。
func (r Runner) mark(st pipeline.Stage) error {
	return r.write(map[string]any{
		"type":     "progress",
		"stage":    st.Name,
		"summary":  "进入阶段 " + st.ID + "（" + st.Name + "）",
		"evidence": "n/a",
	})
}

// retry 记一次"没过、要重做"，**只进后台运行日志，不写事件**。
//
// 逐次重跑是过程：投影黑名单里那条 `retry` 说的就是这个。前台该看到的是终局——
// 过了，或者撞了南墙升级给人工。每一次没过的细节留下来只会让需求方读两遍同一件事。
func (r Runner) retry(st pipeline.Stage, attempt int, vs []pipeline.Violation) {
	r.logf("阶段 %s 第 %d 次未过出口条件：\n", st.ID, attempt)
	for _, v := range vs {
		r.logf("  - %s\n", v.String())
	}
}

// escalate 连续没过之后上报人工。这是流水线的出口，不是又一个重试。
func (r Runner) escalate(st pipeline.Stage, vs []pipeline.Violation) error {
	reasons := make([]string, 0, len(vs))
	for _, v := range vs {
		reasons = append(reasons, v.String())
	}
	return r.write(map[string]any{
		"type":      "blocker",
		"stage":     st.Name,
		"summary":   fmt.Sprintf("阶段 %s（%s）连续 %d 次未达出口条件，需要人工指导者介入", st.ID, st.Name, r.Budget),
		"on":        strings.Join(reasons, "；"),
		"confirmed": true,
		"evidence":  fmt.Sprintf("dispatch: 阶段 %s 出口判定 %d 次未过", st.ID, r.Budget),
	})
}

func (r Runner) emit(rejs []pipeline.Rejection) error {
	for _, rej := range rejs {
		if err := r.write(rej.Event()); err != nil {
			return err
		}
	}
	return nil
}

func (r Runner) write(ev map[string]any) error {
	if r.Events == "" {
		return nil
	}
	seq, err := events.Append(r.Events, ev)
	if err != nil {
		return err
	}
	r.logf("事件 seq=%d type=%v：%v\n", seq, ev["type"], ev["summary"])
	return nil
}

func (r Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	fmt.Fprintf(r.Log, format, args...)
}

// buildPrompt 组装这一轮的上下文。
//
// 关键的一行是"判定不在你这儿"：Agent 看不到判定过程，所以它没法跟判定讲价，
// 只能把交付物改到过为止。
func buildPrompt(st pipeline.Stage, spec string, arts map[string]pipeline.Artifact, out, item string, last []pipeline.Violation) string {
	var b strings.Builder

	fmt.Fprintf(&b, "你只做这一段：阶段 %s「%s」。\n\n", st.ID, st.Name)

	b.WriteString("## 这一段的规范（唯一事实源，照它做）\n\n")
	b.WriteString(spec)
	b.WriteString("\n\n")

	b.WriteString("## 上游交付物（你只能从这里拿输入，别的渠道的输入一律不算）\n\n")
	if len(arts) == 0 {
		b.WriteString("（无——本阶段的上游是人。）\n\n")
	} else {
		for _, from := range sortedKeys(arts) {
			fmt.Fprintf(&b, "### %s\n\n```json\n%s\n```\n\n", from, mustJSON(arts[from]))
		}
	}

	fields := make([]string, 0, len(st.Exit))
	for _, c := range st.Exit {
		fields = append(fields, c.Field)
	}
	b.WriteString("## 你要交的东西\n\n")
	fmt.Fprintf(&b, "把交付物 %s 写成 JSON，落到这个路径：\n\n    %s\n\n", st.Produces, out)
	fmt.Fprintf(&b, "这一份至少会被逐条判这些字段：%s。\n\n", strings.Join(fields, "、"))

	b.WriteString("## 判定不在你这儿\n\n")
	b.WriteString("写完之后，你的交付会被**你之外的程序**逐条对照上面规范里的出口条件判一次，判不过就整份打回重做。\n")
	b.WriteString("没有人会读你的解释：判定只看那个文件。所以——\n\n")
	b.WriteString("- 不要把结论写在对话里，写进文件；\n")
	b.WriteString("- 判不过的字段不要绕过去，也不要自己声明自己做完了；\n")
	b.WriteString("- 拿不准的就照实写拿不准（例如确认状态写 `assumed`），**不要替需求方确认**。\n\n")

	if item != "" {
		fmt.Fprintf(&b, "## 需求\n\n%s\n\n", item)
	}

	if len(last) > 0 {
		b.WriteString("## 上一轮没过，原因是这几条（照这个改，别改别的）\n\n")
		for i, v := range last {
			fmt.Fprintf(&b, "%d. %s —— %s\n", i+1, v.Cond.Field, v.Why)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func sortedKeys(m map[string]pipeline.Artifact) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 顺序稳定，prompt 才可复现。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<无法序列化: %v>", err)
	}
	return string(b)
}
