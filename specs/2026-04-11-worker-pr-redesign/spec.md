# Task 子系统现状与移除评估

## 1. 结论摘要

当前仓库里的 `task` 子系统（`tasks` / `task_items` / task API / task CLI / worker loop）是**完整实现但未被当前主流程使用**的能力。

更准确地说：

- **代码层面**：`task` 仍是完整一等模型，不是单纯的类型残留。
- **运行层面**：当前真实流量全部走 **PR 驱动的 reviewer / fixer**，没有任何证据表明本地实例正在使用 task 路径。
- **策略建议**：从产品现状看，`task` 更像“未启用的旧/旁路方案”，值得进入**移除或至少废弃**评估。

本 spec 不直接修改实现，只做现状梳理、证据归纳和移除建议。

---

## 2. 本次评估范围

本次评估同时基于两类证据：

1. **代码静态结构**：schema / store / API / CLI / runtime / worker / tests
2. **本地真实运行数据**：`~/.looper/looper.sqlite`

核心问题是：

> 这个项目现在是不是实际上已经不再需要 `task` 数据表以及相关逻辑？

---

## 3. 实际数据库现状

数据库：`~/.looper/looper.sqlite`

本地检查结果：

- `tasks = 0`
- `task_items = 0`
- `loops = 2`
- `runs = 104`
- `queue_items = 81`
- `agent_executions = 17`
- `pull_request_snapshots = 2`

进一步分组后发现：

- `loops` 只有：
  - `fixer / pull_request`
  - `reviewer / pull_request`
- `queue_items` 只有：
  - `fixer / pull_request`
  - `reviewer / pull_request`
- `queue_items.task_id` 实际为 **0 条非空**
- `agent_executions.task_id` 实际为 **0 条非空**

这说明：

### 3.1 当前真实运行流量是 PR 驱动的

当前系统在真实运行中，调度和执行的对象是：

- pull request reviewer loop
- pull request fixer loop
- 对应的 queue item
- 对应的 run
- 对应的 agent execution

### 3.2 当前真实运行流量没有进入 task 路径

如果 task 路径被真正使用，至少应该出现以下任一迹象：

- `tasks` 非空
- `task_items` 非空
- `loops.type = worker` 且 `target_type = task`
- `queue_items.task_id` 非空
- `agent_executions.task_id` 非空

本地数据库里这些证据全部缺失。

因此可以确认：

> **当前实例的真实主流程没有使用 task 子系统。**

---

## 4. 代码结构现状

虽然数据库没有 task 数据，但代码里 task 子系统仍然是完整的。

### 4.1 Schema / Migration

关键文件：

- `apps/looperd/src/storage/sqlite/migrations/0001_init.sql`
- `apps/looperd/src/storage/sqlite/migrations/0002_integrations.sql`
- `apps/looperd/src/storage/sqlite/migrations/0003_scheduler_queue.sql`

现状：

- `0001_init.sql` 创建了 `tasks` 与 `task_items`
- `loops.target_type` 仍允许 `'task'`
- 后续 migration 继续在这些表周边增加关联字段：
  - `agent_executions.task_id`
  - `worktrees.task_id`
  - `queue_items.task_id`

这表明 task 不是零碎残留，而是原始设计的一部分。

本次实施方式：

- 保持 `0001_init.sql` / `0002_integrations.sql` / `0003_scheduler_queue.sql` 反映新的无-task 基线结构。
- 额外提供前向迁移，把已存在数据库从 task-target / task-backed worker 模型迁到新的 project-target worker 模型。
- 迁移期间允许清理旧的 `tasks` / `task_items` 表，并移除 `queue_items.task_id` / `agent_executions.task_id` / `worktrees.task_id` 等过渡字段。
- 对无法自动继续执行的旧 worker loop / queue item，迁移后应落到安全状态（如 paused / cancelled），避免运行时继续按旧模型执行。

### 4.2 Store / Storage Types

关键文件：

- `apps/looperd/src/storage/store.ts`
- `apps/looperd/src/storage/types.ts`
- `apps/looperd/src/storage/sqlite/sqlite-store.ts`

现状：

- `Store` 显式暴露：
  - `tasks`
  - `taskItems`
  - `queue.cancelByTask()`
- 持久化类型里仍包含：
  - `TaskRecord`
  - `TaskItemRecord`
  - 多个记录上的 `taskId`

也就是说，task 仍然是存储层正式支持的领域对象。

### 4.3 Domain

关键文件：

- `apps/looperd/src/domain/index.ts`

现状：

- `LOOP_TARGET_TYPES = ["task", "pull_request"]`
- `LOOP_TYPES = ["reviewer", "worker", "fixer"]`
- `TASK_STATUSES`、`TASK_ITEM_STATUSES`、`TASK_ITEM_SOURCES`
- `Task` / `TaskItem` 类型
- Worker steps 完整围绕 task 设计

这说明：

> 在领域模型里，`task` 不是辅助字段，而是与 `pull_request` 并列的 loop target。

### 4.4 HTTP API

关键文件：

- `apps/looperd/src/server/index.ts`

现状：

- `/api/v1/tasks`
  - `GET` 列表
  - `POST` 创建
- `/api/v1/tasks/:id`
  - `GET`
- `/api/v1/tasks/:id/start`
- `/api/v1/tasks/:id/pause`

此外，PR 相关接口仍会顺手把 task 信息拼进响应：

- PR list item 里的 `task`
- PR status 里的 `task`

但这些对 task 的依赖属于**展示层附带信息**，不是当前主流程的核心。

### 4.5 CLI

关键文件：

- `apps/cli/src/index.ts`

现状：

- `task create`
- `task start`
- `task pause`
- `task status`
- `task show`
- `loop start --task <taskId>`

CLI 仍完整暴露 task 子命令。

### 4.6 Worker / Runtime

关键文件：

- `apps/looperd/src/worker/index.ts`
- `apps/looperd/src/runtime/index.ts`
- `apps/looperd/src/scheduler/index.ts`

现状：

- `worker` loop 设计上就是 **task-driven**
- worker queue item 要求 `loopId + taskId`
- worker 启动时必须读取 task 和 task_items
- worker 运行过程中持续回写：
  - task status
  - task_items status
  - task.prNumber

因此：

> `worker` 不是“顺便支持 task”，而是“完全围绕 task 设计”。

---

## 5. 真正的写入逻辑在哪里

这是本次评估里最关键的澄清点。

### 5.1 有 task 写入逻辑

代码里**确实存在**写入 `tasks` / `task_items` 的逻辑。

主要入口：

#### `POST /api/v1/tasks`

文件：`apps/looperd/src/server/index.ts`

职责：

- 创建 `task`
- 解析 body 里的 `items`
- 创建 `task_items`

这里会发生真正写入：

- `store.tasks.upsert(task)`
- `store.taskItems.upsert(item)`

对应 CLI：

- `looper task create`

#### `POST /api/v1/tasks/:id/start`

文件：`apps/looperd/src/server/index.ts`

职责：

- 读取 task
- 创建或恢复 loop
- 回写 task 状态
- enqueue 一个 `worker + task` queue item

#### `apps/looperd/src/worker/index.ts`

职责：

- 执行 `prepare-task`
- 读取 checklist
- 更新 `task_items`
- 回写 `task.status`
- 回写 `task.prNumber`

### 5.2 但没有隐式创建 task 的主流程

这也是当前数据库为空的根本原因。

关键事实：

- `/api/v1/loops` 可以创建 loop
- 但它**不会自动创建 task**
- 只有显式命中 `/api/v1/tasks` 或 `looper task ...`，task 表才会有数据

也就是说：

> `task` 不是当前系统自动流转出来的实体，而是一个需要显式使用 task API/CLI 才会进入的独立分支。

### 5.3 当前 reviewer / fixer 主流程不会写 task

虽然 reviewer/fixer 代码里带有 `queueItem.taskId` 字段透传或日志打印，
但在当前 PR 驱动主流程里：

- 它们不会创建 `tasks`
- 不会创建 `task_items`
- 不会给 queue item 写入 `task_id`
- 不会给 agent execution 写入 `task_id`

所以从真实行为看：

> 当前产品的“日常自动化”已经与 task 子系统解耦。

---

## 6. 为什么会出现“代码里有、数据库里空”的现象

原因不是实现坏掉，而是**产品路径切换了**。

更合理的解释是：

1. 项目最初设计里，存在三条主线：
   - Reviewer Loop
   - Worker Loop
   - Fixer Loop
2. 其中 Worker Loop 是围绕 `Task + Checklist + Spec` 做开发推进
3. 但当前真实可用、持续运行的主路径已经变成：
   - PR reviewer
   - PR fixer
4. task/worker 这条链路虽然还保留在代码里，但没有被当前实例使用

所以现状不是：

- “task 逻辑完全不存在”

而是：

- “task 逻辑完整存在，但当前产品主路径不再依赖它”

---

## 7. 支持移除 task 的最强证据

### 7.1 真实数据库中完全没有 task 使用痕迹

这是最强证据。

不是“用得少”，而是：

- `tasks = 0`
- `task_items = 0`
- 所有近期 queue item / loop / agent execution 都不带 task

### 7.2 当前主产品价值已经集中在 PR 驱动流

当前用户可见、真实运转的能力是：

- reviewer 认领并 review PR
- fixer 修复 PR 问题

这些流程都围绕 `pull_request` 展开，而不是围绕 `task` 展开。

### 7.3 task 是显式旁路，不是隐式底层依赖

只有显式走：

- `looper task create`
- `looper task start`
- `/api/v1/tasks`

才会产生 task 数据。

这意味着 task 更像一套**未启用的产品分支**，而不是必须保留的底层骨架。

### 7.4 PR 相关 task 依赖 mostly 是附带展示

PR list/status 中对 task 的引用主要是：

- 根据 `repo + prNumber` 找到匹配的 task
- 顺手展示 `{ id, title, status }`

在数据库无 task 的情况下，这些字段始终是 `null`，说明并非关键主路径。

---

## 8. 支持保留 task 的最强证据

### 8.1 task 子系统并不是半成品

它不是一两个未完成文件，而是完整贯通了：

- schema
- store
- domain
- API
- CLI
- worker
- runtime
- tests

尤其 `apps/looperd/src/worker/index.ts` 是一套完整 task 执行引擎。

### 8.2 如果未来仍想保留“主动开发”能力，worker 这个角色仍然有价值

从设计意图上看，当前 task 子系统承载的是：

- spec path
- checklist item
- worktree
- 自动开发
- 自动开 PR

但真正有产品价值的未必是 `task` 这个实体，而更可能是 **worker 这个“从用户意图创建 PR” 的角色**。

### 8.3 删除 task 不等于必须删除 worker

如果保持现有实现不重构，删除 task 的实质确实会连带删除 worker。

但在当前仍处于开发阶段、允许重新设计的前提下，更合理的方向是：

- 删除 `task` / `task_items`
- 删除 task API / CLI
- 删除 task 相关 schema / tests / event / types
- **保留 worker 角色，但改造其 target model**

这意味着：

> 应删除的是 `task` 这个中间实体，而不是 “worker 负责从用户意图产出代码和 PR” 这个产品能力。

---

## 9. 综合判断

综合代码与运行现状：

### 判断一：task 不是当前主流程的必需能力

当前真实运行完全不依赖它。

### 判断二：task 也不是纯死代码

它有完整实现和明确边界，只是没有被当前主路径使用。

### 判断三：task 更像“已被边缘化的旧中间模型”

因此它最准确的定位是：

> **latent / dormant subsystem（沉睡中的完整子系统）**

不是底层核心，也不是零碎垃圾代码。

---

## 10. 建议决策

### 推荐：删除 `task` / `task_items`，保留并重构 `worker`

如果团队没有明确计划继续推进 task-driven worker 模式，推荐策略是：

1. 删除 `task` / `task_items` 及相关 API / CLI / schema / 类型
2. 保留 `worker` loop type，但让它不再依赖 `task`
3. 将 worker 重构为 **PR-oriented worker**：从用户输入出发，最终创建/推进一个 PR
4. 将 schema 一次性收敛到最终干净形态

原因：

- 当前项目仍处于开发阶段
- 当前真实 DB 没有 task 数据
- 不需要为历史迁移或兼容性保留包袱
- reviewer / fixer 已经证明 PR 是当前产品的核心实体
- worker 的真正价值是“创建 PR”，不是“维护 task/checklist 表”

### 何时不应删除

只有一种情况应暂停删除：

> 团队仍明确打算保留 `task` 作为独立持久化实体，并继续以 checklist 表来驱动 worker。

如果是这样，则 task 应保留；否则更应尽快把 worker 改造成不依赖 task 的模型。

---

## 11. 建议的重构阶段

### Phase 1：先确认产品方向

确认问题：

- 是否还需要 `looper task ...` 这套用户入口？
- 是否保留 `worker` 作为主动开发角色？
- worker 后续是否统一围绕 PR 工作？

### Phase 2：删除 task 行为层，但保留 worker

优先删除：

- server 中 `/api/v1/tasks`
- CLI 中 `task` 子命令
- `task` 驱动的 worker 入口

### Phase 3：删除 task 领域与存储模型

删除：

- `Task` / `TaskItem` 领域类型
- `task` target type
- `Store.tasks` / `Store.taskItems`
- `cancelByTask`

### Phase 4：把 worker 重构为 PR-oriented worker

目标方向：

- `worker` 保留
- `worker.targetType = project`
- `worker` 不再读取 `tasks` / `task_items`
- `worker` 的计划/分解状态存到 checkpoint 或 payload
- `worker` 的产物是 PR，并与 reviewer/fixer 共用 PR 主线
- worker 创建出的 PR 关联应持久化到 `loops.repo + loops.prNumber`

### Phase 5：清理 schema

直接清理：

- `tasks`
- `task_items`
- `loops.target_type` 中的 `task`
- `queue_items.task_id`
- `agent_executions.task_id`
- `worktrees.task_id`

由于当前目标是不保留历史包袱，因此这一阶段应以**最终 schema 干净一致**为优先目标，而不是兼容旧结构。

---

## 12. 单一最高风险未知项

最高风险已经不再是方向本身，而是**实现时是否会重新引入一个新的 task-like 中间实体**。

即：

> 在删除 `task` 之后，是否会因为实现便利又引入新的 `work_request` / `job` / `spec record` 持久化表，把复杂度重新带回来？

当前更合理的约束应该是：

- `worker.targetType = project`
- worker 输入进入 `loops.metadataJson` / `queue.payloadJson`
- worker 创建后的 PR 关联写入 `loops.repo + loops.prNumber`
- worker 运行态状态进入 checkpoint

如果偏离这条约束，就有很大概率重新长出新的 task-like 模型。

---

## 13. 最终结论

最终结论可以压缩为一句话：

> **当前 Looper 的真实运行模式已经是 PR-driven reviewer/fixer；因此更合理的方向不是保留 `task`，而是删除 `task/task_items`，并把 `worker` 重构成同样围绕 PR 工作的主动开发角色。**

补充到表级别：

> **如果确认要移除 `task`，那么 `task_items` 也应一并移除。**

原因：

- `task_items` 没有独立产品语义，本质上只是 `task` 的 checklist 明细表
- 当前本地数据库中 `task_items = 0`
- `task_items` 的读写完全依附于 task/worker 流程，不被 reviewer/fixer 主流程直接使用
- 保留 `task_items` 而删除 `task` 没有实际意义，只会留下无主表和残余类型

对于后续工程动作，推荐按以下优先级推进：

1. 先确认产品方向
2. 若确认不再需要 task 这个中间实体，则直接在**一个收敛 PR** 中完成 task/task_items 删除，并同步把 worker 重构为 PR-oriented worker
3. worker 新模型应明确采用：`project` 作为 target、`/api/v1/workers` 作为入口、`looper work` 作为 CLI、`loops.repo/prNumber` 作为 PR 关联主记录
