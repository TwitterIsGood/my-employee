// Command projector 是投影器的命令行入口。
//
// 用法:
//
//	projector <events.jsonl> [--out projection.md]
//
// 用法与 back/project.py 一致，退出码 2 表示有记录判不出来（fail-closed）。
package main

import (
	"fmt"
	"os"

	"github.com/TwitterIsGood/my-employee/internal/projection"
)

func main() {
	if len(os.Args) < 2 {
		die("用法: projector <events.jsonl> [--out projection.md]")
	}
	src := os.Args[1]
	dst := "projection.md"
	for i, a := range os.Args {
		if a == "--out" && i+1 < len(os.Args) {
			dst = os.Args[i+1]
		}
	}

	data, err := os.ReadFile(src)
	if err != nil {
		die(err.Error())
	}
	events, err := projection.Parse(data)
	if err != nil {
		die(err.Error())
	}
	res, err := projection.Build(events)
	if err != nil {
		die(err.Error())
	}
	if err := os.WriteFile(dst, []byte(res.Text), 0o644); err != nil {
		die(err.Error())
	}

	fmt.Printf("%d 条事件 -> 出口 %d 条，拦下 %d 条（黑名单 %d / 未定论 %d）-> %s\n",
		res.Total, res.Kept, res.Blacklist+res.Unconfirmed, res.Blacklist, res.Unconfirmed, dst)
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "projector: %s\n", msg)
	os.Exit(2)
}
