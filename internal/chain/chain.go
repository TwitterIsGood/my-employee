// Package chain 驱动整条流水线：01 → 02 → … → 07。非 Agent。
//
// dispatch 管"一个阶段跑一次"，这个包管"一条需求走完全程"——两者的分工不是随手切的：
// 单段的判定与重跑是**过程**（投影黑名单里的 retry），走到哪一段、返工了几次才是
// 一条需求的状态。状态得落在文件里，因为这条链路会**断**：
//
//   - 停在原地等人（取值在需求方手里）——不是失败，答复回来再接着走；
//   - 撞南墙（返工到上限仍不收敛）——升级给人工指导者，也是断。
//
// 断点必须能续。所以每次推进都写 chain.json，那一个文件就是"这条需求现在在哪"。
package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TwitterIsGood/my-employee/internal/dispatch"
	"github.com/TwitterIsGood/my-employee/internal/events"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

// ErrNotConverging 来回返工到上限还没收敛。这是链路级的撞南墙：
// 单段的 Budget 管不住"03 改完 05 又打回"，只有整条链看得见这个循环。
var ErrNotConverging = errors.New("返工到上限仍未收敛")

// StatePath 是链路状态在交付物目录里的位置。它和交付物同目录，但**不是**交付物：
// 它记的是过程，投影层看不到它。
func StatePath(dir string) string { return filepath.Join(dir, "chain.json") }

// State 是"这条需求现在在哪"。它是链路唯一的断点依据——
// 不靠"目录里有哪些交付物"去猜走到哪了，那种推断会被一次驳回留下的旧产物骗过去。
type State struct {
	Item     string   `json:"item"`
	Stage    string   `json:"stage"`  // 下次该叫醒哪一段
	Passed   []string `json:"passed"` // 已过出口条件的阶段，按走过的顺序
	Rework   int      `json:"rework"` // 返工次数，只增不减
	Done     bool     `json:"done"`
	Awaiting bool     `json:"awaiting"`
	Note     string   `json:"note,omitempty"`
	// Back 是"被退回来的那一段欠着谁哪几条"。它得跟状态一起落盘——
	// 链路是会断的（等人、撞南墙都要断），断了再续时那一段如果只知道自己叫 03，
	// 就会拿着和上次一模一样的输入再交一遍：驳回权成了一道手续。
	Back    map[string][]string `json:"back,omitempty"`
	Updated string              `json:"updated"`
}

func LoadState(dir string) (State, error) {
	var s State
	raw, err := os.ReadFile(StatePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("链路状态 %s 不是合法 JSON: %w", StatePath(dir), err)
	}
	return s, nil
}

func SaveState(dir string, s State) error {
	s.Updated = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(StatePath(dir), append(raw, '\n'), 0o644)
}

// Runner 走完一条需求。
type Runner struct {
	Stages    []pipeline.Stage
	SpecsDir  string
	Dir       string // 交付物目录
	Events    string
	Budget    int // 每段的出口重跑上限
	MaxRework int // 整条链的返工上限
	Worker    dispatch.Worker
	// Workers 按阶段覆盖唤醒方式。07 回归是非 Agent 阶段，命令与其它段不同——
	// 而"换的只是命令"是这条流水线的一处设计，不是特例。
	Workers map[string]dispatch.Worker
	Item    string
	Brief   string
	Restart bool // 丢掉旧状态，从头走
	// WakeTimeout 一次唤醒的墙钟上限，交给 dispatch 管。整条链看得见"撞南墙"，
	// 但看不见"叫不醒"——一个不返回的唤醒在这里是一动不动的，连南墙都到不了。
	WakeTimeout time.Duration
	Log         io.Writer

	// back 记着"哪一段被点名要补什么"。被退回来的那段立刻就会重跑，重跑时它得看得见
	// 下游点名的条——否则它会把同一份东西再交一遍，驳回权就成了一道手续。
	// 它是 State.Back 在这次 Run 里的镜像，随状态落盘，断点续跑接得上。
	back map[string][]string
}

func (r *Runner) Run(ctx context.Context) error {
	if r.MaxRework <= 0 {
		r.MaxRework = 2
	}
	if abs, err := filepath.Abs(r.Dir); err == nil {
		r.Dir = abs
	}
	if len(r.Stages) == 0 {
		return errors.New("没有阶段可跑")
	}

	st, err := LoadState(r.Dir)
	if err != nil {
		return err
	}
	if r.Restart || st.Stage == "" {
		st = State{Stage: r.Stages[0].ID}
	}
	// 走完过的链路再跑一次，会把最后一段重做一遍。收尾之后改的是线上，不是文件。
	if st.Done {
		r.logf("需求 %q 已经走完 01→07，不再重走（要重来用 --restart）\n", st.Item)
		return nil
	}
	if r.Item != "" {
		st.Item = r.Item
	}
	// 上一趟断在这儿时欠的账，这一趟接着还。
	r.back = st.Back
	if err := SaveState(r.Dir, st); err != nil {
		return err
	}

	sink := events.Sink{Path: r.Events, Log: r.Log}

	for {
		idx, ok := index(r.Stages, st.Stage)
		if !ok {
			return fmt.Errorf("链路状态里写着阶段 %q，但没有这个阶段", st.Stage)
		}

		// 回到一段**已经过**的阶段，就是一次返工。回退点下游的结论全部作废：
		// 03 的活重做了，04 与 05 的输入跟着变了，它们原先的结论不再成立。
		if contains(st.Passed, st.Stage) {
			if st.Rework >= r.MaxRework {
				if err := r.stuck(sink, st); err != nil {
					return err
				}
				return fmt.Errorf("%w：返工 %d 次后仍在阶段 %s", ErrNotConverging, st.Rework, st.Stage)
			}
			st.Rework++
			r.logf("—— 回到阶段 %s，第 %d 次返工（上限 %d）——\n", st.Stage, st.Rework, r.MaxRework)
			st.Passed = st.Passed[:indexOf(st.Passed, st.Stage)]
			if err := SaveState(r.Dir, st); err != nil {
				return err
			}
		}

		// 上一次是停在等人，这次又没带答复来——那就还是同一个人没答。
		// 再叫醒一次只会把上一轮原样再演一遍，白烧一次唤醒。
		if st.Awaiting && strings.TrimSpace(r.Brief) == "" {
			r.logf("阶段 %s 还在等需求方的答复，这次没带答复来，不再叫醒\n", st.Stage)
			return fmt.Errorf("%w：阶段 %s 有 %s", dispatch.ErrAwaitingInput, st.Stage, st.Note)
		}

		err := r.stageRunner(st.Stage).Run(ctx, st.Stage)
		switch {
		case err == nil:
			if !contains(st.Passed, st.Stage) {
				st.Passed = append(st.Passed, st.Stage)
			}
			// 这一段已经把点名的条读进 prompt 了，账销掉。留着它，下一趟断点续跑
			// 会拿两轮以前的原因去叫醒它——那比没有原因更坏，它会把没人要的东西补一遍。
			delete(st.Back, st.Stage)
			st.Awaiting, st.Note = false, ""
			if idx+1 >= len(r.Stages) {
				st.Done = true
				if err := SaveState(r.Dir, st); err != nil {
					return err
				}
				if err := r.finish(sink, st); err != nil {
					return err
				}
				return nil
			}
			st.Stage = r.Stages[idx+1].ID
			if err := SaveState(r.Dir, st); err != nil {
				return err
			}

		case errors.Is(err, dispatch.ErrAwaitingInput):
			st.Awaiting, st.Note = true, err.Error()
			if serr := SaveState(r.Dir, st); serr != nil {
				return serr
			}
			return err

		case errors.Is(err, dispatch.ErrRejectedUpstream):
			var ur *dispatch.UpstreamRejection
			if !errors.As(err, &ur) {
				return err
			}
			// 回退点取**最靠前**的那个：打回 03 与 05 两段时，从 03 重走，
			// 04、05 会在路上重新被判一次。从 05 走会让 03 的补交没人验收。
			back, berr := r.earliest(ur.Targets)
			if berr != nil {
				return berr
			}
			r.remember(&st, ur)
			st.Stage, st.Note = back, err.Error()
			if serr := SaveState(r.Dir, st); serr != nil {
				return serr
			}

		case errors.Is(err, dispatch.ErrBudgetExhausted):
			st.Note = err.Error()
			if serr := SaveState(r.Dir, st); serr != nil {
				return serr
			}
			return err

		default:
			if serr := SaveState(r.Dir, st); serr != nil {
				return serr
			}
			return err
		}
	}
}

func (r *Runner) stageRunner(id string) dispatch.Runner {
	w := r.Worker
	if ow, ok := r.Workers[id]; ok {
		w = ow
	}
	return dispatch.Runner{
		Stages: r.Stages, SpecsDir: r.SpecsDir, Dir: r.Dir, Events: r.Events,
		Budget: r.Budget, Worker: w, Item: r.Item, Brief: r.Brief, Log: r.Log,
		WakeTimeout: r.WakeTimeout, Reasons: r.back[id],
	}
}

// remember 记下这一次驳回点了谁的名、要补什么。
//
// 上一轮的账全部作废：这一次驳回说的是另一件事，留着旧的会让被退回来的那段
// 去补一批已经没人要的东西。
func (r *Runner) remember(st *State, ur *dispatch.UpstreamRejection) {
	r.back = map[string][]string{}
	for _, rej := range ur.Rejections {
		r.back[rej.From] = append(r.back[rej.From], rej.Reasons...)
	}
	st.Back = r.back
}

// earliest 取回退点里最靠前的那一段。
func (r *Runner) earliest(targets []string) (string, error) {
	best, bestIdx := "", -1
	for _, t := range targets {
		i, ok := index(r.Stages, t)
		if !ok {
			return "", fmt.Errorf("要打回阶段 %q，但没有这个阶段", t)
		}
		if bestIdx < 0 || i < bestIdx {
			best, bestIdx = t, i
		}
	}
	if bestIdx < 0 {
		return "", errors.New("入口判定没通过，却没给出打回目标")
	}
	return best, nil
}

// finish 记一句"整条走完了"。
//
// 用黑名单类型是**故意的**，和 dispatch 的阶段标记同一个道理：完成是确定性事实，
// 没有可出口的细节。投影层的 lastStage 读的是原始事件，于是前台那行"阶段"
// 不会停在"07 回归"上——那是上一段的动作，不是这条需求的状态。
func (r *Runner) finish(sink events.Sink, st State) error {
	return sink.Write(map[string]any{
		"type":     "progress",
		"stage":    "全部阶段已完成",
		"summary":  "需求走完了 01→07，返工 " + fmt.Sprint(st.Rework) + " 次",
		"evidence": "n/a",
	})
}

// stuck 把"不收敛"上报人工指导者。这是链路的出口，不是又一次重试。
func (r *Runner) stuck(sink events.Sink, st State) error {
	// stage 写名字，跟别处的「阶段：<名字>」对得上——卡点要能被后续的接受消解，
	// 而消解是按同一个 stage 值认人的。
	name := st.Stage
	if s, ok := pipeline.ByID(r.Stages, st.Stage); ok {
		name = s.Name
	}
	return sink.Write(map[string]any{
		"type":      "blocker",
		"stage":     name,
		"summary":   fmt.Sprintf("返工 %d 次仍未收敛，需要人工指导者介入", st.Rework),
		"on":        st.Note,
		"confirmed": true,
		"evidence":  fmt.Sprintf("chain: 返工上限 %d，停在阶段 %s（%s）", r.MaxRework, st.Stage, name),
	})
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	fmt.Fprintf(r.Log, format, args...)
}

func index(stages []pipeline.Stage, id string) (int, bool) {
	for i, s := range stages {
		if s.ID == id {
			return i, true
		}
	}
	return -1, false
}

func indexOf(ids []string, id string) int {
	for i, s := range ids {
		if s == id {
			return i
		}
	}
	return -1
}

func contains(ids []string, id string) bool { return indexOf(ids, id) >= 0 }
