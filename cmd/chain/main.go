// Command chain 把一条需求走完整条流水线：01 → 02 → … → 07。非 Agent。
//
// cmd/stage 跑一段就退出来；这个跑到底。它比 stage 多知道两件事：
//   - 下一段是哪一段（走完了就收尾）；
//   - 被打回时退到哪一段（退到**最靠前**的那个回退点，下游结论因此作废重判）。
//
// 链路会断——停在等人（退出码 3）或撞南墙（退出码 1）。断在哪写在 chain.json 里，
// 带着 --brief 再跑一次就从那个断点接着走，不必从头。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TwitterIsGood/my-employee/internal/chain"
	"github.com/TwitterIsGood/my-employee/internal/cli"
	"github.com/TwitterIsGood/my-employee/internal/dispatch"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

const usage = `chain —— 把一条需求走完 01→07（非 Agent）

用法：
  chain run [--选项]

  --stages <dir>      阶段 spec 目录（默认 standards/stages）
  --dir <dir>         交付物目录，也是链路状态 chain.json 的落点（必填）
  --events <path>     事件日志；空则不记
  --item <名字>       需求名
  --budget <n>        每段出口未过时的重跑上限（默认 3）
  --wake-timeout <d>  一次唤醒的墙钟上限（默认 30m）。超了就掐掉、连同它拉起的
                      子进程一起收走，并留一条卡点。撞南墙会升级给人，
                      "叫不醒"不会——所以它得自己有个头
  --max-rework <n>    整条链的返工上限（默认 2）。超了就升级给人工指导者——
                      单段的 budget 管不住"03 改完 05 又打回"这种来回
  --worker-cmd <c>    唤醒命令，prompt 从 stdin 进（默认见下）
  --settings <file>   传给 claude 的 --settings（挂出口与 hooks）
  --work <dir>        唤醒命令的工作目录（默认同 --dir）
  --brief <file>      需求方的答复（JSON）。断在等人时，带上它接着走
  --restart           丢掉 chain.json，从头走（不删已有的交付物）

  --stage-worker <阶段>=<命令>
                      给某一段换唤醒命令，可重复。07 回归是非 Agent 阶段，
                      它换的只是命令，判定逻辑完全一样：
                        --stage-worker 07='./scripts/regression.sh'

默认唤醒命令：
  claude -p --settings <settings> --dangerously-skip-permissions

退出码：0 全程走完 / 1 撞南墙、某段预算用尽、或某段叫不醒 / 2 用法或读取错误 /
3 停在等人答复。撞了断的这几种，断在哪都写在 chain.json 里，接着跑就是接着走。
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "chain:", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, dispatch.ErrAwaitingInput):
		return 3 // 在等人，不是失败——答复回来接着走
	case errors.Is(err, chain.ErrNotConverging),
		errors.Is(err, dispatch.ErrRejectedUpstream),
		errors.Is(err, dispatch.ErrWakeFailed),
		errors.Is(err, dispatch.ErrBudgetExhausted):
		return 1
	}
	return 2
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("需要子命令 run")
	}

	fs := flag.NewFlagSet("chain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		stagesDir = fs.String("stages", filepath.Join("standards", "stages"), "阶段 spec 目录")
		dir       = fs.String("dir", "", "交付物目录")
		eventsLog = fs.String("events", "", "事件日志路径")
		item      = fs.String("item", "", "需求名")
		budget    = fs.Int("budget", 3, "每段的重跑上限")
		maxRework = fs.Int("max-rework", 2, "整条链的返工上限")
		wakeTO    = fs.Duration("wake-timeout", 30*time.Minute, "一次唤醒的墙钟上限")
		workerCmd = fs.String("worker-cmd", "", "唤醒命令")
		settings  = fs.String("settings", "", "claude 的 --settings 文件")
		work      = fs.String("work", "", "唤醒命令的工作目录")
		brief     = fs.String("brief", "", "需求方的答复")
		restart   = fs.Bool("restart", false, "丢掉旧状态从头走")
	)
	var stageWorkers stageWorkerFlags
	fs.Var(&stageWorkers, "stage-worker", "给某一段换唤醒命令：<阶段>=<命令>，可重复")

	flags, pos := cli.SplitFlags(args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(pos) != 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("run 不接受位置参数")
	}
	if *dir == "" {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("run 需要 --dir（交付物目录）")
	}

	stages, err := pipeline.Load(*stagesDir)
	if err != nil {
		return err
	}

	workDir := *work
	if workDir == "" {
		workDir = *dir
	}
	workDir = cli.AbsOrSame(workDir)

	workers := make(map[string]dispatch.Worker, len(stageWorkers.byStage))
	for id, c := range stageWorkers.byStage {
		if _, ok := pipeline.ByID(stages, id); !ok {
			return fmt.Errorf("--stage-worker 指到阶段 %q，但没有这个阶段", id)
		}
		workers[id] = dispatch.CmdWorker{Cmd: c, Dir: workDir}
	}

	// 每一段都被 --stage-worker 换掉了，就没必要再要一份默认命令。
	var def dispatch.Worker
	if *workerCmd != "" || anyStageWithout(stages, workers) {
		cmd := *workerCmd
		if cmd == "" {
			if cmd, err = cli.DefaultWorkerCmd(*settings); err != nil {
				return err
			}
		}
		def = dispatch.CmdWorker{Cmd: cmd, Dir: workDir}
	}

	return (&chain.Runner{
		Stages:      stages,
		SpecsDir:    *stagesDir,
		Dir:         *dir,
		Events:      *eventsLog,
		Budget:      *budget,
		MaxRework:   *maxRework,
		WakeTimeout: *wakeTO,
		Worker:      def,
		Workers:     workers,
		Item:        *item,
		Brief:       *brief,
		Restart:     *restart,
		Log:         os.Stdout,
	}).Run(context.Background())
}

func anyStageWithout(stages []pipeline.Stage, workers map[string]dispatch.Worker) bool {
	for _, s := range stages {
		if _, ok := workers[s.ID]; !ok {
			return true
		}
	}
	return false
}

// stageWorkerFlags 收集 --stage-worker，可重复。
type stageWorkerFlags struct {
	byStage map[string]string
}

func (f *stageWorkerFlags) String() string { return "" }

func (f *stageWorkerFlags) Set(v string) error {
	id, cmd, ok := strings.Cut(v, "=")
	id = strings.TrimSpace(id)
	if !ok || id == "" || strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("要写成 <阶段>=<命令>，收到的是 %q", v)
	}
	if f.byStage == nil {
		f.byStage = map[string]string{}
	}
	f.byStage[id] = cmd
	return nil
}
