// Package events 是后台事件日志的写入口。
//
// 唯一职责：分配 seq、按 JSONL 追加。它是流水线唯一的对外记录面——
// 前台看不见这个文件（连路径都拿不到），只能看见 cmd/gate 的投影结果。
package events

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// Append 追加一条事件，返回分配给它的 seq。
//
// seq 是本次写入时才定的，调用方不用关心并发以外的一致性：这个流水线是
// 顺序推进的，没有并发写入者。文件末尾缺换行时补一个，避免两条记录粘成一行
// ——粘行的 JSONL 会让投影层直接把整次投影判成失败。
func Append(path string, ev map[string]any) (int, error) {
	seq, trailing, err := nextSeq(path)
	if err != nil {
		return 0, err
	}
	ev["seq"] = seq

	line, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	prefix := ""
	if !trailing {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + string(line) + "\n"); err != nil {
		return 0, err
	}
	return seq, nil
}

// nextSeq 读出现有最大 seq，返回下一个；同时报告文件末尾有没有换行。
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
