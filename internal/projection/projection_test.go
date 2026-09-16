package projection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 投影规则是靠一条一条实测踩出来的（验证 A 里前台确实把"准备交给开发团队"
// 这种没发生的事讲给了需求方）。规则写进文档会腐，写成断言不会。

func fixture(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "back", "fixtures", "events-active-users.jsonl"))
	if err != nil {
		t.Fatalf("读 fixture 失败: %v", err)
	}
	events, err := Parse(data)
	if err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	return events
}

func ev(kv map[string]any) []map[string]any {
	base := map[string]any{"seq": 1, "actor": "开发", "stage": "本地开发", "summary": "一句话"}
	for k, v := range kv {
		base[k] = v
	}
	return []map[string]any{base}
}

func TestFixture(t *testing.T) {
	res, err := Build(fixture(t))
	if err != nil {
		t.Fatalf("主 fixture 应该正常出口，却报错: %v", err)
	}
	if res.Kept != 4 || res.Blacklist != 7 || res.Unconfirmed != 1 || res.Total != 12 {
		t.Errorf("统计不对: total=%d kept=%d black=%d unconf=%d",
			res.Total, res.Kept, res.Blacklist, res.Unconfirmed)
	}

	// 已确认的事实必须出口
	for _, want := range []string{"活跃度数据源定不下来", "users 表没有 department 字段"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("已确认的事实没出口: %q", want)
		}
	}

	// 黑名单一律不出
	for _, bad := range []string{"准备交给开发团队", "可能已经在 users 表里", "这次会按后端数据口径"} {
		if strings.Contains(res.Text, bad) {
			t.Errorf("黑名单内容漏到前台了: %q", bad)
		}
	}

	// —— 这一条就是大总管定的验收标准 ——
	// 类型合法（change）但没发生的事，必须被拦下
	if strings.Contains(res.Text, "我们准备把活跃度改成只统计") {
		t.Error("未确认的 change 漏到了前台——类型对不代表事实对这道门没起作用")
	}
}

func TestFailClosed(t *testing.T) {
	cases := []struct {
		name string
		in   []map[string]any
	}{
		{"白名单类型缺 confirmed 标记", ev(map[string]any{"type": "change", "scope": "users 表", "evidence": "x"})},
		{"未知 type", ev(map[string]any{"type": "改了点东西"})},
		{"缺 type", ev(map[string]any{"summary": "没有类型"})},
		{"已确认但没有 evidence", ev(map[string]any{"type": "failure", "where": "x", "cause": "y", "confirmed": true})},
		{"decision_needed 少于 2 个选项", ev(map[string]any{
			"type": "decision_needed", "options": []any{"就一个"}, "confirmed": true, "evidence": "x"})},
		{"缺 summary", ev(map[string]any{
			"type": "change", "scope": "x", "confirmed": true, "evidence": "y", "summary": ""})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Build(c.in); err == nil {
				t.Error("应该报错退出，却放行了")
			}
		})
	}
}

func TestConfirmedFalseIsDroppedNotError(t *testing.T) {
	// 显式标了没发生 → 不出，但不算错（后台本来就该记在途的事）
	res, err := Build(ev(map[string]any{
		"type": "change", "scope": "users 表", "confirmed": false, "evidence": "n/a"}))
	if err != nil {
		t.Fatalf("confirmed=false 不该报错: %v", err)
	}
	if res.Kept != 0 || res.Unconfirmed != 1 {
		t.Errorf("应记为未定论 1 条: kept=%d unconf=%d", res.Kept, res.Unconfirmed)
	}
}

func TestGoodEventPasses(t *testing.T) {
	res, err := Build(ev(map[string]any{
		"type": "observation", "metric": "m", "window": "w", "value": "v",
		"confirmed": true, "evidence": "x"}))
	if err != nil {
		t.Fatalf("字段齐全的已确认事实应该出口: %v", err)
	}
	if res.Kept != 1 {
		t.Errorf("应出口 1 条，实际 %d", res.Kept)
	}
}

func TestParseRejectsBadJSON(t *testing.T) {
	if _, err := Parse([]byte("{\"a\":1}\nnot json\n")); err == nil {
		t.Error("非法 JSON 行应该报错")
	}
}

func TestNumberRendering(t *testing.T) {
	// 0 不能渲染成 0.0
	var evt map[string]any
	dec := json.NewDecoder(strings.NewReader(`{"type":"observation","metric":"m","window":"w","value":0,"confirmed":true,"evidence":"x","summary":"s"}`))
	dec.UseNumber()
	if err := dec.Decode(&evt); err != nil {
		t.Fatal(err)
	}
	res, err := Build([]map[string]any{evt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "value: 0\n") {
		t.Errorf("数字渲染不对:\n%s", res.Text)
	}
}
