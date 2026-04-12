# Task 删除 + Worker 重构 Checklist

## 决策确认

- [x] 确认删除 `task` / `task_items`
- [x] 确认保留 `worker`
- [x] 确认 worker 改为 PR-oriented worker
- [x] 确认不再引入新的 task-like 中间持久化实体
- [x] 定义 worker 的最终 `loop.target_type` 为 `project`
- [x] 定义新的 worker API / CLI 入口为 `POST /api/v1/workers` + `looper work`

## 单 PR 交付范围

- [x] 删除 task CLI
- [x] 删除 `loop start --task`
- [x] 删除 `/api/v1/tasks*`
- [x] 删除 PR payload 中的 `task` 字段
- [x] 删除 `tasks` / `task_items` schema 与 store
- [x] 删除所有 `taskId` 字段与透传
- [x] 从 domain 中删除 `task` target model
- [x] 清理 `AUDIT_EVENT_TYPES` / `AUDIT_ENTITY_TYPES` 中的 task 残余
- [x] 修复 `FIXER_STEPS` 与真实实现不一致的问题
- [x] 保留并重构 `worker`
- [x] 更新 runtime / scheduler / tests / docs

## Worker 重构

- [x] worker 不再读取 `tasks`
- [x] worker 不再读取 `task_items`
- [x] worker 输入改为 `projectId + repo + baseBranch + prompt/specPath`
- [x] 定义 worker queue item 结构
- [x] 定义 worker 的 `lockKey` / `dedupeKey`
- [x] 计划/分解状态改存 checkpoint / payload
- [x] step sequence 重构为 PR-oriented 流程
- [x] 明确 worker 是否 requeue（默认不再沿用 task slice requeue）
- [x] 评估并简化 `openPrStrategy`
- [x] 明确 worker 创建出的 PR 关联持久化在 `loops.repo + loops.prNumber`
- [x] worktree / reconcile-commits / validate / open-pr 路径可运行

## Schema 设计

- [x] 决定最终 `loops.target_type` 允许集合
- [x] 清理 `repository` / `manual` 等未对齐 domain 的约束含义
- [x] 明确 schema reset / migration 实施方式

## 最终验收

- [x] `bun run lint` 通过
- [x] `bun run typecheck` 通过
- [x] `bun run test` 通过
- [x] `bun run build` 通过
- [x] worker 能创建 PR
- [x] reviewer / fixer 主流程继续可运行
