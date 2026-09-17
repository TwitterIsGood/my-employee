package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TwitterIsGood/my-employee/internal/events"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
	"github.com/TwitterIsGood/my-employee/internal/projection"
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
	// stage 是名字：前台那行「阶段」显示的就是名字，卡点写编号会让同一段在两处
	// 显示成两个样子，而消解是按这个值认人的。
	if evs[0]["stage"] != "需求澄清" {
		t.Errorf("该打回「需求澄清」，实际 %v", evs[0]["stage"])
	}
}

// 出口过了 ≠ 这一趟就该往下走：05 判「驳回」是在**裁决**上游，不是自己没交够。
//
// 这条边曾经是缺的，代价是实测出来的：05 合法交付（结论=驳回），06 的入口要
// 结论=通过，于是 06 把它退回来、05 不改结论、两边对着卡死到返工上限。
// 两边都守着自己的契约，链路一个字节没往前挪——缺的是契约，不是哪一段失职。
func TestVerdictBouncesUpstreamInsteadOfAdvancing(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	// 05 的入口：04 得交出一份带退出码和未覆盖清单的测试报告。
	write(t, dir, "测试报告", map[string]any{
		"退出码": 0, "用例数": 6,
		"未覆盖场景": []string{"超长 user_id"},
	})

	calls, w := writer(dir, "审查记录", map[string]any{
		"结论":   "驳回",
		"驳回至":  "03",
		"驳回理由": []string{"一个 ~1MB 的 user_id 能让看板永久 500"},
		"证伪尝试": []map[string]any{
			{"怎么试的": "POST /login 带一个超长 user_id", "结果": "缓存写坏，之后每次请求都 500"},
		},
		"反例": "POST /login 带超长 user_id 返回 200；随后 GET /api/daily 一直 500",
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "05")

	var ur *UpstreamRejection
	if !errors.As(err, &ur) {
		t.Fatalf("05 判了驳回，就该把上游打回，而不是过关交给 06：%v", err)
	}
	if len(ur.Targets) != 1 || ur.Targets[0] != "03" {
		t.Errorf("退回目标该由交付物里的「驳回至」点名（03），实际 %v", ur.Targets)
	}
	if *calls != 1 {
		t.Errorf("裁决一判出来就该往回走，不该再唤醒 05，实际 %d 次", *calls)
	}

	evs := blockers(t, eventsLog)
	if len(evs) != 1 {
		t.Fatalf("应留下一条 blocker，实际 %v", evs)
	}
	// 卡点落在**被退回**的那一段（03 本地开发），不是审查者自己那一段——
	// 前台说"卡在本地开发"才是让人知道该谁去改。
	if evs[0]["stage"] != "本地开发" {
		t.Errorf("卡点该写被退回的「本地开发」，实际 %v", evs[0]["stage"])
	}
	if !strings.Contains(str(evs[0]["on"]), "永久 500") {
		t.Errorf("退回理由要带审查者的原话，不能只剩字段名，实际 %v", evs[0]["on"])
	}
	if !strings.Contains(str(evs[0]["summary"]), "对抗式审查") {
		t.Errorf("一句里该点明是谁审出来的，实际 %v", evs[0]["summary"])
	}
}

// 结论是「通过」时这段裁决不许误伤：05 照常交给下游，一条 blocker 都不该有。
func TestVerdictPassAdvancesNormally(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")
	write(t, dir, "测试报告", map[string]any{
		"退出码": 0, "未覆盖场景": []string{"超长 user_id"},
	})

	calls, w := writer(dir, "审查记录", map[string]any{
		"结论":    "通过",
		"证伪尝试":  []map[string]any{{"怎么试的": "并发登录同一用户", "结果": "计数无不一致"}},
		"未覆盖范围": []string{"超长 user_id"},
		"前台事实":  []any{},
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 2, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "05")
	if err != nil {
		t.Fatalf("结论通过就该过关: %v", err)
	}
	if *calls != 1 {
		t.Errorf("该只唤醒 1 次，实际 %d 次", *calls)
	}
	if evs := blockers(t, eventsLog); len(evs) != 0 {
		t.Errorf("通过了就不该留卡点，实际 %v", evs)
	}
}

// 判了驳回却没写退给谁——不能猜、不能默认退到 reject_to，只能报错。
// 猜错的代价是通知错人去改，而错的那段会拿着一张没问题的卡再来一遍。
func TestVerdictWithoutTargetIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "测试报告", map[string]any{
		"退出码": 0, "未覆盖场景": []string{"超长 user_id"},
	})
	// 出口条件本身是过得去的：结论合法、证伪尝试有、驳回附了反例。
	calls, w := writer(dir, "审查记录", map[string]any{
		"结论":   "驳回",
		"驳回理由": []string{"有问题"},
		"证伪尝试": []map[string]any{{"怎么试的": "试了", "结果": "坏了"}},
		"反例":   "复现路径",
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Budget: 1, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "05")
	if err == nil || !strings.Contains(err.Error(), "驳回至") {
		t.Fatalf("该报「没说退给谁」并点名缺失字段，实际 %v", err)
	}
	if *calls != 1 {
		t.Errorf("这是判定炸了，不是重跑的事，实际唤醒 %d 次", *calls)
	}
}

// 挂住的唤醒要有个头，而且得出声。
//
// 实测过一次：05 的自写探针脚本拉起一个 headless 浏览器，浏览器卡住不返回，
// 整条链路就停在那儿——既没往下走，也没走到"撞南墙该升级"的那一刻。
// 它比撞南墙更坏：撞南墙会升级给人，挂住是无声的、无限的。
func TestAHangingWakeIsBoundedAndVisible(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	hung := WorkerFunc(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})

	start := time.Now()
	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "看板", Worker: hung,
		WakeTimeout: 200 * time.Millisecond,
	}.Run(context.Background(), "01")

	if !errors.Is(err, ErrWakeFailed) {
		t.Fatalf("挂住的唤醒该报唤醒失败，实际 %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("挂了 %s 才回来——上限没起作用", d)
	}

	// 前台那边"叫不醒"和"正在跑"长得一模一样，都是没动静。所以得留一条卡点。
	evs := blockers(t, eventsLog)
	if len(evs) != 1 {
		t.Fatalf("该留下一条卡点，实际 %v", evs)
	}
	if !strings.Contains(str(evs[0]["on"]), "没有返回") {
		t.Errorf("卡点要说清是超时，而不是含糊的一句失败，实际 %v", evs[0]["on"])
	}
}

// 一个不返回的唤醒要在上限上被掐掉，而且掐的时候要连它拉起的后台进程一起收走。
//
// 实测的那一次就是这样：05 的探针脚本在前台等着一个卡住的 headless 浏览器，
// 脚本不返回；而脚本此前起过的服务还开着端口。只杀 sh 的话，那些服务会留下来。
func TestATimedOutWakeIsKilledWithItsChildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// 先起一个后台进程记下自己的 pid，再在前台挂住——挂的是前台那条。
	cmd := "sleep 300 & echo $! > " + pidFile + "; sleep 300"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := (CmdWorker{Cmd: cmd, Dir: dir}).Run(ctx, ""); err == nil {
		t.Fatal("超时了就该报错，不能装作跑完了")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("上限没起作用，%s 才回来", d)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // 收走了
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("超时收走了 sh，却把子进程 %d 留在了机器上", pid)
}

// 白名单是一种承诺：承诺了什么，就得有谁去发。
//
// 这条是实测出来的。一趟七段链路跑完，投影里有「已发生的变更」「观测到的数字」
// 两个标题，而 03 改的 9 个文件、05 量出的「门槛比原先估的低 6 倍」一条都进不来——
// harness 里没有任何代码路径发 change / observation / failure。
// 承诺和兑现之间没有第二个人对账，所以它们能一边承诺、一边什么都不发，
// 而且编译得过、跑得通、测试全绿，只是没人发现。
func TestEveryWhitelistedTypeIsReachable(t *testing.T) {
	// 这两类由 harness 自己发，不由交付物发：卡点来自判定与驳回，待决策来自 pause_on。
	byHarness := map[string]bool{"blocker": true, "decision_needed": true}

	for typ := range projection.Types() {
		if byHarness[typ] {
			continue
		}
		if _, ok := publishKeys[typ]; !ok {
			t.Errorf("白名单里的 %q 谁都不发：交付物发不出它（publishKeys 里没有），"+
				"harness 也不发它。承诺了却没人发，前台就在假装有这一栏", typ)
		}
	}
	// 反方向也要对：能发的类型必须在白名单里，否则是发了一条投影认不出来的东西，
	// 那时投影会直接报错——而错的是发射端，不是投影。
	for typ := range publishKeys {
		if _, ok := projection.Types()[typ]; !ok {
			t.Errorf("publishKeys 会发 %q，而投影白名单里没有它", typ)
		}
	}
}

// 报给前台这件事，端到端要看得见：交付物里声明的事实，最后要落到投影上。
//
// 只测"harness 写了一条 type=change 的事件"是不够的——那正是缺陷 C 的形状：
// 事件类型凑齐了，前台还是一个字没有。所以这条一路比到**前台的正文**。
func TestPublishedFactsReachTheFront(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	_, w := writer(dir, "变更卡", map[string]any{
		"diff":  "9 files changed",
		"scope": "internal/api、internal/logstore 等 9 个文件",
		"失败记录":  []any{},
		"前台事实": []any{
			map[string]any{
				"类型": "change", "一句话": "改了 9 个文件",
				"影响面": "internal/api/api.go、internal/logstore/store.go 等 9 个文件",
			},
			map[string]any{
				"类型": "observation", "一句话": "读路径比改前慢一个量级",
				"指标": "/api/daily 单发延迟", "窗口": "100 万行数据", "数值": "1.35s",
			},
			map[string]any{
				"类型": "decision_needed", "一句话": "写入端限长的阈值定多少？",
				"选项": []any{
					map[string]any{"选项": "按现在的 256 字节", "代价": "极长 user_id 被拒"},
					map[string]any{"选项": "放宽到 1 MiB", "代价": "单行可能撑爆响应"},
				},
			},
		},
	})
	write(t, dir, "需求定义卡", map[string]any{"验收标准": "看板 1 秒内出数"})
	write(t, dir, "方案卡", map[string]any{
		"做法": "换读路径", "影响面": "internal", "未标注推断": []any{},
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 3, Item: "活跃度看板", Worker: w,
	}.Run(context.Background(), "03")
	if err != nil {
		t.Fatalf("这一段该过关: %v", err)
	}

	evs, err := projection.Parse(mustRead(t, eventsLog))
	if err != nil {
		t.Fatalf("事件日志读不了: %v", err)
	}
	built, err := projection.Build(evs)
	if err != nil {
		t.Fatalf("harness 发出去的东西投影该认: %v", err)
	}
	front := built.Text

	for _, want := range []string{
		"改了 9 个文件",
		"读路径比改前慢一个量级",
		"1.35s",        // observation 的三个键都得在
		"写入端限长的阈值定多少？", // 待决策要真的到前台
		"按现在的 256 字节",
	} {
		if !strings.Contains(front, want) {
			t.Errorf("前台该看到 %q，实际全文:\n%s", want, front)
		}
	}
	// 「上一个已确认的完成点」读的是最后一条 change。没有 change 它就永远是「无」——
	// 七个阶段真跑下来，那行主标题写着「无」，比不报还难看。
	if strings.Contains(front, "上一个已确认的完成点：无") {
		t.Errorf("有 change 就该填上完成点，实际全文:\n%s", front)
	}
}

// 发出去的事件，形状必须和投影器读它的方式一致。
//
// 这条曾经差过一次：publishOptions 返回 []string，而投影器数选项条数用的是
// `ev["options"].([]any)`——[]string 断不过去，一条选项齐备的待决策在那儿被
// 判成"0 个选项"，整次投影直接报错。
//
// 这个错**在落盘那条路上看不见**：JSON 一来一回会把 []string 重新解成 []any，
// 所以端到端测试照样绿。只有不经磁盘、直接拿内存里那张 map 去投影的人会中招。
// 所以要单独钉一条：按投影器的读法读，而不是按我们自己的写法过一遍。
func TestPublishedFactsHaveTheShapeProjectionReads(t *testing.T) {
	st := pipeline.Stage{ID: "03", Name: "本地开发", Publish: "前台事实"}
	a := pipeline.Artifact{"前台事实": []any{
		map[string]any{
			"类型": "decision_needed", "一句话": "阈值定多少？",
			"选项": []any{
				map[string]any{"选项": "256 字节", "代价": "极长 user_id 被拒"},
				map[string]any{"选项": "1 MiB", "代价": "单行可能撑爆响应"},
			},
		},
	}}

	facts, vs := publishFacts(st, a)
	if len(vs) > 0 {
		t.Fatalf("该发得出去: %s", vs[0].Why)
	}
	opts, ok := facts[0]["options"].([]any)
	if !ok {
		t.Fatalf("options 是 %T，投影器只认 []any——落盘再读回来才发现不了这个错",
			facts[0]["options"])
	}
	if len(opts) != 2 {
		t.Errorf("选项数不对: %d", len(opts))
	}
	// 最后按投影器的方式整个走一遍，内存里这一份能不能过它的校验。
	if _, err := projection.Build([]map[string]any{facts[0]}); err != nil {
		t.Errorf("内存里发出的事件投影器认不了: %v", err)
	}
}

// 声明了 publish 却不交那个字段，是**没交够**，不是"没什么可报的"。
//
// 缺省不等于没有——它等于判不出来，和投影那条 `confirmed` 是同一个道理。
// 真没有可报的就写一个空数组，那才是一个明确的表态。
func TestMissingPublishedFieldIsNotAccepted(t *testing.T) {
	dir := t.TempDir()
	eventsLog := filepath.Join(dir, "events.jsonl")

	card := map[string]any{
		"diff": "9 files changed", "scope": "internal",
		"失败记录": []any{},
	}
	calls, w := writer(dir, "变更卡", card, card)
	write(t, dir, "需求定义卡", map[string]any{"验收标准": "看板 1 秒内出数"})
	write(t, dir, "方案卡", map[string]any{
		"做法": "换读路径", "影响面": "internal", "未标注推断": []any{},
	})

	err := Runner{
		Stages: loadStages(t), SpecsDir: specsDir(), Dir: dir,
		Events: eventsLog, Budget: 2, Item: "看板", Worker: w,
	}.Run(context.Background(), "03")

	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("缺字段不该被当成已接受，实际 %v", err)
	}
	if *calls != 2 {
		t.Errorf("该带着原因重跑到预算用尽，实际唤醒 %d 次", *calls)
	}
	// 中途那两次没过是**过程**，只有预算用尽时那一条才该发到前台。
	evs := blockers(t, eventsLog)
	if len(evs) != 1 {
		t.Fatalf("只该有一条升级卡点，实际 %v", evs)
	}
	if !strings.Contains(str(evs[0]["summary"]), "连续 2 次未达出口条件") {
		t.Errorf("那条该是升级卡点，实际 %v", evs[0]["summary"])
	}
	if !strings.Contains(str(evs[0]["on"]), "前台事实") {
		t.Errorf("升级时要带上卡在哪一条，实际 %v", evs[0]["on"])
	}
}

// 发出去的东西必须当场校验：类型不对、该带的字段没带、想自己发卡点，
// 都在这里被挡下。挡不住的话，前台会收到投影认不出来的记录——
// 那时投影直接报错退出，锅看起来是投影的，其实是发射端发了不该发的。
func TestBadPublishedFactsAreRejectedLoudly(t *testing.T) {
	cases := []struct {
		why  string
		fact map[string]any
	}{
		{"类型不在白名单里", map[string]any{
			"类型": "progress", "一句话": "进展顺利"}},
		{"blocker 不许由交付物发", map[string]any{
			"类型": "blocker", "一句话": "卡住了", "卡在": "编译"}},
		{"缺一句话", map[string]any{
			"类型": "change", "影响面": "internal"}},
		{"change 缺影响面", map[string]any{
			"类型": "change", "一句话": "改了 9 个文件"}},
		{"observation 缺数值", map[string]any{
			"类型": "observation", "一句话": "慢了", "指标": "p99", "窗口": "10 万行"}},
		{"decision_needed 只有一个选项", map[string]any{
			"类型": "decision_needed", "一句话": "选哪个", "选项": []any{
				map[string]any{"选项": "甲", "代价": "贵"}}}},
		{"选项没写代价", map[string]any{
			"类型": "decision_needed", "一句话": "选哪个", "选项": []any{
				map[string]any{"选项": "甲"}, map[string]any{"选项": "乙"}}}},
	}
	for _, c := range cases {
		st := pipeline.Stage{ID: "03", Name: "本地开发", Produces: "变更卡", Publish: "前台事实"}
		a := pipeline.Artifact{"前台事实": []any{c.fact}}
		facts, vs := publishFacts(st, a)
		if len(vs) != 1 {
			t.Errorf("%s：该被挡下，实际过了 %v", c.why, facts)
			continue
		}
		if facts != nil {
			t.Errorf("%s：挡下就不该同时发出去", c.why)
		}
	}
}

// 没声明 publish 的阶段一条都不发。这不是漏——没打算报的阶段本来就没打算报。
func TestStageWithoutPublishPublishesNothing(t *testing.T) {
	st := pipeline.Stage{ID: "01", Name: "需求澄清", Produces: "需求定义卡"}
	facts, vs := publishFacts(st, pipeline.Artifact{"前台事实": []any{
		map[string]any{"类型": "change", "一句话": "改了什么"},
	}})
	if len(facts) != 0 || len(vs) != 0 {
		t.Errorf("没声明 publish 就不该发，实际 facts=%v vs=%v", facts, vs)
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
	if len(evs) != 2 {
		t.Fatalf("该留两条事件（进入这一段 + 这一段被接受），实际 %d 条: %v", len(evs), evs)
	}
	for _, e := range evs {
		if e["type"] != "progress" {
			t.Errorf("两条都该用黑名单类型，实际 %v", e["type"])
		}
		if e["stage"] != "需求澄清" {
			t.Errorf("都该带阶段名，实际 %v", e["stage"])
		}
	}
	// 只有后一条是"被接受"——卡点靠它消解，进入那一条不算。
	if evs[0]["outcome"] != nil {
		t.Errorf("「进入阶段」不该带 outcome，实际 %v", evs[0]["outcome"])
	}
	if evs[1]["outcome"] != "accepted" {
		t.Errorf("「交付通过出口条件」该标 accepted，实际 %v", evs[1]["outcome"])
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
	p := buildPrompt(st, "规范正文", nil, "/tmp/卡.json", "做看板", "", nil, nil)
	if !strings.Contains(p, "不许替他答") || !strings.Contains(p, "待决策") {
		t.Errorf("没有答复时该要求停下来问，实际:\n%s", p)
	}
}

// prompt 里必须把"判定在你自己之外"说清楚，也要把出口字段点名。
func TestPromptStatesTheBarrier(t *testing.T) {
	st := loadStages(t)[1] // 02
	p := buildPrompt(st, "规范正文", map[string]pipeline.Artifact{
		"需求定义卡": {"范围外": []any{"不做留存"}, "确认状态": "confirmed"},
	}, "/tmp/方案卡.json", "活跃度看板", "", nil, nil)

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

// blockers 只挑卡点。日志里还混着 progress 那种"走到哪一段了"的脚印，
// 它们前台看不到，混在一起数就把门槛验糊了。
func blockers(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range readEvents(t, path) {
		if e["type"] == "blocker" {
			out = append(out, e)
		}
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
