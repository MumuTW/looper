# Task / TaskItems 删除 + PR-Oriented Worker 重构计划

## 1. 目标

在不影响当前 **PR-driven reviewer / fixer** 主流程的前提下：

- 删除 `tasks`
- 删除 `task_items`
- 删除 task API / CLI
- 删除 `taskId` 字段和 task target model
- 保留 `worker` loop type
- 将 `worker` 重构为 **PR-oriented worker**

这里的 PR-oriented worker 指：

> worker 不再围绕 `task` 持久化实体运转，而是围绕“从用户输入出发，创建并推进一个 PR”这个目标运转。

---

## 2. 最终设计原则

### 2.1 PR 是统一主线

最终产品形态统一为：

- `worker`：创建 PR
- `reviewer`：审查 PR
- `fixer`：修复 PR

三者都围绕 `pull_request` 工作，只是生命周期阶段不同。

### 2.2 删除中间持久化实体，保留执行角色

要删除的是：

- `task`
- `task_items`

要保留的是：

- `worker` 这个主动开发角色

### 2.3 计划/分解状态不再放数据库表

原来由 `task_items` 承载的 checklist / plan / slice 状态，改为放在：

- worker checkpoint
- queue payload
- 或 run checkpoint

不再单独维护 `task_items` 表。

### 2.4 开发期直接收敛到最终形态

由于项目仍处于开发阶段：

- 不保留兼容旧 schema 的包袱
- 不保留 `taskId` 残余列
- 不保留 `task` target type

---

## 3. 单 PR 范围

这个 PR 一次性完成以下事情：

1. 删除 task CLI
2. 删除 `/api/v1/tasks*`
3. 删除 `task` target type
4. 删除 `tasks` / `task_items` store 与 schema
5. 删除所有 `taskId` 字段和透传
6. 保留 `worker`，但把它改成不依赖 task 的新模型
7. 更新 runtime / scheduler / tests / docs

---

## 4. Worker 新模型

## 4.1 Worker 输入

worker 不再接受 `taskId`。

新的 worker 启动输入应至少包含：

- `projectId`
- `repo`
- `baseBranch`
- `prompt` 或 `specPath`

这些输入可以来自：

- 新的 API payload
- queue payload
- loop config / metadata

### 建议

第一阶段优先用 **queue payload / loop config**，而不是再建一个新的 `work_request` 表。

原因：

- 避免把 `task` 换名再引入一个新的中间实体
- worker 的过程状态本来就更适合 checkpoint
- schema 更简单

### 4.1.1 Worker 的 `loop.target_type`

这是实现前必须先定死的点。

定稿方案：

- reviewer / fixer：`targetType = "pull_request"`
- worker：`targetType = "project"`

worker 的真实输入（`prompt/specPath/repo/baseBranch/title`）放入：

- `loops.metadataJson`
- 必要时镜像到 `queue.payloadJson`

不再引入新的 `task` / `work_request` 持久化实体。

## 4.2 Worker 目标

worker 的业务目标改为：

> 从用户输入出发，在独立 worktree 中生成代码、验证、提交、push，并创建一个新的 PR。

因此：

- worker 本身是“PR 生产者”
- reviewer / fixer 是“PR 处理者”

## 4.3 Worker 状态承载

删除 task/task_items 后，以下状态不再落表：

- checklist items
- planned item ids
- remaining item ids

改为保存在 worker checkpoint 中，例如：

- `plan.items[]`
- `plan.completed[]`
- `execution`
- `validation`
- `pullRequest`

### 4.4 Worker API Entry Point

删除 `/api/v1/tasks*` 后，必须补新的 worker 启动入口。

定稿方案：

- 服务端：`POST /api/v1/workers`
- CLI：`looper work`

建议输入：

- `projectId`
- `repo`（可选）
- `baseBranch`（可选）
- `prompt` 或 `specPath`
- `title`（可选）

行为：

- 创建 `worker` loop
- 写入 `loops.metadataJson` / `queue.payloadJson`
- enqueue
- 后续由 worker 创建 branch、改代码、开 PR

### 4.5 Worker Queue Item Shape

定稿 queue item 形态：

- `type = "worker"`
- `targetType = "project"`
- `targetId = projectId`
- `payloadJson` 承载：
  - `prompt`
  - `specPath`
  - `repo`
  - `baseBranch`
  - `title`

#### lockKey

定稿：

- `worker:<loopId>`

第一阶段以 loop 维度幂等为准；如果后续需要项目级串行化，再单独收紧。

#### dedupeKey

定稿：

- `worker:<loopId>`

### 4.5.1 Worker 与 PR 的关联记录

worker 的输入 target 是 `project`，但仍需要记录它最终创建/拥有的 PR。

定稿方案：

- durable association 放在 `loops` 表：
  - `loops.repo`
  - `loops.prNumber`
- 运行态/恢复态信息放在 checkpoint：
  - `checkpoint.pullRequest`

状态演进：

- worker 创建时：
  - `targetType = "project"`
  - `targetId = projectId`
  - `repo = null`
  - `prNumber = null`
- `open-pr` 成功后：
  - 回写 `loops.repo`
  - 回写 `loops.prNumber`

这让 worker 既保留 project-scoped 输入语义，也拥有 durable PR ownership 记录。

### 4.6 Worker 是否 requeue

建议第一阶段定义为：

> 单次输入 -> 单次执行 -> 创建/更新一个 PR 的单轮流程。

因此：

- 不再沿用 `remainingItemIds` 的 task/checklist requeue 逻辑
- 围绕 task slice 的 `handlePostRunSuccess` requeue 逻辑应删除
- 未来若需要多轮推进，应基于 PR 状态或显式再次触发，而不是基于 `task_items`

### 4.7 `openPrStrategy` 处理建议

当前策略带有 checklist 驱动语义。

删除 task/checklist 后，建议：

- 第一阶段只保留：
  - 自动开 PR
  - 或手动开 PR
- 删除 `all_done` / `first_commit` 这类依赖 checklist 的策略

### 4.8 CLI UX

定稿 CLI 形态：

```bash
looper work --project <projectId> --title "..." --spec <path>
```

可选扩展：

- `--item <text>` 多次传入
- `--repo <owner/name>`
- `--base-branch <branch>`

不再保留：

- `task create`
- `task start`
- `loop start --task`

---

## 5. 建议的 worker 流程

建议将 worker 流程重构为更接近 fixer 的形态：

1. `prepare-worktree`
2. `plan`
3. `execute`
4. `reconcile-commits`
5. `validate`
6. `open-pr`

### 说明

- `prepare-worktree`
  - 创建/恢复独立 worktree
  - 准备 branch
- `plan`
  - 由 prompt/spec 生成内部计划
  - 计划只进 checkpoint，不进 DB 表
- `execute`
  - 调用 agent 修改代码
- `reconcile-commits`
  - 参考 fixer 的 post-repair 模式移植
  - 统一处理 commit 状态
- `validate`
  - 执行 lint/test/build 等
- `open-pr`
  - push 并创建 PR

这比当前 task/checklist 驱动模型更符合当前产品主线。

---

## 6. 需要删除的内容

### 6.1 Schema / Store

- `tasks`
- `task_items`
- `queue.cancelByTask`
- `QueueItemRecord.taskId`
- `AgentExecutionRecord.taskId`
- `WorktreeRecord.taskId`
- worker 旧的 task/checklist requeue 逻辑

### 6.2 Domain

- `Task`
- `TaskItem`
- `TASK_STATUSES`
- `TASK_ITEM_STATUSES`
- `TASK_ITEM_SOURCES`
- `TaskLoopTarget`
- 所有 task helper / assert / factory
- `task` 从 `LOOP_TARGET_TYPES` 中移除
- `AUDIT_EVENT_TYPES` 中的 `task.checklist.updated`
- `AUDIT_ENTITY_TYPES` 中的 `task` / `task_item`

并且应顺手修复：

- `FIXER_STEPS` 与真实 fixer 实现已不一致的问题

### 6.3 API / CLI

- `/api/v1/tasks*`
- `looper task ...`
- `loop start --task`

### 6.4 PR 响应残余

- PR list/status payload 中的 `task`
- 任何通过 `repo + prNumber` 反查 task 的展示逻辑

---

## 7. 需要保留并改造的内容

### 7.1 保留

- `worker` loop type
- `worker` runner 文件
- runtime 对 worker 的 wiring
- `loops.repo + loops.prNumber` 作为 worker PR 关联记录

### 7.2 改造

- worker 输入模型
- worker checkpoint 结构
- worker step sequence
- worker open PR 逻辑
- worker 的 worktree / commit / validation 流程
- worker API / CLI 入口

### 7.3 对齐 fixer

worker 应尽量复用 fixer 已经比较成熟的模式：

- 独立 worktree
- commit reconcile
- validate
- push / create PR

---

## 8. 文件级改动方向

## 8.1 删除 task surface

- `apps/cli/src/index.ts`
- `apps/cli/src/index.test.ts`
- `apps/looperd/src/server/index.ts`
- `apps/looperd/src/server/index.test.ts`
- `README.md`

补充：

- 需要新增 `/api/v1/workers`
- 需要新增 `looper work`

## 8.2 删除 task schema / types

- `apps/looperd/src/storage/store.ts`
- `apps/looperd/src/storage/types.ts`
- `apps/looperd/src/storage/sqlite/sqlite-store.ts`
- `apps/looperd/src/storage/sqlite/sqlite-store.test.ts`
- `apps/looperd/src/storage/sqlite/migrations/*`

补充：

- `loops.target_type` 的最终约束需重新定义；当前 SQL 中还包含 `repository` / `manual`
- migration 采用前向迁移方案：保留新的 baseline migration，同时增加迁移脚本把旧库里的 worker/task 结构收敛到 project-target worker 结构

## 8.3 删除 taskId 残余

- `apps/looperd/src/reviewer/index.ts`
- `apps/looperd/src/fixer/index.ts`
- `apps/looperd/src/infra/agent.ts`
- `apps/looperd/src/infra/git.ts`
- `apps/looperd/src/infra/git.test.ts`
- `apps/looperd/src/projects/index.ts`
- `apps/looperd/src/scheduler/index.ts`

## 8.4 重构 worker

- `apps/looperd/src/worker/index.ts`
- `apps/looperd/src/worker/index.test.ts`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/runtime/index.test.ts`
- `apps/looperd/src/domain/index.ts`

---

## 9. 推荐实施顺序（单 PR）

1. 删除 task CLI / API / PR payload task 字段
2. 删除 task schema / store / types
3. 删除所有 `taskId` 残余字段与透传
4. 从 domain 中删除 `task` target model
5. 重构 worker 输入与 step sequence
6. 更新 runtime / scheduler / tests / docs
7. 收敛最终 schema

在第 5 步开始前，必须先定清：

- worker 的 `targetType`（已定：`project`）
- worker 的 API / CLI 入口（已定：`POST /api/v1/workers` + `looper work`）
- worker queue item / lockKey / dedupeKey 形态（已定）

---

## 10. 验收标准

完成后应满足：

- 仓库中不再存在 `tasks` / `task_items`
- 服务端不再暴露 `/api/v1/tasks*`
- CLI 不再暴露 `looper task ...`
- `taskId` 不再出现在 queue / agent execution / worktree 结构中
- `LOOP_TARGET_TYPES` 不再包含 `task`
- `AUDIT_EVENT_TYPES` / `AUDIT_ENTITY_TYPES` 不再包含 task 残余
- `worker` 仍存在，但不依赖 task
- worker 的输入通过 `loops.metadataJson` / `queue.payloadJson` 进入系统
- worker 创建出的 PR 关联持久化在 `loops.repo + loops.prNumber`
- worker 能从输入生成 PR
- reviewer / fixer 主流程继续可运行
- `bun run lint && bun run typecheck && bun run test && bun run build` 全部通过
