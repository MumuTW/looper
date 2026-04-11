# Task 删除 + Worker 重构 Checklist

## 决策确认

- [ ] 确认删除 `task` / `task_items`
- [ ] 确认保留 `worker`
- [ ] 确认 worker 改为 PR-oriented worker
- [ ] 确认不再引入新的 task-like 中间持久化实体
- [ ] 定义 worker 的最终 `loop.target_type`
- [ ] 定义新的 worker API / CLI 入口

## 单 PR 交付范围

- [ ] 删除 task CLI
- [ ] 删除 `loop start --task`
- [ ] 删除 `/api/v1/tasks*`
- [ ] 删除 PR payload 中的 `task` 字段
- [ ] 删除 `tasks` / `task_items` schema 与 store
- [ ] 删除所有 `taskId` 字段与透传
- [ ] 从 domain 中删除 `task` target model
- [ ] 清理 `AUDIT_EVENT_TYPES` / `AUDIT_ENTITY_TYPES` 中的 task 残余
- [ ] 修复 `FIXER_STEPS` 与真实实现不一致的问题
- [ ] 保留并重构 `worker`
- [ ] 更新 runtime / scheduler / tests / docs

## Worker 重构

- [ ] worker 不再读取 `tasks`
- [ ] worker 不再读取 `task_items`
- [ ] worker 输入改为 `projectId + repo + baseBranch + prompt/specPath`
- [ ] 定义 worker queue item 结构
- [ ] 定义 worker 的 `lockKey` / `dedupeKey`
- [ ] 计划/分解状态改存 checkpoint / payload
- [ ] step sequence 重构为 PR-oriented 流程
- [ ] 明确 worker 是否 requeue（默认不再沿用 task slice requeue）
- [ ] 评估并简化 `openPrStrategy`
- [ ] worktree / reconcile-commits / validate / open-pr 路径可运行

## Schema 设计

- [ ] 决定最终 `loops.target_type` 允许集合
- [ ] 清理 `repository` / `manual` 等未对齐 domain 的约束含义
- [ ] 明确 schema reset / migration 实施方式

## 最终验收

- [ ] `bun run lint` 通过
- [ ] `bun run typecheck` 通过
- [ ] `bun run test` 通过
- [ ] `bun run build` 通过
- [ ] worker 能创建 PR
- [ ] reviewer / fixer 主流程继续可运行
