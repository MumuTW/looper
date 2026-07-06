# PLAN — looper HITL 改造全貌

> 状态:草案 · 待拍板。汇总真机 e2e(v0.10.1)+ 共享群试用暴露的问题与由此确定的改造方向。
> 面向:每人本地部署一套 looper、决策卡发到同一个飞书群的团队场景。

## 1. 背景与动机

HITL 已随 **v0.10.1** 上线(决策卡、多轮对话、任务 thread、按人 Plane 路由、一键接管)。用真机跑「fresh agent 照 SETUP-PROMPT 从零配 + 冒烟」并把卡片发进共享群后,暴露出一批 **UX 含糊 / 正确性 / 健壮性** 问题。这份 plan 把它们和对应改造一次性理清,作为后续分 PR 实现的依据。

一句话目标:**卡片是「任务」的精准实时镜像,label 是「意图」不随阶段变身,断电能自恢复,只有合并才算完成。**

## 2. 现状问题清单(真机 e2e 实测)

| # | 现象 | 根因 |
|---|---|---|
| P1 | 同一任务**发了 2–3 张 anchor 卡** | planner loop 竞态 11ms 双发;且 planner/worker 是两个 loop 各发一张。`feishu_threads` 无「按任务去重」 |
| P2 | 「处理中」卡片**露裸命令**(`tmpdir=$(mktemp …`) | 标题由守护进程反推工具流;无里程碑时 fallback 成裸命令 |
| P3 | 「✅ 已完成」**误导** | worker 的 completed = 只是「开了 PR」,PR 实际 `OPEN/未合并`;下游还有 review/QA/冲突/CI/合并 |
| P4 | 一个 issue **冒出 2 个 PR** | 默认 planner+worker 都吃 `looper:plan`(且 bundle 误配 worker 也吃 plan)→ 各出一个 PR |
| P5 | 任务完成后 thread 里**追问不理人** | 终态 loop 直接跳过入站消息(`hitl_github_poll.go:161`) |
| P6 | onboarding 用 `nohup looperd &` | 裸后台进程,**重启/合盖/登出就没了、崩了不拉起** |

## 3. 改造项

### A. 卡片 = 任务/PR 状态的精准实时镜像

**原则**:标题反映**真实状态**,不用 looper 内部那套「处理中/已完成」。**只有「已合并」是绿色终态。**

状态映射(数据来自 `gh pr view`:`state / isDraft / reviewDecision / mergeable / statusCheckRollup / mergedAt`):

| PR 实况 | 标题 |
|---|---|
| draft | 📝 草稿 · PR #N |
| CI 跑测中 | 🔄 CI 检查中 · PR #N |
| CI 失败 | ❌ CI 失败 · PR #N |
| 冲突(CONFLICTING) | ⚠️ 冲突待解 · PR #N |
| 待 review | 👀 待 review · PR #N |
| 请求修改 | ✋ 待修改 · PR #N |
| 已批准+可合并+绿 | ✅ 待合并 · PR #N |
| **merged** | 🎉 **已合并** · PR #N ←唯一终态 |
| closed 未合并 | 🚫 已关闭 · PR #N |

**状态怎么来 —— agent 自报 + webhook,不用轮询 issue 关闭(太滞后):**
- **活跃干活时 = agent 自报**:给 agent 一个 looper 工具/CLI(暴露现成的 `RecordMilestone`,如 `looper milestone "已开 PR,待 review"`),**在 prompt 里写清期望的状态词表**,agent 边干边更新卡片。
- **空档的外部事件**(人去 review / 合并 / CI 出结果)→ 走 **GitHub webhook 实时接**(looper 本就支持 `webhook`),不 poll issue 关闭。

### B. Thread 绑「任务」,不绑 PR、不绑 loop

**事实**:一个 issue 可能有多个 PR(planner 的 spec PR + worker 的实现 PR,甚至多个实现 PR)。e2e 里 issue #1 就开了 PR #2 + #3。

- **一个任务(issue / Plane work-item)= 一个 thread**;planner + 多个 worker + 多个 PR + reviewer/fixer **全汇进这一条**。
- 卡片头 = **任务级汇总**;正文逐条列每个 PR 的实时状态。
- 关联现成:worker loop 的 metadata 存了 `IssueNumber` + `prNumber`;reviewer/fixer 可回溯源 issue。
- **任务完成 = issue 解决 / 名下 PR 全合并**,不是单个 PR。

示例(多 PR):
```
🔧 进行中 · #1 · 2 PR
──────────────
· PR #2 spec  👀 待 review
· PR #3 impl  ✅ 待合并
```

### C. 三档 label + 新增全自动档(label 不变身)

**原则**:**label = 意图**,阶段推进是 looper 内部的事,**不靠改 label 驱动**;阶段可见性交给卡片。

| Label(全程不变) | looper 内部怎么走 |
|---|---|
| `looper:plan` | 规划 → **停,等人评审 spec** |
| `looper:worker-ready` | **直接实现** |
| **`looper:auto`**(新) | 规划 →(**内部交棒**)→ 实现,一路到实现完成 |

- `looper:auto`:planner 发现 → 写 spec → **完成时内部直接把 worker 工作入队**(带 spec + issue 上下文),**不改 label**。worker 接住实现到底。
- **全程 HITL 照常**:真拿不准仍发决策卡问人。「自动」≠「不问」,只是不用人手动推进阶段。
- 修 bundle 误配:worker 触发器应是 `looper:worker-ready`,不是 `looper:plan`。

### D. spec 的编写与评审 —— 用 spec-forge(前端),looper 只管执行(后端)

**分层,不内嵌**:
- **spec-forge(Agent Skill)= 前端**:AUTHOR 写齐 → GRILL 独立 fresh agent 对抗拷问 → DECIDE(能 AFK 还是要 HITL)→ PUBLISH 到 Plane work item、置 `spec:reviewing`、交人 approve。
- **looper = 执行后端**:approve 后,Plane item 按 spec-forge 的 execution_mode 打 looper label(能 AFK → `looper:auto`;要人盯 → `looper:plan`/`worker-ready`),**looper 原生 Plane provider 接手实现已批准的 spec**。

**好处**:①spec 被 grill 过、质量高,胜过 looper planner 的「autonomous 写个 md」;②**评审在 Plane 的 `spec:reviewing → approve`,不是悬空 GitHub PR** → 根治「已批准但不合并的 spec PR」;③execution_mode 直接决定 looper 档;④looper 自带 planner **降级**为轻量可选或弃用。

**为什么松耦合、不把 spec-forge 塞进 looper planner**:GRILL 的对抗价值要独立 fresh agent + 真人,塞进 looper 的自主 loop 里成本高且削弱对抗性;两层靠 Plane label 衔接即可(技术上 looper 的 planner 能调 SKILL.md,但不划算)。

**待定**:approve 后谁给 Plane item 打 looper label —— 人手动 / spec-forge PUBLISH 时按 DECIDE 预置 / approve→label 小 hook。

> 简单活(无需 spec)不走这条:直接 `looper:worker-ready`(或 `looper:auto` 让 looper 自己规划实现,不产出正式 spec 评审)。

### E. 恢复 / 本地部署健壮性(笔记本随时关)

**已有(不用造)**:launchd 托管守护进程(`looper daemon install/start`,`RunAtLoad` 自启 + 保活)、启动恢复管线(标记中断 run / 清孤儿 agent / requeue)、checkpoint 续跑、native session 续跑。

**要改 / 要加固**:
1. **onboarding 改用 `looper daemon install` + `looper daemon start`**(替掉 `nohup &`)→ 合盖/重启后 launchd 自动拉起 → 恢复管线自动续。**(P6,笔记本场景关键一改)**
2. **睡眠 ≠ 关机**:合盖是挂起进程(codex 子进程可能死、token/网络过期);醒来后 reaper/reconcile 要能发现并重新驱动 —— 需专门测。
3. **恢复幂等**:续跑**不得重复开 PR / 重复发卡** —— 与 P1 的 anchor 去重**同源**,合并处理。
4. **半截被杀**:worktree 有半截改动时 `replay_step` 要能干净叠加 —— 验常见「实现到一半被杀」。

### F. 完成后追问(P5)

PR 未合并前 thread 一直活,追问可路由(转 fixer/looper 应答)。**合并后**收到追问,三选一(待定):
- **A** 自动重开/续跑(当新一轮需求或答疑)
- **B** 至少回一句「本任务已完成,如需继续请在 issue/PR 上说或新建任务」,不冷场
- **C** 保持静默(现状)

### G. 独立 bug 修复

- **P1 双发**:`feishu_threads` 改按任务绑定 + 唯一约束 + insert-if-absent → 杜绝竞态双发;planner/worker 复用同一 thread。
- **P2 裸命令**:标题派生**永不显示裸命令**,PR 前显示友好措辞(「🔧 实现中…」),原始工具流只留 thread 内。
- **P3/措辞**:被 A 的状态映射吸收(不再有含糊「已完成」)。

## 4. 与 synclo AFK 的收敛

`looper:auto` = **looper 原生的「全自主执行」触发**,正是让 synclo `afk:go` 那套自建执行器**退役**的前提:AFK 只保留上游(triage / 分解 / 派单),产出改成打 `looper:auto` + assign,**执行整个交给 looper**。桥(`looper-bridge.ts` 建 GitHub issue 那层)也随 looper 原生 Plane provider 退役。**注意:同一 item 别既 afk:go 又 looper:auto,否则两执行器抢着做。**

## 5. 分阶段实施(建议 PR 拆分)

1. **PR-1 onboarding 健壮性**(低风险,先上):bundle 启动改 `looper daemon install/start`;关 planner(或改用 auto);修 worker 触发 label。
2. **PR-2 anchor 去重 + 绑任务 + 去裸命令**(P1/P2 + B 的表结构):`feishu_threads` 迁移 + 按任务键 + 友好措辞。
3. **PR-3 卡片 = PR 状态镜像**(A):agent 自报工具/CLI + prompt 词表 + webhook 接外部事件 + 状态映射 + 里程碑。
4. **PR-4 `looper:auto` 全自动档**(C/D):新 label + planner 完成内部交棒 + spec 内联。
5. **PR-5 恢复加固**(E 2/3/4):睡眠唤醒、幂等、半截续跑。
6. **PR-6 完成后追问**(F,取决于 A/B/C 决策)。

## 6. 待你拍板的决策清单

- [ ] **auto 档 label 名**:`looper:auto` / `looper:ship` / `looper:build`?(`looper:go` 别用,撞 afk:go)
- [ ] **rollout 默认档**:worker-only(关 planner)/ 全 auto / 保留 plan 评审流水线?
- [ ] **spec 走 spec-forge**(Plane 评审)而非 looper planner —— 确认这个分层?
- [ ] **approve→looper label 谁来打**:人手动 / spec-forge PUBLISH 预置 / approve 小 hook?
- [ ] **「任务完成」判定**:源 issue 关闭 / 名下 PR 全合并 / 两者兼顾?
- [ ] **完成后追问**:A / B / C?
- [ ] **状态刷新**:webhook 为主 + 兜底轮询间隔(如 60s)?
- [ ] **测试群 chat_id**:给一个独立测试群(不碰「Looper 协作」)供 live 验证。

## 7. 验证与灰度

- **绝不**再用生产群「Looper 协作」做测试(研发已入群)。飞书 live 验证只在**独立测试群**。
- 每个 PR:单元测试 + `scripts/verify.sh`(gofmt/vet/test/build)+ 在测试群/scratch 仓过一遍真机。
- 安全尾巴:轮换泄露过的 Plane key `plane_api_666a…`(每人用自己的)。

---
🤖 与 Claude Code 协作整理
