// Command stage 把后台流水线的一个阶段派给一次唤醒。非 Agent。
//
// 一次唤醒只跑一个阶段：判上游够不够开工 → 唤醒 → 判交付够不够格。
// 上游不够就打回上游；自己没交好就带原因重跑，重跑超预算就升级给人工指导者。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TwitterIsGood/my-employee/internal/dispatch"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

const usage = `stage —— 把流水线的一个阶段派给一次唤醒（非 Agent）

用法：
  stage run <阶段> [--选项]

  --stages <dir>     阶段 spec 目录（默认 standards/stages）
  --dir <dir>        交付物目录：上游产物读这里，本阶段产物写这里（必填）
  --events <path>    事件日志；空则不记
  --item <名字>      需求名
  --budget <n>       出口未过时的重跑上限（默认 3）
  --worker-cmd <c>   唤醒命令，prompt 从 stdin 进（默认见下）
  --settings <file>  传给 claude 的 --settings（挂出口与 hooks）
  --work <dir>       唤醒命令的工作目录（开发类的阶段要给代码仓库，默认同 --dir）
  --brief <file>     需求方的答复（JSON）。后台唯一的"确认"来源——
                     没有它，要需求方拍板的字段只许写 assumed

默认唤醒命令：
  claude -p --settings <settings> --dangerously-skip-permissions

  非 Agent 阶段（07 回归）用 --worker-cmd 换成"跑回归并写出报告"的脚本——
  判定逻辑完全一样，换的只是命令。

退出码：0 通过 / 1 被打回或预算用尽 / 2 用法或读取错误 / 3 停在原地等人答复。
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stage:", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, dispatch.ErrAwaitingInput):
		return 3 // 在等人，不是失败——调用方等着拿答复再来叫醒同一段
	case errors.Is(err, dispatch.ErrRejectedUpstream), errors.Is(err, dispatch.ErrBudgetExhausted):
		return 1
	}
	return 2
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("需要子命令 run")
	}

	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		stagesDir = fs.String("stages", filepath.Join("standards", "stages"), "阶段 spec 目录")
		dir       = fs.String("dir", "", "交付物目录")
		eventsLog = fs.String("events", "", "事件日志路径")
		item      = fs.String("item", "", "需求名")
		budget    = fs.Int("budget", 3, "重跑上限")
		workerCmd = fs.String("worker-cmd", "", "唤醒命令")
		settings  = fs.String("settings", "", "claude 的 --settings 文件")
		work      = fs.String("work", "", "唤醒命令的工作目录")
		brief     = fs.String("brief", "", "需求方的答复")
	)
	flags, pos := splitFlags(args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(pos) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("run 需要一个阶段编号")
	}
	if *dir == "" {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("run 需要 --dir（交付物目录）")
	}

	stages, err := pipeline.Load(*stagesDir)
	if err != nil {
		return err
	}
	if _, ok := pipeline.ByID(stages, pos[0]); !ok {
		return fmt.Errorf("没有阶段 %q", pos[0])
	}

	cmd := *workerCmd
	if cmd == "" {
		if *settings == "" {
			return errors.New("默认唤醒命令需要 --settings（挂出口与 hooks 的那份），" +
				"或者用 --worker-cmd 换一条命令")
		}
		// 唤醒命令有自己的工作目录，相对路径在那边解析会找不到文件。
		abs, err := filepath.Abs(*settings)
		if err != nil {
			return err
		}
		cmd = "claude -p --settings " + shellQuote(abs) + " --dangerously-skip-permissions"
	}

	workDir := *work
	if workDir == "" {
		workDir = *dir
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		workDir = abs
	}

	return dispatch.Runner{
		Stages:   stages,
		SpecsDir: *stagesDir,
		Dir:      *dir,
		Events:   *eventsLog,
		Budget:   *budget,
		Item:     *item,
		Brief:    *brief,
		Log:      os.Stdout,
		Worker:   dispatch.CmdWorker{Cmd: cmd, Dir: workDir, Log: nil},
	}.Run(context.Background(), pos[0])
}

// splitFlags 把 --k v / --k=v 形式的选项与位置参数分开。flag 包只认位置参数之前的
// 选项，而这个工具的用法天然是"run 03 --dir …"。
func splitFlags(args []string) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, pos
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
