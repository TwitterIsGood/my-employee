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

// 事件里是英文键，前台看到的是中文——读的人是需求方，不是读日志的人。
//
// 这条一直没被走过：change / observation 两类事件在"没人发"的那段时间里一条都没有，
// render 里那句直接印 key，于是字段名原样漏到了前台（`metric: …`）。
func TestFieldNamesAreRenderedInChinese(t *testing.T) {
	res, err := Build([]map[string]any{
		{"type": "change", "scope": "9 个文件", "confirmed": true,
			"evidence": "git diff --stat", "summary": "改了 9 个文件"},
		{"type": "observation", "metric": "延迟", "window": "100 万行", "value": "1.35s",
			"confirmed": true, "evidence": "压测日志", "summary": "慢了一个量级"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"影响面: 9 个文件", "指标: 延迟", "窗口: 100 万行", "数值: 1.35s"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("前台该看到 %q，实际:\n%s", want, res.Text)
		}
	}
}

// —— 卡点是有生命周期的 ——
//
// 一段被打回过、后来又补交通过，那条卡点就该跟着消。不消的后果是真实跑出来的：
// 链路已经走到 07、投影里写着"全部阶段已完成"，墙上却还挂着几小时前那条
// "03 被打回"——读的人不知道该不该管它。

func blocker(stage, summary string) map[string]any {
	return map[string]any{
		"type": "blocker", "stage": stage, "summary": summary,
		"on": "一条具体原因", "confirmed": true, "evidence": "pipeline: 入口判定",
	}
}

func accepted(stage string) map[string]any {
	return map[string]any{
		"type": "progress", "stage": stage, "outcome": "accepted",
		"summary": stage + " 的交付通过出口条件", "evidence": "n/a",
	}
}

func TestBlockerIsResolvedWhenThatStageIsAccepted(t *testing.T) {
	res, err := Build([]map[string]any{
		blocker("本地开发", "本地开发的交付被部署验证打回（6 条不通过）"),
		accepted("本地开发"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "打回") {
		t.Errorf("这一段后来被接受了，卡点不该还挂在墙上:\n%s", res.Text)
	}
	if res.Resolved != 1 {
		t.Errorf("应记 1 条消解，实际 %d", res.Resolved)
	}
}

func TestOpenBlockerStays(t *testing.T) {
	res, err := Build([]map[string]any{
		blocker("本地开发", "本地开发的交付被部署验证打回（6 条不通过）"),
		// 只是重新进入这一段，还没交出东西——那不算解决。
		map[string]any{"type": "progress", "stage": "本地开发", "summary": "进入阶段 03", "evidence": "n/a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "打回") {
		t.Errorf("还没补交，卡点不该消:\n%s", res.Text)
	}
	if res.Resolved != 0 {
		t.Errorf("不该有消解，实际 %d", res.Resolved)
	}
}

// 消解只认**更晚**的接受：先通过、再被打回，卡点是新的那个，不许被旧账抹掉。
func TestLaterBlockerIsNotResolvedByAnEarlierAccept(t *testing.T) {
	res, err := Build([]map[string]any{
		accepted("本地开发"),
		blocker("本地开发", "本地开发的交付被部署验证打回（6 条不通过）"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "打回") {
		t.Errorf("先通过后被打回，卡点是在后的那个，不该被旧账抹掉:\n%s", res.Text)
	}
}

// 「需要你决定」同理：问题答过了、那一段接着往下走了，就不该再摆在需求方面前——
// 同一件事问两遍比不问还坏。
func TestDecisionNeededIsResolvedToo(t *testing.T) {
	res, err := Build([]map[string]any{
		{"type": "decision_needed", "stage": "需求澄清", "summary": "活跃度按什么算 —— 需要需求方定一个方向",
			"options":   []any{"只算登录：漏掉只读不写的人", "登录+业务操作：埋点改造，晚一周"},
			"confirmed": true, "evidence": "阶段 01 交付物 待决策[0]"},
		accepted("需求澄清"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "需要你决定") {
		t.Errorf("答复已经回来了，不该再问一遍:\n%s", res.Text)
	}
	if res.Resolved != 1 {
		t.Errorf("应记 1 条消解，实际 %d", res.Resolved)
	}
}

// 已确认的失败/变更/观测是**发生过的事**，撤不掉，也不该撤。
func TestPastFactsAreNeverResolutioned(t *testing.T) {
	res, err := Build([]map[string]any{
		{"type": "failure", "stage": "本地开发", "summary": "聚合查询退化为顺序扫描",
			"where": "internal/stats/daily.go:88", "cause": "缺复合索引",
			"confirmed": true, "evidence": "EXPLAIN 输出"},
		accepted("本地开发"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "退化为顺序扫描") {
		t.Errorf("失败是发生过的事，不该被后来的接受撤掉:\n%s", res.Text)
	}
	if res.Resolved != 0 {
		t.Errorf("不该有消解，实际 %d", res.Resolved)
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
	if !strings.Contains(res.Text, "数值: 0\n") {
		t.Errorf("数字渲染不对:\n%s", res.Text)
	}
}
