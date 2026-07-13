# DESIGN — HITL 强操控(@bot 中止 · 全 thread 上下文 · 追问轻量化)

状态:草稿(2026-07-13)。本文是把一次 #718 真机 demo 暴露的问题收敛成一套设计,供对齐后分阶段实现。**先设计、再实现**。

关联:`DESIGN-hitl-transports.md`、`DESIGN-human-takeover.md`、`GUIDE-hitl-setup.md`。

---

## 1. 动机(这次 demo 实锤的四个坑)

`open-design` 的 Plane #718(设计系统导入品牌色/logo)走 node-H 全流程时,暴露:

1. **人类消息没有"随时随地"的响应点。** 产品负责人孙庆雨在 thread 里 @Looper 问"从哪里导入的品牌",消息进了 `humanInbox` 却**卡住没被处理** —— 当时 planner 正在跑 AUTHOR→GRILL→REVIEW,消息只能等当前这轮结束,而这轮跑完直接到审批门就停了,她的问题**至今没答**。looper 只在 pipeline 步骤之间 drain 消息,不是"随时"。

2. **一条追问 = 整条流水线重跑 + 重复刷屏。** 她那条 pending 消息触发 `followupResume`,而 followup 的设计是"回到 writeSpec,再把 grill/review 这些'幂等门'重跑一遍"。对 spec 内容是幂等的,**但对 thread 不幂等** —— 每重跑一次就把"初稿 FYI / GRILL 拷问 / REVIEW"又发一遍。用户看到的"重启后又从头搞一遍"就是这个:**不是 agent 忘了(native session 有 resume),是编排层重放 + 重复发帖**。

3. **不区分 @bot 和闲聊。** `pollFeishuHITLInboxOnce`(`hitl_feishu_poll.go`)对任何带 `rootId` 的文字回复都 `enqueueMessage → 唤醒 loop`。多人在 thread 下讨论时,每句都会试图唤醒 bot —— 该"@bot 才动,闲聊只当上下文"。

4. **产品看不懂技术腔。** GRILL 拷问结论满屏 `token-contract.ts` / `validateProjectDesignSystemId`,加上一个**显示 bug**(agent 无 `__LOOPER_RESULT__` marker 时,Summary 回退抓了 stream-json 的原始 `{"type":"result",…}` 事件行)直接把原始 JSON 喷进 thread。显示 bug 已修(见 §6);口吻问题要系统解决。

---

## 2. 目标

把"人的操控感"做到最强,同时不让 bot 被闲聊 thrash:

- **G1** 人 @bot 的消息**立刻**得到响应 —— 哪怕 agent 正在跑,也中止当前 turn、立即处理这条。
- **G2** 非 @bot 的消息**只当共享上下文累积**,不打断、不刷屏。
- **G3** bot resume 时能读到 **thread 的完整历史**(含没 @ 它的对话),显式知道"上次 @ 至今新增了哪些"。
- **G4** 一条追问是**同 session 里的轻量回应**(在 thread 答一句),不是重跑整条 author→grill→review + 重刷所有帖。
- **G5** 面向产品的内容用**产品能懂的人话**(见 §7 口吻范本),技术腔留给共享上下文/spec 页。

非目标:重排"共享上下文 thread"本身 —— 用户明确"所有相关都放这条 thread 当共享上下文,产品只管 @他的内容",故**不拆产品/工程双 thread**。

---

## 3. 设计

### 3.1 消息分级(@bot vs 闲聊)

`feishuInboxEvent`(`hitl_feishu_poll.go`)现在只有 `Text/RootID/SenderOpenID`,**拿不到"是否 @了 bot"**。飞书文本里 @bot 渲染成 `@_user_1` 之类占位,靠文本匹配不可靠。

- **CF 飞书服务(不在本仓)**:在推给 `/events` 的事件里加 `mentionedBot: bool`(从飞书 webhook 的 `mentions` 字段判定 bot 的 open_id/app_id 是否在内)。这是 @判定的**唯一可信来源**。
- **looper 侧**:`feishuInboxEvent` 加 `MentionedBot bool`;poll 按它路由:
  - `mentionedBot=true` → **打断路径**(§3.2)
  - `mentionedBot=false` → **累积路径**(§3.3)

过渡期(CF 未改前):可退化为"全部当 @bot"(现状行为)或"全部只累积",由 config flag 控制,避免半成品上线。

### 3.2 @bot 打断(G1)

poll 收到 `mentionedBot=true` 的消息、且目标 loop 有**正在跑的 agent**:

1. 按 `agent_executions.pid` **kill 掉在跑的子进程**(claude/codex)。native session 在跑的第一条事件就被捕获(`claude_jsonl.go` 等),所以杀掉不丢 session。
2. 把消息写入 `humanInbox`,标记 loop 需要"打断式 resume"。
3. **native-resume**:复用现有 followup-resume 机器,但触发源从"正常完成"换成"被打断",且 resume 的目标是**先回应这条消息**(§3.4),而非重跑 pipeline。

若 loop 已 parked/completed(无在跑 agent):走现有 reactivation(`enqueueHumanMessageToLoop`),只是也要接 §3.4 的轻量回应。

**硬点**:一个 turn 跑一半被 kill,claude `--resume` 会从**上一个已完成的 turn** 续(半截 turn 的活丢掉 —— 人主动打断,可接受)。**要真机验一个 mid-turn kill 后 resume 是否干净**(session 状态是否一致、有没有半条 tool_use 残留卡住)。

### 3.3 闲聊累积(G2)

`mentionedBot=false`:**不打断、不唤醒、不发帖**,只把消息追加进 **thread 镜像**(§3.5)。多人讨论自由进行,bot 不动。等下一次有人 @bot 时,§3.4 会把这期间的新增一并带入。

### 3.4 追问 = 轻量回应,不重跑 pipeline(G4)

这是最核心的修法。现在 `followupResume` 无脑重跑 writeSpec→grill→review。改成:

- resume 时先判断**这条消息要什么**:
  - **澄清/追问**(如"从哪导入的品牌")→ 只在**同 session** 里让 agent 读全上下文(§3.5)+ 回一句到 thread(§3.6 卡片),**不动 spec、不重跑 grill/review**。
  - **实质变更**(改了产品决策、要求改方案)→ 才回到 writeSpec 增量改 spec;且 grill/review 若要重跑,**其 thread 发帖必须按 marker 去重**(同一阶段不重复刷)。
- 落点:`internal/planner/runner.go` 的 followupResume 分支 + node-H 各 `postNodeHThreadNote` 加"本 loop 本阶段已发过"marker(存 loop metadata),重放时跳过。

判定"澄清 vs 变更"可以让 agent 自己在 resume 的第一步输出一个意图标签(轻量、无副作用),据此分流。

### 3.5 全 thread 上下文(G3)—— thread 镜像

bot 只喂"@它的那条"太窄。维护一份 **thread 镜像**:

- **CF 服务(不在本仓)**:把 thread 里**所有**消息(不只 @bot 的)落一份(推给 looper 或写共享存储),含 sender、时间、文本、是否 @bot。
- **looper 侧**:本地存 thread 镜像(可复用/扩展 `feishu_threads`,或新表 `feishu_thread_messages(root, seq, sender, at, text, mentioned_bot)`)。
- **resume 喂 context**:agent 拿到"**上次 @ 你至今新增的对话在这里(从 seq X 到 Y):镜像文件路径 = …,自己翻**"。显式给范围 + 文件,让 agent 自由读全 thread,而不是只塞一条。

比现拉飞书 API 更稳:离线可读、能给显式范围、不受 API 限流。

### 3.6 实时回复卡片(仿 lark-coding-agent-bridge)

回应/追问用 §7 口吻的 lark_md 卡片(已有 `PostThreadDecisionCard` 可复用/推广),而非纯文本。

---

## 4. 组件改动地图

| 组件 | 改动 | 在本仓? |
|---|---|---|
| **CF 飞书服务** | 事件加 `mentionedBot`;thread 全量消息落库/透传 | ❌ 独立部署,要一起动 |
| `hitl_feishu_poll.go` | `feishuInboxEvent` 加字段;按 @判定路由(打断 / 累积) | ✅ |
| `hitl_github_poll.go` / `enqueueHumanMessageToLoop` | 打断式 resume 入口;轻量回应分流 | ✅ |
| planner `runner.go` | followupResume 分流(澄清 vs 变更);node-H 发帖按 marker 去重 | ✅ |
| worker `runner.go` | 同样接打断/轻量回应(worker 也在 thread 里) | ✅ |
| executor / `*_jsonl.go` | mid-turn kill 后 resume 的健壮性验证 | ✅ |
| notify `gateway.go` | thread 镜像存取;回复卡片推广 | ✅ |
| prompt(`plannerProductAskInstruction` 等) | §7 产品口吻范本 | ✅ |

---

## 5. 硬点 / 待定

- **mid-turn kill 的 resume 干净度**:必须真机验(claude `--resume` 对被杀在半途的 session)。若不干净,退化为"标记中止意图 + 等当前 turn 自然结束再插入",牺牲一点即时性换稳。
- **@判定信号**:强依赖 CF 服务透传 mention。CF 未就绪前用 flag 退化,别上半成品。
- **"澄清 vs 变更"判定**:让 agent 出意图标签 vs 规则判定 —— 倾向前者(agent 更准),但要防它把澄清也当变更去重跑。
- **镜像存储**:复用 `feishu_threads` 还是新表;容量/清理策略。
- **多 loop 共享一 thread**:一个 task 的 coordinator/planner/worker 共用 thread,打断要作用到"当前活跃的那个 loop"(`feishu_threads.loop_id` 已随复用更新到最新 loop,可用)。

---

## 6. 已完成(本 session,不属于本设计但相关)

- **显示 bug 修复**(`95e1241`):agent 无 `__LOOPER_RESULT__` marker 时,Summary 回退从"抓 stream-json 原始事件行"改成"用 translator 的干净最终文本"(`assistantText`/`combinedText`)。这就是 GRILL 拷问被喷成原始 JSON 的根因。
- **productAsk 结构化 card**(`0640cbd`):header-less lark_md 卡片 + prompt 结构化(背景/现状/需要你拍板)。
- **closed-task ack 去重**(`8ca0d99`)、**卡片状态真实化**(`66e449e`)、**auto-@产品**(`4cee616`)。

---

## 7. 产品沟通口吻范本(G5)

2026-07-13 用户给的标准:群里「智能体」把 looper 技术输出翻成产品能懂的人话,用户明确"**这样他就看得懂,你发的那种看不懂**"。范式:

```
它在干什么
<一句白话说清此刻在做啥>

挖出的病根(最关键的一条)
- <bullet,关键词加粗>
- <把机制翻成用户能想象的画面>
结论:<一句白话>

修复办法
<白话说清怎么修>

一句话总结
<给个抓得住的生活化比喻,如"被当成草稿雪藏了,谁都不理它">
```

原则:**所有黑话翻成用户能想象的画面**("draft 状态两道闸拦死"→"草稿雪藏,谁都不理它");加粗小标题 + bullet;一句话总结收尾。这是 looper @产品时的目标口吻,要调进 `plannerProductAskInstruction` 及一切面向产品的输出。技术腔只留给共享上下文的 spec 页/工程记录。

---

## 8. 分阶段实现(建议)

1. **P0 止血(已做)**:显示 bug 修复 + productAsk 卡片化。
2. **P1 追问轻量化(G4,纯 looper 侧)**:followupResume 分流 + node-H 发帖去重。**不依赖 CF**,先做,直接消除"重跑刷屏"。
3. **P2 全 thread 上下文(G3)**:thread 镜像 + resume 喂范围。looper 侧建表 + 喂 context;CF 侧全量落库。
4. **P3 @bot 打断(G1/G2)**:CF 加 `mentionedBot` → poll 路由 → mid-turn kill + resume(先真机验干净度)。
5. **P4 口吻(G5)**:prompt 调成 §7 范本,面向产品的输出统一走。
