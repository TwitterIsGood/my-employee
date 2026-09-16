// Command egress-listener 是出口观测器 —— 验证「降级出口真的生效了」。
//
// 存在的理由：2026-09-16 实测发现 multica agent 的 custom_env 是**静默失效**的
// （~/.claude/settings.json 的 env 会覆盖进程环境），`agent env get` 回显正常
// 但请求根本没走那条出口。只有让请求打到自己的监听器上才看得见。
//
// 用法:
//
//	egress-listener --port 18444 --log /tmp/egress_probe.log
//
// 然后把出口指到 http://127.0.0.1:18444，发一条消息，
// 日志里出现 POST 才算这个出口真的在生效。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := flag.Int("port", 18444, "监听端口")
	logPath := flag.String("log", "/tmp/egress_probe.log", "请求日志路径")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "[%s] %s %s\n", time.Now().Format("15:04:05"), r.Method, r.URL.Path)
			for _, k := range []string{"Authorization", "X-Api-Key", "anthropic-version", "User-Agent"} {
				if v := r.Header.Get(k); v != "" {
					fmt.Fprintf(f, "    %s: %.100s\n", k, v)
				}
			}
			if len(body) > 300 {
				body = body[:300]
			}
			fmt.Fprintf(f, "    body[:300]: %q\n", body)
			f.Close()
		}

		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		if req.Model == "" {
			req.Model = "unknown"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_egress_probe", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]string{{"type": "text", "text": "EGRESS-PROBE-OK"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("listening on %s, logging to %s", addr, *logPath)
	log.Fatal(http.ListenAndServe(addr, nil))
}
