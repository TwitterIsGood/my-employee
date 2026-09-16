# my-employee

一个**能接入公司 IM 的 AI 员工**的本地技术可行性验证。不做商用。

## 目标

对外只有**一个** Agent（大总管）面对需求方（真实人类）；对内是**多个** Agent 干活的开发流水线，人工指导者可以下场指导。性质要求：**长时间 / 可跟踪状态 / 持续 / 不会一直撞南墙**。

流水线：

```
需求澄清 → 技术方案设计 → 本地开发 → 自测
  → 对抗式审查代码 → 部署验证 → 回归
```

交付标准是「**安全生产六要两不要**」——见 [`standards/delivery.md`](standards/delivery.md)。
每个阶段**额外**要守什么、以及它的**入口条件与驳回权**，见 [`standards/stages/`](standards/stages/index.md)。
顶层准则和阶段规范不重复：全局准则在 `delivery.md` 里只写一遍，阶段文件只说"这一段要守第几条"。

## 架构：两个平面，不可混为一谈

```
┌─────────────────────────────────────────────────────┐
│ 前台平面（1 个 Agent）                                │
│   公司 IM（飞书/Slack）←→ 大总管 ←→ 需求方（人类）      │
│   职责：澄清需求、派活、把已确认事实投影回去             │
└───────────────────────┬─────────────────────────────┘
                        │ 窄接口：派发 / 状态投影（白名单）
┌───────────────────────┴─────────────────────────────┐
│ 后台平面（多个 Agent，在一个群里互相沟通）              │
│   需求预评审 → 方案设计 → 开发 → 自测 → 对抗审查 → 验证 │
│   人工指导者可以随时插进来指导                          │
└─────────────────────────────────────────────────────┘
```

两条硬约束：

1. **前台读到的只能是白名单投影，不是原始后台记录。** 过滤标准是**确定性**，不是好坏——不报未定论的过程（重试次数、失败假设、后台争论），必须报已确认的事实（失败、变更、影响面、观测数字）。**已落地**：`cmd/gate` 是前台每轮上下文的唯一来源，原始事件日志它连路径都拿不到。
2. **闸门住在 harness，不住在 Agent 里。** harness 是唯一同时接触「模型说了什么」和「环境真的发生了什么」的一层。靠 prompt 自觉守不住——见下面验证 A 的缺陷 1。

## 取和舍

不造轮子。三份开源方案各取所需：

| 层 | 取自 | 舍 |
|---|---|---|
| 前台（IM 接入、会话、鉴权、去重） | **multica** 的 channel 引擎——平台无关内核 + 飞书/Slack 薄适配器 | 不用它的 issue 当流水线载体 |
| 后台（多 Agent 编排、状态注入、spec 注入、事件日志） | **Trellis** 的 channel + hook 层 | 不用它的 worker 做长会话（一次唤醒只跑一轮） |
| 编排范式（队长派活、阶段屏障、独立验收） | **multica** 的 squad leader + 父子 issue + stage 模型 | — |

## 目录

Go，除 `front/` 那个假 IM 适配器（一次性的、换真飞书时整块丢掉）。

```
cmd/gate/                  前台上下文闸门 —— UserPromptSubmit hook，非 Agent
cmd/projector/             投影器 CLI
cmd/pipeline/              后台流水线判定器（入口/出口条件、驳回）
cmd/stage/                 把某个阶段派给一次唤醒（判入口 → 唤醒 → 判出口）
cmd/egress-listener/       出口观测器（验证「备胎真的生效了」）
internal/projection/       投影与校验（纯函数，回归闸 = projection_test.go）
internal/pipeline/         阶段契约的加载与判定（入口条件即上游的验收标准）
internal/dispatch/         一次唤醒跑一个阶段：上游不够不启动，自己没交好带原因重跑
internal/events/           后台事件日志的写入口（seq + JSONL）
internal/spec/             阶段 spec 树自己的完整性测试（契约缺一节就失败）
standards/delivery.md      六要两不要（单一事实源，双渲染成散文 + 闸门谓词）
standards/stages/          阶段 spec 树：7 个阶段各自的入口/出口/闸门/驳回权
standards/projection.md    投影规则（前台能看到的 vs 看不到的）
standards/resilience.md    上游韧性（从一个真实故障里总结）
front/                     前台：大总管指令、假 IM 适配器、延迟探针
back/fixtures/             投影的回归样本（含真实泄漏原文）
runs/                      验证记录与证据
local/                     gitignored：凭据、Agent settings、运行时状态
```

## 已完成的验证

- **验证 A（主 Agent 沟通自然度）—— 部分通过。** 澄清纪律、不撒谎、定位真实阻塞点都过；投影纪律有真缺陷；延迟已定位并找到杠杆。
  - 证据：[`runs/2026-09-16_validation-A_findings.md`](runs/2026-09-16_validation-A_findings.md)
  - 对话原文：[`runs/2026-09-16_validation-A_run1.txt`](runs/2026-09-16_validation-A_run1.txt)

## 复现

```bash
make build        # 产出 local/bin/{gate,projector,pipeline,stage,egress-listener}
make test         # 投影层回归闸 + 阶段契约 + 流水线判定 + 派活

# 只看投影：后台事件 -> 前台唯一可见的状态文档
./local/bin/projector back/fixtures/events-active-users.jsonl --out local/tmp/proj.md

# 后台流水线：阶段契约就是流水线定义
./local/bin/pipeline stages
./local/bin/pipeline gate 02 entry back/fixtures/entry-02-reject.json --item 活跃度看板
# 驳回 -> 一条 blocker 事件 -> 前台看到「卡在哪」
./local/bin/pipeline gate 02 entry back/fixtures/entry-02-reject.json \
    --item 活跃度看板 --events local/tmp/events.jsonl

# 派一个阶段给一次唤醒：判上游 -> 唤醒 -> 判交付
./local/bin/stage run 01 --dir local/tmp/art \
    --item "我想要个活跃度看板" --events local/tmp/events.jsonl --budget 2 \
    --worker-cmd "claude -p --settings local/agents/stage-worker.settings.json \
                  --dangerously-skip-permissions"
# 非 Agent 阶段（07 回归）换 --worker-cmd 就行，判定逻辑完全一样

# 口径定不下来时它会停住等人（退出码 3），取舍经投影摆到需求方面前；
# 答复回来再叫醒同一段——后台唯一的"确认"来源就是这份答复
./local/bin/stage run 01 --dir local/tmp/art --brief local/tmp/brief.json …

# 假 IM 适配器（用 multica chat API 当传输层）
cd front
./im.sh new 会话名                  # 新建会话，打印 session id
./im.sh say <sid> "需求描述"         # 需求方发言
./im.sh log <sid>                    # 打印全部消息

# 延迟探针
python3 timing_probe.py <sid> "问题"
```

依赖：本机跑着 multica（`multica config` 里的 `server_url`），凭据读自 `~/.multica/config.json`，不落在仓库里。

Agent 侧接闸门的做法：

```bash
multica agent update <id> --custom-args '["--settings","<abs>/local/agents/<name>.settings.json"]'
```

那个 settings 文件里同时挂着出口（`env.ANTHROPIC_BASE_URL` 等）和闸门（`hooks.UserPromptSubmit`）。
两个都必须走它，原因见 [`standards/resilience.md`](standards/resilience.md) 第 5 条——
`custom_env` 是静默失效的，配了看着正常，请求根本不走。
