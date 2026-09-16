package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TwitterIsGood/my-employee/internal/events"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
)

// 这一层守的是**屏障**：上游没交齐，下游一次都不许启动；
// 自己没交好，只许带原因重跑，不许自己宣布做完了。

func specsDir() string { return filepath.Join("..", "..", "standards", "stages") }

func loadStages(t *testing.T) []pipeline.Stage {
	t.Helper()
	st, err := pipeline.Load(specsDir())
	if err != nil {
		t.Fatalf("加载阶段契约失败: %v", err)
	}
	return st
}

func write(t *testing.T, dir, name string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writer 造一个"唤醒"：把交付物写成给定内容。calls 记被唤醒了几次。
func writer(dir, produces string, artifacts ...map[string]any) (*int, WorkerFunc) {
	calls := 0
	return &calls, func(_ context.Context, _ string) error {
		i := calls
		calls++
		if i >= len(artifacts) {
			return nil // 没给内容就不写——正好用来验"缺交付物也算没过"
		}
		if artifacts[i] == nil {
			return nil // 故意什么都不写
		}
		b, _ := json.Marshal(artifacts[i])
		return os.WriteFile(filepath.Join(dir, produces+".json"), b, 0o644)
	}
}

// 01 的上游是人，没有入口条件。产出的卡里确认状态是 assumed，就该被打回重做——
// **Agent 不许替需求方确认**，这是这一层最要紧的一条。
func TestCannotSelfConfirm(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	assumed := map[string]any{
		"目标": "做活跃度看板", "范围内": []string{"日活"},
		"范围外": []string{"不做留存"}, "验收标准": "p99 < 500ms",
		"确认状态": "assumed", // 需求方还没确认
	}
	confirmed := map[string]any{}
	for k, v := range assumed {
		confirmed[k] = v
	}
	confirmed["确认状态"] = "confirmed"
	confirmed["确认来源"] = "im 会话 3f2a… 消息 #12"

	calls, w := writer(dir, "需求定义卡", assumed, assumed, confirmed)
	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "01")

	if err != nil {
		t.Fatalf("第三次确认了就不该失败: %v", err)
	}
	if *calls != 3 {
		t.Errorf("应重跑到第三次才过，实际唤醒了 %d 次", *calls)
	}

	// 中间那两次没过是**过程**（黑名单里的 retry），不该出现在事件里——
	// 否则需求方会把同一件事读两遍。
	for _, e := range readEvents(t, eventsLog) {
		if e["type"] == "blocker" {
			t.Errorf("中途重跑不该写 blocker，实际 %v", e)
		}
	}
}

// 重跑时 prompt 必须带上上一轮的原因，否则就是让它盲猜。
func TestRetryCarriesReasonsBack(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")

	bad := map[string]any{
		"目标": "做看板", "范围内": []string{"日活"}, "范围外": []string{},
		"验收标准": "p99 < 500ms", "确认状态": "confirmed",
	}
	calls := 0
	w := WorkerFunc(func(_ context.Context, p string) error {
		calls++
		os.WriteFile(promptPath, []byte(p), 0o644)
		b, _ := json.Marshal(bad)
		return os.WriteFile(filepath.Join(dir, "需求定义卡.json"), b, 0o644)
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Budget: 2, Worker: w,
	}.Run(context.Background(), "01")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("这份卡永远过不了，应报预算用尽，实际 %v", err)
	}

	second := string(mustRead(t, promptPath))
	if !strings.Contains(second, "上一轮没过") || !strings.Contains(second, "范围外") {
		t.Errorf("第二轮 prompt 该点名上一轮没过的字段，实际:\n%s", second)
	}
}

// 上游交付物缺失 → 一次都不唤醒。屏障的意义就在这里。
func TestMissingUpstreamNeverWakes(t *testing.T) {
	dir := t.TempDir()
	calls, w := writer(dir, "方案卡") // 没有需求定义卡
	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Budget: 3, Worker: w,
	}.Run(context.Background(), "02")

	if !errors.Is(err, ErrRejectedUpstream) {
		t.Fatalf("上游缺失应报 ErrRejectedUpstream，实际 %v", err)
	}
	if *calls != 0 {
		t.Errorf("上游没交齐就唤醒了 %d 次——屏障被绕过", *calls)
	}
}

// 上游交了个边界没想清楚的卡（范围外为空）→ 打回 01，02 不启动。
func TestVagueUpstreamRejectsAndPointsAtProducer(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")
	write(t, dir, "需求定义卡", map[string]any{
		"目标": "做看板", "范围内": []string{"日活"},
		"范围外": []string{}, "验收标准": "p99 < 500ms", "确认状态": "confirmed",
	})
	calls, w := writer(dir, "方案卡")

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "02")

	if !errors.Is(err, ErrRejectedUpstream) {
		t.Fatalf("应报上游被打回，实际 %v", err)
	}
	if *calls != 0 {
		t.Errorf("02 不该被唤醒，实际 %d 次", *calls)
	}
	evs := readEvents(t, eventsLog)
	if len(evs) != 1 || evs[0]["type"] != "blocker" {
		t.Fatalf("应留下一条 blocker，实际 %v", evs)
	}
	if evs[0]["stage"] != "01" {
		t.Errorf("该打回 01（阶段列应显示 01），实际 %v", evs[0]["stage"])
	}
}

// 交付物没写出来 / 不是合法 JSON，也走同一条打回重跑的路，不能变成旁路。
func TestBadArtifactIsRetriedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.txt")

	calls := 0
	w := WorkerFunc(func(_ context.Context, p string) error {
		calls++
		os.WriteFile(prompt, []byte(p), 0o644)
		if calls == 1 {
			return os.WriteFile(filepath.Join(dir, "需求定义卡.json"), []byte("{不是 JSON"), 0o644)
		}
		return os.WriteFile(filepath.Join(dir, "需求定义卡.json"), []byte(`{
			"目标":"做活跃度看板","范围内":["日活"],"范围外":["不做留存"],
			"验收标准":"p99 < 500ms","确认状态":"confirmed",
			"确认来源":"im 会话 3f2a… 消息 #12"}`), 0o644)
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Budget: 3, Worker: w,
	}.Run(context.Background(), "01")
	if err != nil {
		t.Fatalf("第二轮改好了就该过: %v", err)
	}
	if calls != 2 {
		t.Errorf("该重跑一次，实际唤醒 %d 次", calls)
	}
	// 第一次的坏 JSON 必须把原因带回给第二轮。
	second := string(mustRead(t, prompt))
	if !strings.Contains(second, "上一轮没过") || !strings.Contains(second, "合法 JSON") {
		t.Errorf("第二轮 prompt 该带上第一轮的原因，实际:\n%s", second)
	}
}

// 预算用尽 = 撞南墙，必须升级给人工，而不是无限重试。
func TestBudgetExhaustedEscalates(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")
	// 每次都交一张没有"范围外"的卡：永远过不了。
	calls, w := writer(dir, "需求定义卡", map[string]any{
		"目标": "做看板", "范围内": []string{"日活"},
		"范围外": []string{}, "验收标准": "p99 < 500ms", "确认状态": "confirmed",
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "01")

	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("预算用尽应报 ErrBudgetExhausted，实际 %v", err)
	}
	if *calls != 3 {
		t.Errorf("预算 3 就该只唤醒 3 次，实际 %d 次", *calls)
	}

	evs := readEvents(t, eventsLog)
	last := evs[len(evs)-1]
	if last["type"] != "blocker" {
		t.Fatalf("最后一条应是 blocker，实际 %v", last["type"])
	}
	if !strings.Contains(str(last["summary"]), "人工指导者") {
		t.Errorf("升级要指明找人工指导者，实际 %q", last["summary"])
	}
}

// 每次唤醒前记一句"走到哪一段"。它用黑名单类型是故意的：
// 前台能答"在部署验证阶段"，但抄不到后台正在试什么。
func TestStageMarkerIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")
	_, w := writer(dir, "需求定义卡", map[string]any{
		"目标": "做看板", "范围内": []string{"日活"}, "范围外": []string{"不做留存"},
		"验收标准": "p99 < 500ms", "确认状态": "confirmed",
		"确认来源": "im 会话 3f2a… 消息 #12",
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Worker: w, Budget: 1,
	}.Run(context.Background(), "01")
	if err != nil {
		t.Fatalf("这份卡该过: %v", err)
	}

	evs := readEvents(t, eventsLog)
	if len(evs) != 1 {
		t.Fatalf("应恰好留一条事件，实际 %d 条", len(evs))
	}
	if evs[0]["type"] != "progress" {
		t.Errorf("阶段标记该用黑名单类型，实际 %v", evs[0]["type"])
	}
	if evs[0]["stage"] != "需求澄清" {
		t.Errorf("阶段标记该带阶段名，实际 %v", evs[0]["stage"])
	}
}

// 口径定不下来时，本阶段该**停住等人**，不是失败、也不是重跑。
// 重跑多少次都问不出需求方本人的答案。
func TestPausesWhenWaitingForStakeholder(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	calls := 0
	w := WorkerFunc(func(_ context.Context, _ string) error {
		calls++
		return os.WriteFile(filepath.Join(dir, "需求定义卡.json"), []byte(`{
			"目标":"做活跃度看板","范围内":["日活"],"范围外":["不做留存"],
			"验收标准":"p99 < 500ms","确认状态":"assumed",
			"待决策":[{
				"问题":"活跃的口径",
				"选项":[
					{"选项":"只统计登录时间","代价":"users.last_login_at 已有索引，今天就能出"},
					{"选项":"登录 + 业务操作","代价":"audit_log 4.2 亿行，加索引需一次停机窗口"}
				]
			}]}`), 0o644)
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "01")

	if !errors.Is(err, ErrAwaitingInput) {
		t.Fatalf("该停在原地等人，实际 %v", err)
	}
	if calls != 1 {
		t.Errorf("在等人就不该重跑，实际唤醒了 %d 次", calls)
	}

	// 取舍要摆到台面上：前台据此问需求方。
	evs := readEvents(t, eventsLog)
	var asks []map[string]any
	for _, e := range evs {
		if e["type"] == "decision_needed" {
			asks = append(asks, e)
		}
	}
	if len(asks) != 1 {
		t.Fatalf("应恰好摆出 1 项待决策，实际 %d 项", len(asks))
	}
	opts, _ := asks[0]["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("选项该有 2 个，实际 %d 个：%v", len(opts), opts)
	}
	for _, o := range opts {
		if !strings.Contains(str(o), "：") {
			t.Errorf("每个选项都要带代价，实际 %q", o)
		}
	}
	// 每条都要有证据，否则投影层会直接判整次投影失败。
	if str(asks[0]["evidence"]) == "" {
		t.Error("decision_needed 必须带 evidence")
	}
}

// "我在等人"不能成为绕开出口条件的捷径：选项不成形就不算暂停。
func TestMalformedPauseIsNotAPause(t *testing.T) {
	cases := []struct {
		name  string
		pause any
	}{
		{"字段缺失", nil},
		{"空数组", []any{}},
		{"只有一个选项", []any{map[string]any{
			"问题": "口径", "选项": []any{map[string]any{"选项": "只算登录", "代价": "快"}},
		}}},
		{"选项没有代价", []any{map[string]any{
			"问题": "口径", "选项": []any{
				map[string]any{"选项": "只算登录", "代价": ""},
				map[string]any{"选项": "带业务操作", "代价": "要停机"},
			},
		}}},
		{"没有问题", []any{map[string]any{
			"问题": "", "选项": []any{
				map[string]any{"选项": "A", "代价": "x"},
				map[string]any{"选项": "B", "代价": "y"},
			},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			card := map[string]any{
				"目标": "做看板", "范围内": []string{"日活"}, "范围外": []string{"不做留存"},
				"验收标准": "p99 < 500ms", "确认状态": "assumed",
			}
			if tc.pause != nil {
				card["待决策"] = tc.pause
			}
			calls, w := writer(dir, "需求定义卡", card)

			err := Runner{
				Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
				Budget: 2, Worker: w,
			}.Run(context.Background(), "01")

			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("不成形的暂停该走重跑→升级，实际 %v", err)
			}
			if *calls != 2 {
				t.Errorf("该重跑到预算用尽，实际 %d 次", *calls)
			}
		})
	}
}

// 答复回来之后，同一段应当能交出通过出口条件的卡。
func TestBriefUnblocksTheStage(t *testing.T) {
	dir := t.TempDir()
	brief := filepath.Join(dir, "brief.json")
	os.WriteFile(brief, []byte(`{"答复":[{"问题":"活跃的口径","答复":"只统计登录时间",
		"来源":"im 会话 3f2a… 消息 #12","确认时间":"2026-09-16T06:10:00Z"}]}`), 0o644)

	var got string
	w := WorkerFunc(func(_ context.Context, p string) error {
		got = p
		return os.WriteFile(filepath.Join(dir, "需求定义卡.json"), []byte(`{
			"目标":"做活跃度看板","范围内":["日活"],"范围外":["不做留存"],
			"验收标准":"p99 < 500ms","确认状态":"confirmed",
			"确认来源":"im 会话 3f2a… 消息 #12"}`), 0o644)
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Brief: brief, Budget: 1, Worker: w,
	}.Run(context.Background(), "01")
	if err != nil {
		t.Fatalf("有答复就该过: %v", err)
	}
	if !strings.Contains(got, "消息 #12") {
		t.Errorf("prompt 该把答复原文带进去，实际:\n%s", got)
	}
	if !strings.Contains(got, "连确认来源") {
		t.Error("prompt 该要求把确认来源抄进交付物")
	}
}

// 没有答复时，prompt 必须明说"不许替他答"——这条是 headless 实测里唯一真正守住的东西。
func TestNoBriefForbidsAnsweringForTheStakeholder(t *testing.T) {
	st := loadStages(t)[0] // 01
	p := buildPrompt(st, "规范正文", nil, "/tmp/卡.json", "做看板", "", nil)
	if !strings.Contains(p, "不许替他答") || !strings.Contains(p, "待决策") {
		t.Errorf("没有答复时该要求停下来问，实际:\n%s", p)
	}
}

// prompt 里必须把"判定在你自己之外"说清楚，也要把出口字段点名。
func TestPromptStatesTheBarrier(t *testing.T) {
	st := loadStages(t)[1] // 02
	p := buildPrompt(st, "规范正文", map[string]pipeline.Artifact{
		"需求定义卡": {"范围外": []any{"不做留存"}, "确认状态": "confirmed"},
	}, "/tmp/方案卡.json", "活跃度看板", "", nil)

	for _, want := range []string{
		"判定不在你这儿", "唯一事实源", "上游交付物", "/tmp/方案卡.json",
		"影响面", "不许替他答", "待决策",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺 %q\n---\n%s", want, p)
		}
	}
	if !strings.Contains(p, "不做留存") {
		t.Error("prompt 该把上游交付物的内容带进去")
	}
}

// —— helpers ——

func readEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw := mustRead(t, path)
	if len(raw) == 0 {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("事件日志不是合法 JSONL: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	return b
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// events.Append 与本包共用同一个写入路径，这里顺手确认它按 seq 递增。
func TestEventsAppendIncrementsSeq(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.jsonl")
	for want := 1; want <= 3; want++ {
		got, err := events.Append(p, map[string]any{"type": "blocker"})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("seq 应为 %d，实际 %d", want, got)
		}
	}
	if n := len(readEvents(t, p)); n != 3 {
		t.Errorf("应写进 3 条，实际 %d 条", n)
	}
}
