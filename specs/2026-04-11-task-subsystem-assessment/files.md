# Task 删除 + Worker 重构影响面清单

## 1. Schema / Migration

- `apps/looperd/src/storage/sqlite/migrations/0001_init.sql`
  - `tasks`
  - `task_items`
  - `loops.target_type` 包含 `task`
  - 同时还包含 `repository` / `manual`，需重新定义最终约束
- `apps/looperd/src/storage/sqlite/migrations/0002_integrations.sql`
  - `agent_executions.task_id`
  - `worktrees.task_id`
- `apps/looperd/src/storage/sqlite/migrations/0003_scheduler_queue.sql`
  - `queue_items.task_id`

## 2. Domain

- `apps/looperd/src/domain/index.ts`
  - `LOOP_TARGET_TYPES`
  - `LOOP_TYPES`（保留 `worker`，删除 `task target`）
  - `TASK_STATUSES`
  - `TASK_ITEM_STATUSES`
  - `TASK_ITEM_SOURCES`
  - `Task`
  - `TaskItem`
  - `WORKER_STEPS`（需重写，不删除 worker 角色）
  - `FIXER_STEPS`（当前已与 fixer 实现不一致，应顺手修复）
  - `AUDIT_EVENT_TYPES` 中的 task 残余
  - `AUDIT_ENTITY_TYPES` 中的 task 残余

## 3. Storage / Types

- `apps/looperd/src/storage/store.ts`
  - `tasks`
  - `taskItems`
  - `cancelByTask`
- `apps/looperd/src/storage/types.ts`
  - `TaskRecord`
  - `TaskItemRecord`
  - `taskId` 关联字段
- `apps/looperd/src/storage/sqlite/sqlite-store.ts`
  - task/task_items CRUD
  - taskId 映射

## 4. Server API

- `apps/looperd/src/server/index.ts`
  - `/api/v1/tasks`
  - `/api/v1/tasks/:id`
  - `/api/v1/tasks/:id/start`
  - `/api/v1/tasks/:id/pause`
  - PR payload 中的 `task` 附带字段

## 5. CLI

- `apps/cli/src/index.ts`
  - `task create`
  - `task start`
  - `task pause`
  - `task status`
  - `task show`
  - `loop start --task`

## 6. Runtime / Worker / Scheduler

- `apps/looperd/src/worker/index.ts`
  - 目前是 task-driven worker 主流程，目标是重构为 PR-oriented worker
- `apps/looperd/src/runtime/index.ts`
  - worker runner wiring（保留）
- `apps/looperd/src/scheduler/index.ts`
  - `cancelByTask`
  - `deriveLockKey(taskId)`

## 7. Related References

- `apps/looperd/src/reviewer/index.ts`
  - 日志里透传 `queueItem.taskId`
- `apps/looperd/src/fixer/index.ts`
  - 日志/执行输入里透传 `queueItem.taskId`
- `apps/looperd/src/projects/index.ts`
  - worktree 记录保留 `taskId`
- `apps/looperd/src/infra/agent.ts`
  - agent execution 持久化时透传 `taskId`
- `apps/looperd/src/infra/git.test.ts`
  - 存在 `taskId` 相关测试值

## 8. Tests / Docs

- `README.md`
  - 文档仍写 `task create|start|pause|status|show`
- `apps/looperd/src/worker/index.test.ts`
- `apps/looperd/src/server/index.test.ts`
- `apps/looperd/src/storage/sqlite/sqlite-store.test.ts`
- `apps/looperd/src/scheduler/index.test.ts`
- `apps/looperd/src/runtime/index.test.ts`
- `apps/cli/src/index.test.ts`
