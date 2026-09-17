package chain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TwitterIsGood/my-employee/internal/dispatch"
	"github.com/TwitterIsGood/my-employee/internal/pipeline"
	"github.com/TwitterIsGood/my-employee/internal/projection"
)

// 这一层守的是**断点**：一条需求走到哪了、被打回要退回哪一段、返工几次算撞南墙。
// 单段的判定归 dispatch 管，这里只管链路的账。

// fake 是一个可控的唤醒集合：每一段有自己的一叠"每次被叫醒写什么"。
type fake struct {
	dir     string
	calls   map[string]int
	prompts map[string][]string
}

func newFake(dir string) *fake {
	return &fake{dir: dir, calls: map[string]int{}, prompts: map[string][]string{}}
}

// worker 给某一段配一叠交付物：第 n 次被叫醒写第 n 份。给 nil 表示这次故意什么都不写。
func (f *fake) worker(produces string, cards ...map[string]any) dispatch.Worker {
	return dispatch.WorkerFunc(func(_ context.Context, prompt string) error {
		i := f.calls[produces]
		f.calls[produces]++
		f.prompts[produces] = append(f.prompts[produces], prompt)
		if i >= len(cards) || cards[i] == nil {
			return nil
		}
		b, err := json.Marshal(cards[i])
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(f.dir, produces+".json"), b, 0o644)
	})
}

// newStage 造一段合成的阶段。契约与 spec 文件都写在这里——
// dispatch 要去 spec 目录里读那一段的散文，缺文件会被判成"没写 spec"。
func newStage(t *testing.T, specsDir, contract string) pipeline.Stage {
	t.Helper()
	var st pipeline.Stage
	if err := json.Unmarshal([]byte(contract), &st); err != nil {
		t.Fatalf("合成契约不是合法 JSON: %v", err)
	}
	doc := "## 契约（机器可读）\n\n```json\n" + contract + "\n```\n"
	if err := os.WriteFile(filepath.Join(specsDir, st.ID+"-fake.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return st
}

// 合成一段三段链。01 会产出"卡一"，02 要 卡一.A，03 要 卡二.B。
// 用合成链是为了精确控制"哪一段被打回"——真实七段树的输入太多，摆不出干净的反例。
func threeStages(t *testing.T, dir string) ([]pipeline.Stage, string) {
	t.Helper()
	specsDir := filepath.Join(dir, "standards")
	if err := os.MkdirAll(specsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return []pipeline.Stage{
		newStage(t, specsDir, `{"id":"01","name":"澄清","produces":"卡一","entry":[],
			"exit":[{"field":"A","check":"nonempty"}],"pause_on":"待决策"}`),
		newStage(t, specsDir, `{"id":"02","name":"方案","produces":"卡二",
			"entry":[{"from":"卡一","field":"A","check":"nonempty"}],
			"exit":[{"field":"B","check":"nonempty"}],"reject_to":"01"}`),
		newStage(t, specsDir, `{"id":"03","name":"开发","produces":"卡三",
			"entry":[{"from":"卡二","field":"B","check":"nonempty"}],
			"exit":[{"field":"C","check":"nonempty"}],"reject_to":"02"}`),
	}, specsDir
}

// verdictStages 合成「开发 → 审查 → 部署」三段，把 验证B 撞上的那次死锁摆成反例。
func verdictStages(t *testing.T, dir string) ([]pipeline.Stage, string) {
	t.Helper()
	specsDir := filepath.Join(dir, "standards")
	if err := os.MkdirAll(specsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return []pipeline.Stage{
		newStage(t, specsDir, `{"id":"01","name":"开发","produces":"卡一","entry":[],
			"exit":[{"field":"A","check":"nonempty"}],"pause_on":"待决策"}`),
		// 审查：出口只要求「结论是个二值」，判驳回不算自己没交够。
		newStage(t, specsDir, `{"id":"02","name":"审查","produces":"卡二",
			"entry":[{"from":"卡一","field":"A","check":"nonempty"}],
			"exit":[{"field":"结论","check":"one_of","value":["通过","驳回"]}],
			"reject_on":[{"when":[{"field":"结论","check":"equals","value":"驳回"}],
				"target_field":"驳回至","reasons_field":"驳回理由"}],
			"reject_to":"01"}`),
		// 部署：入口要「结论 = 通过」。缺了 reject_on，02 的驳回就只会被这一段
		// 当成「上游交得不对」退回来，两边对着卡到返工上限。
		newStage(t, specsDir, `{"id":"03","name":"部署","produces":"卡三",
			"entry":[{"from":"卡二","field":"结论","check":"equals","value":"通过"}],
			"exit":[{"field":"D","check":"nonempty"}],"reject_to":"02"}`),
	}, specsDir
}

// 审查者判了驳回，就该走回被点名的那一段——下游**一次都不该被叫醒**。
//
// 这是 验证B 实测出来的死锁：02 合法交付（结论=驳回），03 的入口要 结论=通过，
// 于是 03 把 02 退回来、02 不改结论（证据没变，改了就是伪造）、两边来回，
// 四个回合撞上返工上限，链路一个字节没往前挪。缺的是契约上那条往回走的边。
func TestVerdictRoutesBackInsteadOfBouncingOffDownstream(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := verdictStages(t, dir)
	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)

	rejected := map[string]any{
		"结论": "驳回", "驳回至": "01",
		"驳回理由": []string{"一个超长 user_id 能永久打坏看板"},
	}
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog,
		Budget: 2, MaxRework: 3, Item: "看板",
		Workers: map[string]dispatch.Worker{
			"01": f.worker("卡一", map[string]any{"A": "v1"}, map[string]any{"A": "v2"}),
			"02": f.worker("卡二", rejected, rejected, rejected),
			"03": f.worker("卡三", map[string]any{"D": "ok"}),
		},
	}

	// 01 每次都交同一份没人动的卡 → 02 每次都判驳回 → 该退到 01，来回直到上限。
	err := r.Run(context.Background())
	if !errors.Is(err, ErrNotConverging) {
		t.Fatalf("该以「没收敛」收场，实际 %v", err)
	}
	if f.calls["卡三"] != 0 {
		t.Errorf("02 判了驳回，03 一次都不该被叫醒，实际 %d 次", f.calls["卡三"])
	}
	if f.calls["卡一"] < 2 {
		t.Errorf("该退回 01 让它补，实际只叫醒 %d 次", f.calls["卡一"])
	}
	// 退回时得把审查者的原话带下去，否则 01 只能拿着和上次一样的输入再交一遍。
	if p := f.prompts["卡一"][1]; !strings.Contains(p, "永久打坏看板") {
		t.Errorf("退回去的 prompt 要带审查者写的理由，实际:\n%s", p)
	}

	// 卡点落在被退回的那一段，且写着是审查判的——不是「某段入口判定没过」。
	var sawVerdict bool
	for _, e := range readEvents(t, evLog) {
		s, _ := e["summary"].(string)
		if e["type"] == "blocker" && e["stage"] == "开发" && strings.Contains(s, "审出") {
			sawVerdict = true
		}
	}
	if !sawVerdict {
		t.Error("该留下一条「审查审出开发有问题」的卡点")
	}
}

// 退回来的账要跟状态一起落盘：链路是会断的（停等人、撞南墙都要断），
// 断了再续时那一段如果只知道自己叫 03，就会拿着和上次一样的输入再交一遍。
func TestPendingReasonsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := verdictStages(t, dir)
	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)

	paused := map[string]any{
		"A": "",
		"待决策": []map[string]any{{
			"问题": "超长 user_id 是拒绝还是截断",
			"选项": []map[string]any{
				{"选项": "写入端限长，超长拒收", "代价": "老客户端可能被打回"},
				{"选项": "读取端跳过坏行", "代价": "静默丢一条登录"},
			},
		}},
	}
	rejected := map[string]any{
		"结论": "驳回", "驳回至": "01",
		"驳回理由": []string{"一个超长 user_id 能永久打坏看板"},
	}
	workers := func() map[string]dispatch.Worker {
		return map[string]dispatch.Worker{
			// 第一次交得出，被打回后第二次改口"这得需求方定"，拿到答复再交。
			"01": f.worker("卡一", map[string]any{"A": "ok"}, paused, map[string]any{"A": "ok"}),
			"02": f.worker("卡二", rejected),
			"03": f.worker("卡三", map[string]any{"D": "ok"}),
		}
	}

	// 第一趟：02 判驳回 → 退回 01 → 01 停下来等需求方。
	err := (&Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog,
		Budget: 1, MaxRework: 1, Item: "看板", Workers: workers(),
	}).Run(context.Background())
	if !errors.Is(err, dispatch.ErrAwaitingInput) {
		t.Fatalf("该停在被退回的那一段等人，实际 %v", err)
	}
	st, _ := LoadState(dir)
	if st.Stage != "01" || !st.Awaiting {
		t.Fatalf("状态该记着在 01 等人，实际 stage=%q awaiting=%v", st.Stage, st.Awaiting)
	}
	if len(st.Back["01"]) == 0 {
		t.Fatal("被退回的账没落盘——续跑那一段就看不见要补什么")
	}

	// 第二趟：换一个 Runner（等于进程重启），带着需求方的答复回来。
	brief := filepath.Join(dir, "brief.json")
	if err := os.WriteFile(brief, []byte(`{"口径":"超长拒收"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog,
		Budget: 1, MaxRework: 1, Item: "看板", Brief: brief, Workers: workers(),
	}).Run(context.Background()); !errors.Is(err, ErrNotConverging) {
		t.Fatalf("02 还是交同一份驳回，该撞返工上限，实际 %v", err)
	}

	// 续跑那一次的 prompt 里，两样都得在：审查者写的那句，和需求方的答复。
	p := f.prompts["卡一"][2]
	if !strings.Contains(p, "永久打坏看板") {
		t.Errorf("续跑丢了被退回的理由，那一段只能再交一遍同样的东西:\n%s", p)
	}
	if !strings.Contains(p, "超长拒收") {
		t.Errorf("需求方的答复没进 prompt:\n%s", p)
	}
}

func readEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读事件日志失败: %v", err)
	}
	evs, err := projection.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// 全绿的一条链：三段依次过，最后一段过了就收尾。
func TestWalksTheWholeChain(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)
	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)

	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog, Budget: 2,
		Workers: map[string]dispatch.Worker{
			"01": f.worker("卡一", map[string]any{"A": "ok"}),
			"02": f.worker("卡二", map[string]any{"B": "ok"}),
			"03": f.worker("卡三", map[string]any{"C": "ok"}),
		},
		Item: "活跃度看板",
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("全绿的一条链不该失败: %v", err)
	}

	for _, produces := range []string{"卡一", "卡二", "卡三"} {
		if f.calls[produces] != 1 {
			t.Errorf("%s 该被叫醒 1 次，实际 %d 次", produces, f.calls[produces])
		}
	}

	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done {
		t.Error("走完了状态里该标 done")
	}
	if strings.Join(st.Passed, ",") != "01,02,03" {
		t.Errorf("已过阶段应是 01,02,03，实际 %v", st.Passed)
	}
	if st.Rework != 0 {
		t.Errorf("全绿不该有返工，实际 %d", st.Rework)
	}

	// 前台那行「阶段」不能停在「开发」上——那是上一段的动作，不是这条需求的状态。
	if got := lastStage(t, evLog); got != "全部阶段已完成" {
		t.Errorf("事件里的阶段该收成「全部阶段已完成」，实际 %q", got)
	}
}

// 走完再跑一次不许重做最后一段：收尾之后改的是线上，不是文件。
func TestFinishedChainDoesNotRerun(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)
	f := newFake(dir)
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Budget: 2,
		Workers: map[string]dispatch.Worker{
			"01": f.worker("卡一", map[string]any{"A": "ok"}),
			"02": f.worker("卡二", map[string]any{"B": "ok"}),
			"03": f.worker("卡三", map[string]any{"C": "ok"}, map[string]any{"C": "ok"}),
		},
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.calls["卡三"]
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls["卡三"] != before {
		t.Errorf("走完的链路被重走了：03 又被叫醒了一次")
	}
}

// 停在原地等人：状态得记着停在哪，没带答复再跑一次不许再叫醒——
// 同一个人没答，重演一遍只会白烧一次唤醒。
func TestPauseThenResumeWithBrief(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)
	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)

	pause := map[string]any{
		"A": "",
		"待决策": []map[string]any{{
			"问题": "活跃度按什么算",
			"选项": []map[string]any{
				{"选项": "只算登录", "代价": "漏掉只读不写的人"},
				{"选项": "登录+业务操作", "代价": "埋点改造，晚一周"},
			},
		}},
	}
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog, Budget: 2,
		Workers: map[string]dispatch.Worker{
			"01": f.worker("卡一", pause, map[string]any{"A": "ok"}),
			"02": f.worker("卡二", map[string]any{"B": "ok"}),
			"03": f.worker("卡三", map[string]any{"C": "ok"}),
		},
	}

	err := r.Run(context.Background())
	if !errors.Is(err, dispatch.ErrAwaitingInput) {
		t.Fatalf("该停在等人，实际 %v", err)
	}
	st, _ := LoadState(dir)
	if !st.Awaiting || st.Stage != "01" {
		t.Errorf("状态该记着在 01 等答复，实际 awaiting=%v stage=%q", st.Awaiting, st.Stage)
	}
	if f.calls["卡一"] != 1 {
		t.Fatalf("该只叫醒 1 次，实际 %d", f.calls["卡一"])
	}

	// 没带答复：不叫醒，还是原来那个结论。
	if err := r.Run(context.Background()); !errors.Is(err, dispatch.ErrAwaitingInput) {
		t.Fatalf("没带答复该还是在等人，实际 %v", err)
	}
	if f.calls["卡一"] != 1 {
		t.Errorf("没带答复不该再叫醒，实际叫了 %d 次", f.calls["卡一"])
	}

	// 带上答复：接着走，而且答复要进 prompt——那是后台唯一的"确认"来源。
	brief := filepath.Join(dir, "brief.json")
	if err := os.WriteFile(brief, []byte(`{"口径":"只算登录"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Brief = brief
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("答复回来该能走完: %v", err)
	}
	if got := f.prompts["卡一"][1]; !strings.Contains(got, "只算登录") {
		t.Error("答复没进 prompt——那 Agent 就只能在没有确认来源的情况下瞎写")
	}
	st, _ = LoadState(dir)
	if !st.Done || st.Awaiting {
		t.Errorf("走完该是 done 且不再等人，实际 done=%v awaiting=%v", st.Done, st.Awaiting)
	}

	// 取舍要摆到台面上，不然前台答不了它。
	sawDecision := false
	for _, e := range readEvents(t, evLog) {
		if e["type"] == "decision_needed" {
			sawDecision = true
		}
	}
	if !sawDecision {
		t.Error("停在等人时该产出一条 decision_needed")
	}
}

// 被打回就退回上游。这一次 03 的入口同时要 02 里的两个字段和 01 里的一个字段，
// 缺的东西分别归 01 和 02 补——两条 blocker 各记各的，不许合成一条。
func TestRejectionGoesBackAndDownstreamIsRedone(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)

	// 换掉 03：入口多要两样，一样归 01 补、一样归 02 补。
	stages[2] = newStage(t, specsDir, `{"id":"03","name":"开发","produces":"卡三",
		"entry":[
			{"from":"卡二","field":"B","check":"nonempty"},
			{"from":"卡二","field":"E","check":"nonempty"},
			{"from":"卡一","field":"D","check":"nonempty"}],
		"exit":[{"field":"C","check":"nonempty"}],
		"reject_to":"02","reject_upstream":{"卡一":"01"}}`)

	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog, Budget: 2, MaxRework: 2,
		Workers: map[string]dispatch.Worker{
			"01": f.worker("卡一", map[string]any{"A": "ok"}, map[string]any{"A": "ok", "D": "ok"}),
			"02": f.worker("卡二", map[string]any{"B": "ok"}, map[string]any{"B": "ok", "E": "ok"}),
			"03": f.worker("卡三", map[string]any{"C": "ok"}),
		},
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("补交之后该能走完: %v", err)
	}

	// 退回 01 之后，02 的输入变了，必须重做，不许沿用旧结论。
	if f.calls["卡一"] != 2 || f.calls["卡二"] != 2 {
		t.Errorf("回退点及下游该重做，实际叫醒次数 01=%d 02=%d",
			f.calls["卡一"], f.calls["卡二"])
	}
	// 而 03 只被叫醒一次——它第一次到的时候上游不合格，屏障拦在门口，一次都没启动。
	if f.calls["卡三"] != 1 {
		t.Errorf("03 该只在自己那轮启动一次，实际 %d 次", f.calls["卡三"])
	}

	var targets []string
	for _, e := range readEvents(t, evLog) {
		if e["type"] == "blocker" {
			targets = append(targets, e["stage"].(string))
		}
	}
	if strings.Join(targets, ",") != "澄清,方案" {
		t.Errorf("缺的东西该分别打回 01 与 02，实际 %v", targets)
	}

	// 被退回来的那段必须看见下游点名的条，否则它会把同一份东西再交一遍，
	// 驳回权就成了一道手续。
	second := f.prompts["卡一"][1]
	if !strings.Contains(second, "下游把这份交付退回来了") {
		t.Error("被退回来重跑时该明说是下游退的，而不是自己没干完")
	}
	if !strings.Contains(second, "D") {
		t.Errorf("下游点名要的 D 没进 prompt，01 无从知道要补什么:\n%s", second)
	}

	st, _ := LoadState(dir)
	if st.Rework != 1 {
		t.Errorf("退回一次该记 1 次返工，实际 %d", st.Rework)
	}
	if !st.Done {
		t.Error("补交之后该走完")
	}
}

// 回退点取最靠前的那个：打回 03 与 05 两段时从 03 重走，04、05 在路上会重新被判一次；
// 从 05 走会让 03 的补交没人验收。
func TestEarliestTargetWins(t *testing.T) {
	dir := t.TempDir()
	stages, _ := threeStages(t, dir)
	r := &Runner{Stages: stages}

	got, err := r.earliest([]string{"03", "01", "02"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "01" {
		t.Errorf("该退回最靠前的 01，实际 %q", got)
	}
}

// 上游永远补不齐 = 不收敛。单段的预算管不住这种来回，只有整条链看得见，
// 所以撞南墙的账记在链上，撞了就升级给人工指导者，不是继续转。
func TestReworkCeilingEscalates(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)
	stages[1] = newStage(t, specsDir, `{"id":"02","name":"方案","produces":"卡二",
		"entry":[{"from":"卡一","field":"A","check":"nonempty"},
		         {"from":"卡一","field":"D","check":"nonempty"}],
		"exit":[{"field":"B","check":"nonempty"}],"reject_to":"01"}`)

	evLog := filepath.Join(dir, "events.jsonl")
	f := newFake(dir)
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Events: evLog, Budget: 2, MaxRework: 2,
		Workers: map[string]dispatch.Worker{
			// 01 始终不知道要补 D，所以 02 每次都打回它。
			"01": f.worker("卡一", map[string]any{"A": "ok"}, map[string]any{"A": "ok"},
				map[string]any{"A": "ok"}, map[string]any{"A": "ok"}),
			"02": f.worker("卡二", map[string]any{"B": "ok"}),
		},
	}
	err := r.Run(context.Background())
	if !errors.Is(err, ErrNotConverging) {
		t.Fatalf("返工到上限该报不收敛，实际 %v", err)
	}
	if f.calls["卡二"] != 0 {
		t.Errorf("02 一次都不该启动，实际 %d 次", f.calls["卡二"])
	}
	if f.calls["卡一"] != 3 {
		t.Errorf("01 该被叫醒 1+2 次（首轮 + 两次返工），实际 %d", f.calls["卡一"])
	}

	var escalation map[string]any
	for _, e := range readEvents(t, evLog) {
		if e["type"] == "blocker" && strings.Contains(anyStr(e["summary"]), "人工指导者") {
			escalation = e
		}
	}
	if escalation == nil {
		t.Fatal("撞南墙该上报人工指导者")
	}
	if anyStr(escalation["on"]) == "" {
		t.Error("升级要给得出卡在哪——on 是投影层唯一能摆给前台的字段")
	}
}

// 某一段预算用尽也是断点：状态留在那一段，下次接着来。
func TestBudgetExhaustionStopsTheChain(t *testing.T) {
	dir := t.TempDir()
	stages, specsDir := threeStages(t, dir)
	f := newFake(dir)
	r := &Runner{
		Stages: stages, SpecsDir: specsDir, Dir: dir, Budget: 2,
		Workers: map[string]dispatch.Worker{
			// 01 永远写不出 A，出口永远不过。
			"01": f.worker("卡一", map[string]any{"A": ""}, map[string]any{"A": ""}),
		},
	}
	err := r.Run(context.Background())
	if !errors.Is(err, dispatch.ErrBudgetExhausted) {
		t.Fatalf("该报预算用尽，实际 %v", err)
	}
	st, _ := LoadState(dir)
	if st.Stage != "01" {
		t.Errorf("断点该留在 01，实际 %q", st.Stage)
	}
	if st.Done || len(st.Passed) != 0 {
		t.Errorf("没交好就不该记进已过阶段，实际 passed=%v", st.Passed)
	}
}

// 真实七段树走不走得通。每一段交的就是**本段 spec 自己的出口条件**要的那几条，
// 一条不多——多交了，这个测试就变成"我照着下游的胃口替上游写卡"，那正是要验的东西。
//
// 它是 spec 树的回归闸：谁把某一段的出口条件改窄到覆盖不了下游的入口条件，
// 链路就会在这里卡住或反复返工。
func TestRealTreeWalks(t *testing.T) {
	dir := t.TempDir()
	stages, err := pipeline.Load(filepath.Join("..", "..", "standards", "stages"))
	if err != nil {
		t.Fatal(err)
	}

	cards := map[string]string{
		// 01 出口：目标 / 范围内 / 范围外 / 验收标准 / 确认状态 / 确认来源
		"需求定义卡": `{
			"目标":"做一个活跃度看板，看到每天有多少人登录",
			"范围内":["按天统计登录过的用户数","后台页面能看到"],
			"范围外":["不做留存分析","不做导出"],
			"验收标准":"看板 1 秒内出数，数字与当天登录日志去重后一致",
			"确认状态":"confirmed","确认来源":"im 会话 3f2a7c 消息 #12","待决策":[]}`,
		// 02 出口：做法 / 影响面 / 风险 / 验证方式 / 未标注推断(空)
		"方案卡": `{
			"做法":"新增 daily_login_stats 聚合表，定时任务每小时重算当天，看板只读这张表",
			"影响面":"只新增表与只读接口，不改登录链路",
			"风险":["定时任务失败会导致数据滞后","首次回填占用一次写放大"],
			"验证方式":"本地造 10 万条登录记录，比对聚合结果与直接查询","未标注推断":[]}`,
		// 03 出口：diff / scope / 失败记录(present)
		"变更卡": `{
			"diff":"新增 migrations/0042_daily_login_stats.sql 与 internal/stats/daily.go",
			"scope":"只新增表与只读接口，不改登录链路","失败记录":[]}`,
		// 04 出口：测试命令 / 退出码 / 未覆盖场景(present)；(触及数据路径=false 时不要求日志)
		"测试报告": `{
			"测试命令":"go test ./internal/stats/... && ./scripts/compare-aggregate.sh",
			"退出码":0,"未覆盖场景":["跨天边界的归属规则"],"触及数据路径":false}`,
		// 05 出口：结论 / 证伪尝试；(结论=通过 时要求未覆盖范围)
		"审查记录": `{
			"结论":"通过",
			"证伪尝试":["同一天两条相同 user_id 的登录是否去重——去了"],
			"未覆盖范围":["跨天边界的归属规则只在实现里体现，没有断言"]}`,
		// 06 出口：观测记录(≥2) / 回滚预演日志；(观测到异常=true 时要求故障上报)
		"上线记录": `{
			"观测记录":[
				{"级别":"一级：内部账号","指标":"写入延迟 p99","时间窗口":"14:00–14:30","读数":"1.8s","结论":"正常"},
				{"级别":"二级：20% 流量","指标":"写入延迟 p99","时间窗口":"15:00–15:40","读数":"2.1s","结论":"正常"}],
			"回滚预演日志":"测试环境执行 0042_down.sql，耗时 0.4s","观测到异常":false,"故障上报":false}`,
		// 07 出口：全量套件退出码 / 基线对比(present)；(有回退=true 时要求告警已确认)
		"回归报告": `{
			"全量套件退出码":0,"基线对比":{"用例数":412,"新增失败":0},"有回退":false}`,
	}

	f := newFake(dir)
	workers := map[string]dispatch.Worker{}
	for _, st := range stages {
		body, ok := cards[st.Produces]
		if !ok {
			t.Fatalf("阶段 %s 产出的 %s 没有对应的样本", st.ID, st.Produces)
		}
		workers[st.ID] = f.worker(st.Produces, card(t, body))
	}
	// 03 被 06 打回后照下游点名的条补交——驳回权该有的样子就是这样收场的，
	// 所以链路走到这里会收敛，不是撞南墙。
	workers["03"] = f.worker("变更卡", card(t, cards["变更卡"]), card(t, `{
		"diff":"新增 migrations/0042_daily_login_stats.sql 与 internal/stats/daily.go",
		"scope":"只新增表与只读接口，不改登录链路","失败记录":[],
		"测试报告":"测试报告.json","变更时间":"2026-09-16T14:00:00+08:00",
		"变更步骤":["一级：内部账号灰度，观察 30 分钟","二级：20% 线上流量，观察 40 分钟"],
		"变更风险":"回填历史数据时写放大，可能抬高写入延迟",
		"观测指标":"daily_login_stats 写入延迟 p99",
		"回滚方案":"psql -f 0042_down.sql（测试环境已预演，耗时 0.4s）"}`))

	r := &Runner{
		Stages: stages, SpecsDir: filepath.Join("..", "..", "standards", "stages"),
		Dir: dir, Events: filepath.Join(dir, "events.jsonl"), Budget: 2, MaxRework: 4,
		Workers: workers,
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("按每段自己的 spec 交活，链路该走得通: %v", err)
	}

	st, _ := LoadState(dir)
	if !st.Done || len(st.Passed) != len(stages) {
		t.Fatalf("该走完 %d 段，实际 passed=%v", len(stages), st.Passed)
	}
	// 目前恰好一次：06 的入口要变更卡六字段，而 03 的出口只要 diff/scope/失败记录。
	// 这一条正是驳回权在干活（06 打回 03 补交），不是 bug；但它意味着**每一次**
	// 走真实链路都至少一次返工。上限放宽到 1 是为了这条已知缺口能看清，
	// 一旦 03 的出口条件补齐了六字段，这里会变成 0。
	if st.Rework > 1 {
		t.Errorf("真实链路只该有一次可预期的返工，实际 %d 次", st.Rework)
	}
}

func card(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("样本不是合法 JSON: %v", err)
	}
	return m
}

// 状态文件损坏必须报出来，不许当成"从头开始"——那会把一整条链路悄悄重跑一遍。
func TestBrokenStateIsNotSilentlyReset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{ 这不是 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadState(dir)
	if err == nil {
		t.Fatal("状态文件坏了该报错")
	}
}

func lastStage(t *testing.T, path string) string {
	t.Helper()
	evs := readEvents(t, path)
	for i := len(evs) - 1; i >= 0; i-- {
		if s, ok := evs[i]["stage"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func anyStr(v any) string {
	s, _ := v.(string)
	return s
}
