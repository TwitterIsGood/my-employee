// Command pipeline 是后台流水线的判定器。非 Agent。
//
// 它把 standards/stages/*.md 里的契约块当流水线定义，判定"这份交付，下游收不收"。
// 不通过时产出结构化驳回，并可追加一条 blocker 事件——于是驳回经 cmd/gate 投影到前台，
// 需求方看到的是"卡在哪、因为什么"，不是后台来回了几轮。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

const usage = `pipeline —— 后台流水线判定器（非 Agent）

用法：
  pipeline stages                                列出阶段契约
  pipeline gate <阶段> entry <交付物>            判入口：上游给的够不够我开工
  pipeline gate <阶段> exit  <交付物>            判出口：这份交付下游收不收

  交付物为 "-" 时读 stdin。entry 需要多个上游交付物，形如
  {"需求定义卡":{...},"方案卡":{...}}；exit 是单个交付物，形如 {...}。

  --stages <dir>   阶段 spec 目录（默认 standards/stages）
  --events <path>  不通过时追加一条 blocker 事件到后台事件日志
  --item <名字>    被驳回的需求名（默认取交付物文件名）

退出码：0 通过 / 1 被驳回 / 2 用法或读取错误。
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("缺子命令")
	}
	sub := args[0]

	fs := flag.NewFlagSet("pipeline", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stagesDir := fs.String("stages", filepath.Join("standards", "stages"), "阶段 spec 目录")
	events := fs.String("events", "", "驳回时追加 blocker 事件的 JSONL 路径")
	item := fs.String("item", "", "被驳回的需求名")

	// flag 包只认位置参数之前的选项；这个工具的用法天然是"命令 参数… --选项"，
	// 所以先把选项挑出来，再交给 flag 解析。
	flags, pos := splitFlags(args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	stages, err := pipeline.Load(*stagesDir)
	if err != nil {
		return err
	}

	switch sub {
	case "stages":
		return listStages(stages)
	case "gate":
		if len(pos) != 3 {
			fmt.Fprint(os.Stderr, usage)
			return errors.New("gate 需要 <阶段> <entry|exit> <交付物>")
		}
		return gate(stages, pos[0], pos[1], pos[2], *events, *item)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("未知子命令 %q", sub)
	}
}

// splitFlags 把 --k v / --k=v 形式的选项与位置参数分开。
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

func listStages(stages []pipeline.Stage) error {
	for _, s := range stages {
		up := s.RejectTo
		if up == "" {
			up = "—（链头，上游是人）"
		}
		fmt.Printf("%s  %-8s  产出 %-6s  入口 %d 条  出口 %d 条  驳回权 → %s\n",
			s.ID, s.Name, s.Produces, len(s.Entry), len(s.Exit), up)
	}
	return nil
}

func gate(stages []pipeline.Stage, id, dir, src, events, item string) error {
	st, ok := pipeline.ByID(stages, id)
	if !ok {
		return fmt.Errorf("没有阶段 %q", id)
	}
	if item == "" && src != "-" {
		item = filepath.Base(src)
	}
	raw, err := readSource(src)
	if err != nil {
		return err
	}

	var vs []pipeline.Violation
	switch dir {
	case "exit":
		var a pipeline.Artifact
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Errorf("exit 的交付物应是单个 JSON 对象: %w", err)
		}
		vs = st.CheckExit(a)
	case "entry":
		arts := map[string]pipeline.Artifact{}
		if err := json.Unmarshal(raw, &arts); err != nil {
			return fmt.Errorf("entry 的交付物应是 {\"上游交付物\": {...}} 形状: %w", err)
		}
		vs = st.CheckEntry(arts)
	default:
		return fmt.Errorf("方向只能是 entry 或 exit，得到 %q", dir)
	}

	if len(vs) == 0 {
		fmt.Printf("通过：阶段 %s 的%s条件全部满足\n", st.ID, dirName(dir))
		return nil
	}

	// 入口不通过 → 打回上游（可能不止一段）；出口不通过 → 退回自己重做。
	var rejs []pipeline.Rejection
	if dir == "exit" {
		rejs = []pipeline.Rejection{pipeline.SelfReject(st, item, vs)}
	} else {
		rejs = pipeline.Reject(st, item, vs)
	}

	for _, rej := range rejs {
		verb := "退回本阶段重做"
		if dir == "entry" {
			verb = "打回 " + rej.From
		}
		fmt.Printf("驳回：阶段 %s（%s）的%s条件不满足，%s\n\n", st.ID, st.Name, dirName(dir), verb)
		for i, r := range rej.Reasons {
			fmt.Printf("  %d. %s\n", i+1, r)
		}
		fmt.Println()
	}

	if events != "" {
		for _, rej := range rejs {
			ev, err := appendBlocker(events, rej)
			if err != nil {
				return err
			}
			fmt.Printf("已追加 blocker 事件 seq=%v → %s\n", ev["seq"], events)
		}
	}

	os.Exit(1)
	return nil
}

func dirName(dir string) string {
	if dir == "exit" {
		return "出口"
	}
	return "入口"
}

func readSource(src string) ([]byte, error) {
	if src == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(src)
}

// appendBlocker 把驳回写进后台事件日志。seq 递增，与投影层的输入格式一致。
func appendBlocker(path string, rej pipeline.Rejection) (map[string]any, error) {
	seq, trailing, err := nextSeq(path)
	if err != nil {
		return nil, err
	}
	ev := rej.Event()
	ev["seq"] = seq

	line, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	prefix := ""
	if !trailing {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + string(line) + "\n"); err != nil {
		return nil, err
	}
	return ev, nil
}

// nextSeq 读出现有最大 seq，返回下一个；同时告诉调用方文件末尾有没有换行。
func nextSeq(path string) (int, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 1, true, nil
		}
		return 0, false, err
	}
	if len(b) == 0 {
		return 1, true, nil
	}
	max := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if n, ok := ev["seq"].(float64); ok && int(n) > max {
			max = int(n)
		}
	}
	return max + 1, b[len(b)-1] == '\n', nil
}
