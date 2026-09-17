package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试守的不是"代码能跑"，是**驳回权真的能被机器判出来**。
// 一条只在文档里存在的入口条件，第一个赶时间的人就会绕过它。

func loadReal(t *testing.T) []Stage {
	t.Helper()
	stages, err := Load(filepath.Join("..", "..", "standards", "stages"))
	if err != nil {
		t.Fatalf("加载阶段契约失败: %v", err)
	}
	return stages
}

func art(t *testing.T, j string) Artifact {
	t.Helper()
	var a Artifact
	if err := json.Unmarshal([]byte(j), &a); err != nil {
		t.Fatalf("测试用例不是合法 JSON: %v", err)
	}
	return a
}

// 阶段树本身就是流水线定义。它读不出来，整条流水线就没有定义。
func TestRealTreeLoads(t *testing.T) {
	stages := loadReal(t)
	if len(stages) != 7 {
		t.Fatalf("阶段应为 7 个，实际 %d", len(stages))
	}
	want := []string{"01", "02", "03", "04", "05", "06", "07"}
	for i, s := range stages {
		if s.ID != want[i] {
			t.Errorf("第 %d 个阶段 id=%q，应为 %q", i+1, s.ID, want[i])
		}
		if s.Produces == "" {
			t.Errorf("阶段 %s 没有声明交付物", s.ID)
		}
		if len(s.Exit) == 0 {
			t.Errorf("阶段 %s 的出口条件为空——没有谓词的阶段视为未落地", s.ID)
		}
		if i > 0 && len(s.Entry) == 0 {
			t.Errorf("阶段 %s 的入口条件为空——没有入口条件就没有驳回权", s.ID)
		}
	}
	if stages[0].RejectTo != "" {
		t.Errorf("链头不该有 reject_to，实际 %q", stages[0].RejectTo)
	}
}

// 每个入口条件都要指向上游交付物；漏了 From 就变成"对空气判定"。
func TestEntryConditionsNameUpstream(t *testing.T) {
	for _, s := range loadReal(t) {
		for _, c := range s.Entry {
			if c.From == "" {
				t.Errorf("阶段 %s 的入口条件 %s 没写 from", s.ID, c.Field)
			}
		}
	}
}

// 01 交付的边界没想清楚 → 02 打回 01。
func TestEntryRejectsVagueRequirement(t *testing.T) {
	s02, ok := ByID(loadReal(t), "02")
	if !ok {
		t.Fatal("找不到阶段 02")
	}

	cases := []struct {
		name string
		card string
	}{
		{"范围外为空", `{"目标":"做活跃度看板","范围内":["日活"],"范围外":[],"验收标准":"p99 < 500ms","确认状态":"confirmed"}`},
		{"确认状态是 assumed", `{"目标":"做活跃度看板","范围内":["日活"],"范围外":["不做留存"],"验收标准":"p99 < 500ms","确认状态":"assumed"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := s02.CheckEntry(map[string]Artifact{"需求定义卡": art(t, tc.card)})
			if len(vs) == 0 {
				t.Fatal("这份需求定义卡应该在 02 的入口被拦下")
			}
			rejs := Reject(loadReal(t), s02, "活跃度看板", vs)
			if len(rejs) != 1 {
				t.Fatalf("卡片上两个字段都来自 01，应合成 1 条驳回，实际 %d 条", len(rejs))
			}
			if rejs[0].From != "01" {
				t.Errorf("驳回应打回 01，实际 %q", rejs[0].From)
			}
			if rejs[0].By != "02" {
				t.Errorf("驳回方应为 02，实际 %q", rejs[0].By)
			}
			if len(rejs[0].Reasons) != len(vs) {
				t.Errorf("驳回理由应逐条附上，%d != %d", len(rejs[0].Reasons), len(vs))
			}
		})
	}

	// 上游交付物整个缺失，也要能判、能驳回。
	vs := s02.CheckEntry(nil)
	if len(vs) != len(s02.Entry) {
		t.Errorf("上游交付物缺失时应逐条报缺，实际 %d 条（入口条件 %d 条）", len(vs), len(s02.Entry))
	}
}

// 真实事故：一条没标注的假设，烧掉了整个 03 阶段。
func TestExitCatchesUnlabeledHypothesis(t *testing.T) {
	s02, _ := ByID(loadReal(t), "02")
	card := art(t, `{
		"做法":"加一个按 user_id 聚合的查询",
		"影响面":["users 表读取路径"],
		"风险":[{"desc":"索引缺失导致顺序扫描"}],
		"验证方式":"p99 < 500ms",
		"未标注推断":["活跃度可以直接从 audit_log 推出来"]
	}`)
	vs := s02.CheckExit(card)
	if len(vs) != 1 {
		t.Fatalf("应恰好拦下 1 条，实际 %d 条: %v", len(vs), vs)
	}
	if vs[0].Cond.Field != "未标注推断" {
		t.Errorf("应拦下 未标注推断，实际 %s", vs[0].Cond.Field)
	}

	// 标清楚了就放行。
	ok := art(t, `{
		"做法":"加一个按 user_id 聚合的查询",
		"影响面":["users 表读取路径"],
		"风险":[{"desc":"索引缺失导致顺序扫描"}],
		"验证方式":"p99 < 500ms",
		"未标注推断":[]
	}`)
	if vs := s02.CheckExit(ok); len(vs) != 0 {
		t.Errorf("标注齐全的方案卡不该被拦，实际被拦: %v", vs)
	}
}

// min_items：至少一条风险。零风险的方案不是没风险，是没想过。
func TestExitNeedsAtLeastOneRisk(t *testing.T) {
	s02, _ := ByID(loadReal(t), "02")
	card := art(t, `{"做法":"改查询","影响面":["users"],"风险":[],"验证方式":"p99 < 500ms","未标注推断":[]}`)
	vs := s02.CheckExit(card)
	if len(vs) != 1 || vs[0].Cond.Field != "风险" {
		t.Fatalf("零风险应在出口被拦下，实际: %v", vs)
	}
}

// 条件谓词：只有触及线上数据路径时，才要求测试环境的预演日志。
func TestConditionalGateOnDataPath(t *testing.T) {
	s04, _ := ByID(loadReal(t), "04")

	// 触及数据路径、却没给测试环境日志 → 拦下。
	dirty := art(t, `{"测试命令":"go test ./...","退出码":0,"未覆盖场景":[],"触及数据路径":true}`)
	if vs := s04.CheckExit(dirty); len(vs) != 1 || vs[0].Cond.Field != "测试环境日志" {
		t.Fatalf("触及数据路径却没做测试环境预演，应被拦下，实际: %v", vs)
	}

	// 给了日志 → 放行。
	fixed := art(t, `{"测试命令":"go test ./...","退出码":0,"未覆盖场景":[],"触及数据路径":true,"测试环境日志":"runs/rollback-dryrun.log"}`)
	if vs := s04.CheckExit(fixed); len(vs) != 0 {
		t.Fatalf("预演日志齐全不该被拦，实际: %v", vs)
	}

	// 没触及数据路径 → 这条不适用。
	clean := art(t, `{"测试命令":"go test ./...","退出码":0,"未覆盖场景":[],"触及数据路径":false}`)
	if vs := s04.CheckExit(clean); len(vs) != 0 {
		t.Fatalf("不涉及数据路径时不该要求预演日志，实际: %v", vs)
	}
}

// **前置说不清 = 未落地。** 前置字段缺失按不通过处理，不许静默跳过闸门。
func TestMissingPreconditionIsRejectedNotSkipped(t *testing.T) {
	s06, _ := ByID(loadReal(t), "06")
	rec := art(t, `{"观测记录":[{},{}],"回滚预演日志":"runs/rollback-dryrun.log"}`)
	vs := s06.CheckExit(rec)
	if len(vs) != 1 {
		t.Fatalf("前置字段缺失应报一条，实际 %d 条: %v", len(vs), vs)
	}
	if !strings.Contains(vs[0].Why, "前置条件") {
		t.Errorf("原因应指明是前置条件缺失，实际 %q", vs[0].Why)
	}
}

// 观测到异常却没置位故障上报 → 准则 7 被跳过，驳回。
func TestFaultReportRequiredOnlyWhenObserved(t *testing.T) {
	s06, _ := ByID(loadReal(t), "06")

	bad := art(t, `{"观测记录":[{},{}],"回滚预演日志":"x","观测到异常":true,"故障上报":false}`)
	if vs := s06.CheckExit(bad); len(vs) != 1 || vs[0].Cond.Field != "故障上报" {
		t.Fatalf("观测到异常却没上报，应被拦下，实际: %v", vs)
	}

	good := art(t, `{"观测记录":[{},{}],"回滚预演日志":"x","观测到异常":true,"故障上报":true}`)
	if vs := s06.CheckExit(good); len(vs) != 0 {
		t.Fatalf("已上报不该被拦，实际: %v", vs)
	}

	// 没观测到异常时不再要求它——但前置字段本身必须在（上一条测试守这个）。
	quiet := art(t, `{"观测记录":[{},{}],"回滚预演日志":"x","观测到异常":false}`)
	if vs := s06.CheckExit(quiet); len(vs) != 0 {
		t.Fatalf("无异常时不该要求故障上报，实际: %v", vs)
	}
}

// 准则 4：至少两级影响面递进。一步到全量，结构上就过不了 06 的入口。
func TestChangeStepsNeedTwoLevels(t *testing.T) {
	s06, _ := ByID(loadReal(t), "06")
	arts := map[string]Artifact{
		"审查记录": art(t, `{"结论":"通过","未覆盖范围":["缓存击穿"]}`),
	}
	base := `{
		"测试报告":"runs/report.md",
		"变更时间":"2026-09-16T10:00:00+08:00",
		"变更风险":"索引变更锁表",
		"观测指标":"p99",
		"回滚方案":"kubectl rollout undo deploy/api",
		"变更步骤":%s
	}`

	oneStep := art(t, strings.Replace(base, "%s", `["全量发布"]`, 1))
	vs := s06.CheckEntry(withArtifact(arts, "变更卡", oneStep))
	if len(vs) != 1 || vs[0].Cond.Field != "变更步骤" {
		t.Fatalf("一步到全量应在入口被拦下，实际: %v", vs)
	}

	twoSteps := art(t, strings.Replace(base, "%s", `["单机灰度","全部"]`, 1))
	if vs := s06.CheckEntry(withArtifact(arts, "变更卡", twoSteps)); len(vs) != 0 {
		t.Fatalf("两级灰度不该被拦，实际: %v", vs)
	}
}

// 一次入口判定同时退回两段时，要分组——合成一条"退回 05"会让 03 不知道要补东西。
func TestRejectionSplitsByTarget(t *testing.T) {
	s06, _ := ByID(loadReal(t), "06")
	arts := map[string]Artifact{
		// 审查记录本身不完整：结论不是通过。
		"审查记录": art(t, `{"结论":"基本可以","未覆盖范围":["缓存击穿"]}`),
		// 变更卡缺字段，且只有一级灰度。
		"变更卡": art(t, `{"测试报告":"r","变更时间":"t","变更步骤":["全量发布"],"变更风险":"r","观测指标":"p99"}`),
	}

	vs := s06.CheckEntry(arts)
	if len(vs) == 0 {
		t.Fatal("这份上游交付应该被拦下")
	}
	rejs := Reject(loadReal(t), s06, "活跃度看板", vs)
	byTarget := map[string]Rejection{}
	for _, r := range rejs {
		byTarget[r.From] = r
	}
	if _, ok := byTarget["03"]; !ok {
		t.Errorf("变更卡缺字段应打回 03，实际打回: %v", targetsOf(rejs))
	}
	if _, ok := byTarget["05"]; !ok {
		t.Errorf("审查记录的问题应打回 05，实际打回: %v", targetsOf(rejs))
	}
	if len(byTarget["03"].Reasons) != 2 {
		t.Errorf("03 该收到 2 条（变更步骤 + 回滚方案），实际 %d 条: %v",
			len(byTarget["03"].Reasons), byTarget["03"].Reasons)
	}
}

func targetsOf(rejs []Rejection) []string {
	out := make([]string, 0, len(rejs))
	for _, r := range rejs {
		out = append(out, r.From)
	}
	return out
}

// 出口不通过是退回自己，不是打回上游。
func TestExitFailureIsSelfRejection(t *testing.T) {
	s02, _ := ByID(loadReal(t), "02")
	card := art(t, `{"做法":"改查询","影响面":["users"],"风险":[],"验证方式":"x","未标注推断":[]}`)
	vs := s02.CheckExit(card)
	if len(vs) == 0 {
		t.Fatal("零风险应在出口被拦下")
	}
	rej := SelfReject(s02, "活跃度看板", vs)
	if rej.From != "02" || rej.By != "02" {
		t.Errorf("出口不通过该退回自己，实际 from=%q by=%q", rej.From, rej.By)
	}
	if rej.From == s02.RejectTo {
		t.Error("出口不通过不该打回上游——上游没错")
	}
	// 判据要卡在"没写成被打回"上，不能卡在某个具体编号上——
	// 编号已经不进 summary 了（那里只放名字），照编号断言会变成一条永远为真的空断言。
	if s := rej.Event()["summary"].(string); strings.Contains(s, "打回") {
		t.Errorf("自己退自己不该写成「被打回」——上游没错，是这一段没干完，实际 %q", s)
	}
}

// 驳回要能变成前台的可见事实：一条 blocker 事件。
func TestRejectionEventIsProjectable(t *testing.T) {
	s02, _ := ByID(loadReal(t), "02")
	card := art(t, `{"目标":"做看板","范围内":["日活"],"范围外":[],"验收标准":"p99 < 500ms","确认状态":"confirmed"}`)
	rejs := Reject(loadReal(t), s02, "活跃度看板", s02.CheckEntry(map[string]Artifact{"需求定义卡": card}))
	if len(rejs) != 1 {
		t.Fatalf("应恰好 1 条驳回，实际 %d", len(rejs))
	}
	ev := rejs[0].Event()
	for _, k := range []string{"type", "summary", "on", "confirmed", "evidence", "stage"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("blocker 事件缺字段 %q", k)
		}
	}
	if ev["type"] != "blocker" {
		t.Errorf("事件类型应为 blocker，实际 %v", ev["type"])
	}
	if ev["confirmed"] != true {
		t.Error("驳回是已确认的事实，confirmed 必须为 true")
	}
	// stage 写的是名字不是编号：卡点要跟别处的「阶段：<名字>」对得上，
	// 不然同一条卡点在两处显示成两个样子，读的人还得自己去对一张表。
	if ev["stage"] != "需求澄清" {
		t.Errorf("打回 01 后流水线回到「需求澄清」，stage 应为名字，实际 %v", ev["stage"])
	}
	if !strings.Contains(fmt.Sprint(ev["summary"]), "需求澄清") {
		t.Errorf("summary 也该说人话，实际 %v", ev["summary"])
	}
}

func withArtifact(arts map[string]Artifact, name string, a Artifact) map[string]Artifact {
	out := make(map[string]Artifact, len(arts)+1)
	for k, v := range arts {
		out[k] = v
	}
	out[name] = a
	return out
}

// 06 的入口只收 `通过`。"基本可以上线"这种结论在 05 的出口就该死。
func TestReviewConclusionIsBinary(t *testing.T) {
	s05, _ := ByID(loadReal(t), "05")

	hedged := art(t, `{"结论":"基本可以上线，小问题后续优化","证伪尝试":["试了空数组输入"],"未覆盖范围":["缓存击穿"]}`)
	if vs := s05.CheckExit(hedged); len(vs) != 1 || vs[0].Cond.Field != "结论" {
		t.Fatalf("含糊结论应被拦下，实际: %v", vs)
	}

	noFalsify := art(t, `{"结论":"通过","证伪尝试":[],"未覆盖范围":["缓存击穿"]}`)
	if vs := s05.CheckExit(noFalsify); len(vs) != 1 || vs[0].Cond.Field != "证伪尝试" {
		t.Fatalf("没有证伪尝试就不算审查，应被拦下，实际: %v", vs)
	}

	passNoScope := art(t, `{"结论":"通过","证伪尝试":["试了空数组"],"未覆盖范围":[]}`)
	if vs := s05.CheckExit(passNoScope); len(vs) != 1 || vs[0].Cond.Field != "未覆盖范围" {
		t.Fatalf("通过却没列未覆盖范围，应被拦下，实际: %v", vs)
	}

	rejectNoCase := art(t, `{"结论":"驳回","证伪尝试":["试了空数组"]}`)
	if vs := s05.CheckExit(rejectNoCase); len(vs) != 1 || vs[0].Cond.Field != "反例" {
		t.Fatalf("驳回却无可复现反例，应被拦下，实际: %v", vs)
	}

	good := art(t, `{"结论":"通过","证伪尝试":["试了空数组"],"未覆盖范围":["缓存击穿"]}`)
	if vs := s05.CheckExit(good); len(vs) != 0 {
		t.Fatalf("完整的审查记录不该被拦，实际: %v", vs)
	}

	// 05 的出口结论直接决定 06 能不能开工。
	s06, _ := ByID(loadReal(t), "06")
	if vs := s06.CheckEntry(map[string]Artifact{"审查记录": hedged}); len(vs) == 0 {
		t.Fatal("含糊结论不该让 06 开工")
	}
}

// `present` 与 `nonempty` 的区别：字段必须在，但空数组是合法回答。
func TestPresentAllowsEmptyButRequiresField(t *testing.T) {
	s04, _ := ByID(loadReal(t), "04")

	explicit := art(t, `{"测试命令":"go test ./...","退出码":0,"未覆盖场景":[],"触及数据路径":false}`)
	if vs := s04.CheckExit(explicit); len(vs) != 0 {
		t.Errorf("显式写出的空数组应放行，实际: %v", vs)
	}

	silent := art(t, `{"测试命令":"go test ./...","退出码":0,"触及数据路径":false}`)
	vs := s04.CheckExit(silent)
	if len(vs) != 1 || vs[0].Cond.Field != "未覆盖场景" {
		t.Fatalf("不写未覆盖场景（等于把风险藏起来）应被拦下，实际: %v", vs)
	}
	if !strings.Contains(vs[0].Why, "显式") {
		t.Errorf("原因应说明要求显式给出，实际 %q", vs[0].Why)
	}
}

// 退出码不为 0 就流转不下去。有退出码才算跑过。
func TestExitCodeMustBeZero(t *testing.T) {
	s04, _ := ByID(loadReal(t), "04")
	red := art(t, `{"测试命令":"go test ./...","退出码":1,"未覆盖场景":[],"触及数据路径":false}`)
	vs := s04.CheckExit(red)
	if len(vs) != 1 || vs[0].Cond.Field != "退出码" {
		t.Fatalf("非零退出码应被拦下，实际: %v", vs)
	}
}

// —— 加载器的自检：坏掉的契约树必须报错，不能静默降级 ——

func writeStage(t *testing.T, dir, name, contract string) {
	t.Helper()
	doc := "# 阶段\n\n" + contractHeading + "\n\n```json\n" + contract + "\n```\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsChainBreak(t *testing.T) {
	dir := t.TempDir()
	writeStage(t, dir, "01-a.md", `{"id":"01","name":"A","produces":"甲","exit":[{"field":"x","check":"nonempty"}]}`)
	writeStage(t, dir, "02-b.md", `{"id":"02","name":"B","produces":"乙","entry":[{"from":"甲","field":"x","check":"nonempty"}],"exit":[{"field":"y","check":"nonempty"}],"reject_to":"03"}`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "链条断了") {
		t.Fatalf("reject_to 指错阶段应报链条断，实际: %v", err)
	}
}

func TestLoadRejectsMissingContract(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "01-a.md"), []byte("# 只有散文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), contractHeading) {
		t.Fatalf("没有契约块的阶段文件应报错，实际: %v", err)
	}
}

func TestLoadRejectsNonContiguousIDs(t *testing.T) {
	dir := t.TempDir()
	writeStage(t, dir, "01-a.md", `{"id":"01","name":"A","produces":"甲","exit":[{"field":"x","check":"nonempty"}]}`)
	writeStage(t, dir, "02-b.md", `{"id":"03","name":"B","produces":"乙","entry":[{"from":"甲","field":"x","check":"nonempty"}],"exit":[{"field":"y","check":"nonempty"}],"reject_to":"01"}`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "编号不连续") {
		t.Fatalf("编号跳号应报错，实际: %v", err)
	}
}

// 写不出谓词的判定词不许上线。
func TestUnknownCheckFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeStage(t, dir, "01-a.md", `{"id":"01","name":"A","produces":"甲","exit":[{"field":"x","check":"看着还行"}]}`)
	stages, err := Load(dir)
	if err != nil {
		t.Fatalf("加载不该失败（错误应在判定时报）: %v", err)
	}
	vs := stages[0].CheckExit(art(t, `{"x":"有值"}`))
	if len(vs) != 1 || !strings.Contains(vs[0].Why, "未知的判定") {
		t.Fatalf("未知判定词应报错，实际: %v", vs)
	}
}
