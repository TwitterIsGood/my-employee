// Package cli 收着几个 CLI 层的小工具。它们被多个命令共用，但都不属于流水线内核。
package cli

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SplitFlags 把 --k v / --k=v 形式的选项与位置参数分开。
//
// flag 包只认位置参数之前的选项，而这些工具天然是"run 03 --dir …"的用法，
// 阶段号在前、选项在后。分完再交给 flag.Parse。
func SplitFlags(args []string) (flags, pos []string) {
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

// ShellQuote 把一个路径安全地嵌进 sh -c 的命令串里。
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// DefaultWorkerCmd 组装默认唤醒命令：claude CLI，prompt 从 stdin 进。
//
// settings 必须是绝对路径。唤醒的命令有自己的工作目录，相对路径在那边解析会
// 找不到文件——而报出来的错会是"settings file not found"，看上去像配置丢了。
func DefaultWorkerCmd(settings string) (string, error) {
	if settings == "" {
		return "", fmt.Errorf("默认唤醒命令需要 --settings（挂出口与 hooks 的那份），" +
			"或者用 --worker-cmd 换一条命令")
	}
	abs, err := filepath.Abs(settings)
	if err != nil {
		return "", err
	}
	return "claude -p --settings " + ShellQuote(abs) + " --dangerously-skip-permissions", nil
}

// AbsOrSame 尽量把路径转绝对；转不了就原样返回（让下游按原样报错）。
func AbsOrSame(p string) string {
	if p == "" {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
