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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/TwitterIsGood/my-employee/internal/events"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

// ErrRejectedUpstream 上游的交付不够开工，本阶段一次都没启动。
var ErrRejectedUpstream = errors.New("上游交付被打回，本阶段未启动")

// UpstreamRejection 是"上游得先补"这件事本身，带上**打回给哪几段**。
//
// 光有一个错误值不够：驱动整条链路的一层必须知道退回哪一段才能重走。回退点取
// 最靠前的那个——03 的活重做了，04 与 05 的输入就变了，它们的结论也跟着作废。
type UpstreamRejection struct {
	Stage   string   // 本阶段（发现上游不够的那一段）
	Targets []string // 要重做的上游阶段，已去重并升序
	// Rejections 是逐条的账：谁要补什么。驱动整条链路的一层把它原样递回上游，
	// 上游才看得见自己缺的是哪几条。
	Rejections []pipeline.Rejection
}

func (e *UpstreamRejection) Error() string {
	return fmt.Sprintf("%v：阶段 %s 的入口条件不满足，打回 %s",
		ErrRejectedUpstream, e.Stage, strings.Join(e.Targets, "、"))
}

func (e *UpstreamRejection) Unwrap() error { return ErrRejectedUpstream }

// ErrBudgetExhausted 重跑预算用完仍未达出口条件——该升级给人工指导者，不是继续撞。
var ErrBudgetExhausted = errors.New("重跑预算用尽，仍未达出口条件")

// ErrAwaitingInput 本阶段把取舍摆给需求方了，停在这儿等答复。
//
// 这**不是失败**：重跑多少次都问不出需求方本人的答案。它是一次正常的中断，
// 答复回来（--brief）再叫醒同一段。把它和"撞南墙"分开，是为了不让人去修一个
// 本来就该等人回答的东西。
var ErrAwaitingInput = errors.New("在等需求方的答复")

// ErrWakeFailed 这一段的唤醒没能跑完：命令报错，或者超时被掐掉。
//
// 它和撞南墙分开：撞南墙是"跑了、判了、还是不收敛"，那是**判定**的结论，升级给人工指导者；
// 这是**一次唤醒根本没跑成**，属于基础设施层面的中断。分开记，才不会让人去改一个
// 本来没问题的交付物。
var ErrWakeFailed = errors.New("阶段唤醒失败")

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

// waitDelay 是取消之后还肯等多久。过了就不等了——见下面对孙进程的说明。
const waitDelay = 15 * time.Second

func (w CmdWorker) Run(ctx context.Context, prompt string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", w.Cmd)
	cmd.Dir = w.Dir
	cmd.Stdin = strings.NewReader(prompt)
	if w.Log != nil {
		cmd.Stdout, cmd.Stderr = w.Log, w.Log
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}

	// 取消时要杀**一整组**，不能只 kill 掉 sh。唤醒里跑的命令会自己拉后台进程
	// （起个服务、开个浏览器），只杀 sh 就等于每超时一次往机器上留一个孤儿，
	// 端口和内存慢慢堆起来。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// 上面那一刀只对 sh 这一组有效。万一还有别的进程攥着 stdout（Log 不是 *os.File
	// 时 exec 会起一根管子来搬字节，孙进程握着它，Wait 就等不到 EOF），
	// 这里就不等了。少了这一条，那个 kill 只是把一个死等换成另一个死等。
	cmd.WaitDelay = waitDelay
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
	Brief    string // 需求方的答复（JSON）；后台唯一的"确认"来源
	// Reasons 是下游把这份交付打回来时给的条，由驱动整条链路的一层递下来。
	// 少了它，上游会拿着和上次一模一样的输入再交一遍同样的东西——
	// 驳回权就成了一道手续，而不是一次补交。
	Reasons []string
	// WakeTimeout 一次唤醒的墙钟上限。它是「不会一直撞南墙」的机器形态：
	// 一个挂住的唤醒比撞南墙更坏——撞南墙会升级给人，挂住是无声的、无限的。
	// 留 0 表示用 defaultWakeTimeout。
	WakeTimeout time.Duration
	Log         io.Writer
}

// defaultWakeTimeout 给得宽：一次真实的 05 审查会自己跑脚本、复现反例，
// 实测能到二十多分钟。这里卡的是"无限"，不是"慢"。
const defaultWakeTimeout = 30 * time.Minute

func (r Runner) wakeTimeout() time.Duration {
	if r.WakeTimeout > 0 {
		return r.WakeTimeout
	}
	return defaultWakeTimeout
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
		rejs := pipeline.Reject(r.Stages, st, r.Item, vs)
		if err := r.emit(rejs); err != nil {
			return err
		}
		return &UpstreamRejection{Stage: st.ID, Targets: targets(rejs), Rejections: rejs}
	}

	spec, err := r.spec(st)
	if err != nil {
		return err
	}
	brief, err := r.brief()
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
		wctx, cancel := context.WithTimeout(ctx, r.wakeTimeout())
		err := r.Worker.Run(wctx, buildPrompt(st, spec, arts, out, r.Item, brief, r.Reasons, last))
		expired := errors.Is(wctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			// 唤醒失败要出声。原先这里只是 return，前台什么都看不到——
			// 一段叫不醒，需求方那边就是"没动静"，而没动静和"在跑"长得一模一样。
			if werr := r.wakeFailed(st, err, expired); werr != nil {
				return werr
			}
			if expired {
				return fmt.Errorf("%w：阶段 %s（%s）一次唤醒超过 %s，已终止",
					ErrWakeFailed, st.ID, st.Name, r.wakeTimeout())
			}
			return fmt.Errorf("%w：阶段 %s 的唤醒失败: %v", ErrWakeFailed, st.ID, err)
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

			// 出口过了，不等于这一趟就该往下走：交付里可能写着"上游有问题"——
			// 05 的 `结论 = 驳回` 就是。往回走要在这里拦，不能出了出口就当成了。
			v, hit, err := st.Route(a)
			if err != nil {
				return err
			}
			if hit {
				rej := pipeline.RejectVerdict(r.Stages, st, v, r.Item)
				if err := r.emit([]pipeline.Rejection{rej}); err != nil {
					return err
				}
				return &UpstreamRejection{
					Stage: st.ID, Targets: []string{v.Target},
					Rejections: []pipeline.Rejection{rej},
				}
			}
			return r.accept(st)
		}

		// 出口没过，但交付物里写着"我在等需求方拍板"——那是在等人，不是在失败。
		// 先判这个，否则重跑预算会浪费在问一个问不出答案的问题上。
		decisions, ok := pauseItems(a, st.PauseOn)
		if ok {
			if err := r.ask(st, decisions); err != nil {
				return err
			}
			return fmt.Errorf("%w：阶段 %s 有 %d 项要需求方定夺", ErrAwaitingInput, st.ID, len(decisions))
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

// accept 记一句"这一段的交付被接受了"。
//
// 它存在的唯一理由是给**卡点做消解**：一段被打回过、后来又补交通过，
// 卡点就该跟着消。没有这条事件，前台会一边写着"全部阶段已完成"，
// 一边把那条早已解决的卡点继续挂在墙上——那比不报还坏。
//
// 用黑名单类型是故意的，和 mark 同一个道理：接受是确定性事实，但没有可出口的细节，
// 它不进「已发生的变更」那几块，只作为一条**可能消解卡点的状态转移**被投影层读。
func (r Runner) accept(st pipeline.Stage) error {
	return r.write(map[string]any{
		"type":     "progress",
		"stage":    st.Name,
		"outcome":  "accepted",
		"summary":  "阶段 " + st.ID + "（" + st.Name + "）的交付通过出口条件：" + st.Produces,
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

// decision 是交付物里"要需求方定夺"的一项。
//
// 每一项**必须**带至少两个选项及各自动代价。不带代价的选项是把技术清单丢给需求方，
// 不带选项的"需要你决定"是把后台的纠结原样倒过去——两种投影层都会拦。
type decision struct {
	Question string   `json:"问题"`
	Options  []option `json:"选项"`
}

type option struct {
	Choice string `json:"选项"`
	Cost   string `json:"代价"`
}

// pauseItems 看交付物是不是在等人。ok=false 表示"不是合法的暂停"——
// 字段缺失、为空、或者写得不成形，都会掉回重跑的路。**说不清的暂停不算暂停**，
// 否则"我在等人"就成了一条绕开出口条件的捷径。
func pauseItems(a pipeline.Artifact, field string) ([]decision, bool) {
	if field == "" {
		return nil, false
	}
	raw, present := a[field]
	if !present {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var items []decision
	if err := json.Unmarshal(b, &items); err != nil || len(items) == 0 {
		return nil, false
	}
	for _, d := range items {
		if strings.TrimSpace(d.Question) == "" || len(d.Options) < 2 {
			return nil, false
		}
		for _, o := range d.Options {
			if strings.TrimSpace(o.Choice) == "" || strings.TrimSpace(o.Cost) == "" {
				return nil, false
			}
		}
	}
	return items, true
}

// ask 把待定夺的取舍摆到台面上。它们经投影后前台看得到，需求方的答复再由前台带回来。
func (r Runner) ask(st pipeline.Stage, ds []decision) error {
	for i, d := range ds {
		options := make([]any, 0, len(d.Options))
		for _, o := range d.Options {
			options = append(options, o.Choice+"："+o.Cost)
		}
		if err := r.write(map[string]any{
			"type":      "decision_needed",
			"stage":     st.Name,
			"summary":   d.Question + " —— 需要需求方定一个方向",
			"options":   options,
			"confirmed": true,
			"evidence":  fmt.Sprintf("阶段 %s 交付物 %s[%d]", st.ID, st.PauseOn, i),
		}); err != nil {
			return err
		}
	}
	return nil
}

// brief 读需求方的答复。这是后台唯一的"确认"来源——没有它，Agent 只许写 assumed。
func (r Runner) brief() (string, error) {
	if r.Brief == "" {
		return "", nil
	}
	b, err := os.ReadFile(r.Brief)
	if err != nil {
		return "", fmt.Errorf("读不到需求方答复 %s: %w", r.Brief, err)
	}
	return string(b), nil
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

// wakeFailed 把一次叫不醒的唤醒变成一条卡点。
//
// 为什么不只是往日志里写一句：叫不醒的那一段，前台那边看起来和"正在跑"一模一样，
// 都是"没动静"。需求方向它提了需求，然后就是无尽的安静——这比任何一条驳回都难查。
// 它在链路里也没有第二个信号源：不进事件日志，就没人会去管。
//
// 挂在哪一段由 st 决定，于是它按既有的消解规则走：这一段后来又交出了被接受的交付，
// 这条卡点自己就撤了。
func (r Runner) wakeFailed(st pipeline.Stage, err error, expired bool) error {
	why := err.Error()
	if expired {
		why = fmt.Sprintf("一次唤醒超过 %s 没有返回，已连同它拉起的子进程一起终止", r.wakeTimeout())
	}
	return r.write(map[string]any{
		"type":      "blocker",
		"stage":     st.Name,
		"summary":   fmt.Sprintf("阶段 %s（%s）叫不醒，链路停在原地", st.ID, st.Name),
		"on":        why,
		"confirmed": true,
		"evidence":  fmt.Sprintf("dispatch: 阶段 %s 唤醒失败", st.ID),
	})
}

// targets 把一次入口判定里的打回目标去重并排序。顺序按阶段号，不按驳回产生的
// 先后——驱动整条链路的一层要拿它算回退点，而阶段号才是链路上的位置。
func targets(rejs []pipeline.Rejection) []string {
	seen := map[string]bool{}
	var out []string
	for _, rej := range rejs {
		if !seen[rej.From] {
			seen[rej.From] = true
			out = append(out, rej.From)
		}
	}
	sort.Strings(out)
	return out
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
	return events.Sink{Path: r.Events, Log: r.Log}.Write(ev)
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
func buildPrompt(st pipeline.Stage, spec string, arts map[string]pipeline.Artifact, out, item, brief string, reasons []string, last []pipeline.Violation) string {
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

	b.WriteString("## 需求方的答复（你唯一的确认来源）\n\n")
	if strings.TrimSpace(brief) == "" {
		b.WriteString("（还没有答复。）所以：凡是要需求方本人拍板的事，**不许替他答**。\n")
		b.WriteString("把这类事写进交付物的 `待决策`（每项带 ≥2 个选项及各自动代价），本阶段就停在这儿等人。\n")
		b.WriteString("该确认的字段照实写 `assumed`，被驳回也不许改成 `confirmed`——改了就是替他做决定。\n\n")
	} else {
		b.WriteString("```json\n" + brief + "\n```\n\n")
		b.WriteString("只有这里出现的答复才算数，连确认来源（哪条消息）一起抄进交付物。\n")
		b.WriteString("这里没答到的问题，仍然写进 `待决策` 继续等，不要自己补一个答案。\n\n")
	}

	if item != "" {
		fmt.Fprintf(&b, "## 需求\n\n%s\n\n", item)
	}

	// 两种"没过"要分开说：一种是本段自己没干完（出口没过），一种是下游已经把
	// 这份交付退回来了（入口没过）。它们的改法不一样——前者是接着改，后者是补交，
	// 而补交要照下游点名的那几条补，多改的都算越界。
	if len(reasons) > 0 {
		b.WriteString("## 下游把这份交付退回来了，点名要这几条\n\n")
		for i, r := range reasons {
			fmt.Fprintf(&b, "%d. %s\n", i+1, r)
		}
		b.WriteString("\n补的就是这几条。别的一律不动——多改的是没被要求改的东西。\n\n")
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
