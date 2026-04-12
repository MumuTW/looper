import { afterEach, describe, expect, test } from "bun:test";
import { access, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { SqliteStore } from "./sqlite-store";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function createStoreFixture() {
  const rootDir = await mkdtemp(join(tmpdir(), "looper-store-"));
  cleanupPaths.push(rootDir);

  return {
    rootDir,
    dbPath: join(rootDir, "state", "looper.sqlite"),
    backupDir: join(rootDir, "backups"),
  };
}

describe("SqliteStore", () => {
  test("initializes schema, writes records, and reports health", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({
      dbPath: fixture.dbPath,
      backupDir: fixture.backupDir,
    });

    store.initialize({ autoMigrate: true, requireBackup: true });

    const now = "2026-04-11T12:00:00.000Z";

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: '{"tier":"mvp"}',
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_1",
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "idle",
      configJson: '{"priority":"normal"}',
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.runs.upsert({
      id: "run_1",
      loopId: "loop_1",
      status: "running",
      currentStep: "snapshot",
      lastCompletedStep: null,
      checkpointJson: '{"cursor":1}',
      summary: null,
      errorMessage: null,
      startedAt: now,
      lastHeartbeatAt: now,
      endedAt: null,
      createdAt: now,
      updatedAt: now,
    });
    store.pullRequestSnapshots.upsert({
      id: "snapshot_1",
      projectId: "project_1",
      repo: "acme/looper",
      prNumber: 42,
      headSha: "abc123",
      baseSha: "base123",
      title: "Persistence",
      body: "This adds storage",
      author: "octocat",
      diffRef: "refs/pull/42/head",
      checksSummary: "all-green",
      unresolvedThreadCount: 2,
      reviewState: "changes_requested",
      payloadJson: '{"title":"Persistence"}',
      capturedAt: now,
      createdAt: now,
    });
    store.events.append({
      id: "event_1",
      eventType: "loop.created",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      entityType: "loop",
      entityId: "loop_1",
      correlationId: "corr_1",
      causationId: "cause_1",
      actorType: "agent",
      actorId: "reviewer_1",
      actorDisplayName: "Reviewer",
      payloadJson: '{"status":"idle"}',
      createdAt: now,
    });
    store.queue.upsert({
      id: "queue_reviewer_1",
      projectId: "project_1",
      loopId: "loop_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
      priority: 1,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: '{"source":"discover"}',
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });
    store.queue.upsert({
      id: "queue_fixer_1",
      projectId: "project_1",
      loopId: null,
      type: "fixer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "fixer:acme/looper:42",
      priority: 2,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "pr:acme/looper:42",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(
      store.locks.acquire({
        key: "pr:acme/looper:42",
        owner: "reviewer-loop",
        reason: "claim",
        expiresAt: "2026-04-11T12:05:00.000Z",
        createdAt: now,
        updatedAt: now,
      }),
    ).toBe(true);
    store.agentExecutions.upsert({
      id: "agent_exec_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      vendor: "opencode",
      status: "running",
      pid: 12345,
      commandJson: '{"command":"opencode","args":["run"]}',
      cwd: "/tmp/looper",
      summary: null,
      parseStatus: null,
      completionSignal: null,
      heartbeatCount: 3,
      lastHeartbeatAt: now,
      outputJson: '{"stdout":"ok","stderr":""}',
      errorMessage: null,
      startedAt: now,
      endedAt: null,
      metadataJson: '{"idempotencyKey":"abc"}',
      createdAt: now,
      updatedAt: now,
    });
    store.notifications.upsert({
      id: "notification_1",
      projectId: "project_1",
      loopId: "loop_1",
      runId: "run_1",
      entityType: "loop",
      entityId: "loop_1",
      channel: "in_app",
      level: "info",
      title: "Loop updated",
      subtitle: "loop_1",
      body: "Worker progressed",
      status: "success",
      dedupeKey: "loop.updated:loop:loop_1",
      errorMessage: null,
      payloadJson: '{"title":"Loop updated"}',
      sentAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.worktrees.upsert({
      id: "worktree_1",
      projectId: "project_1",
      repoPath: "/tmp/looper",
      worktreePath: "/tmp/looper-worktrees/feature-loop-1",
      branch: "feature/loop-1",
      baseBranch: "main",
      status: "active",
      headSha: "abc123",
      metadataJson: '{"recovered":false}',
      createdAt: now,
      updatedAt: now,
      cleanedAt: null,
    });

    expect(store.projects.getById("project_1")).toEqual({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: '{"tier":"mvp"}',
      createdAt: now,
      updatedAt: now,
    });
    expect(store.loops.getById("loop_1")?.repo).toBe("acme/looper");
    expect(store.loops.getById("loop_1")?.projectId).toBe("project_1");
    expect(store.runs.listByLoop("loop_1")).toHaveLength(1);
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.headSha,
    ).toBe("abc123");
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.projectId,
    ).toBe("project_1");
    expect(
      store.pullRequestSnapshots.getLatest("acme/looper", 42)?.baseSha,
    ).toBe("base123");
    expect(store.events.listByEntity("loop", "loop_1")).toHaveLength(1);
    expect(store.events.listByEntity("loop", "loop_1")[0]?.actorId).toBe(
      "reviewer_1",
    );
    expect(store.locks.get("pr:acme/looper:42")?.owner).toBe("reviewer-loop");
    expect(store.queue.findActiveByDedupe("reviewer:acme/looper:42")?.id).toBe(
      "queue_reviewer_1",
    );
    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_reviewer_1",
    ]);
    expect(store.queue.claimNext(now, "executor_1")?.id).toBe(
      "queue_reviewer_1",
    );
    expect(store.queue.getById("queue_reviewer_1")?.status).toBe("running");
    store.queue.complete("queue_reviewer_1", now);
    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_fixer_1",
    ]);
    expect(store.agentExecutions.listActive()).toHaveLength(1);
    expect(store.agentExecutions.getById("agent_exec_1")?.pid).toBe(12345);
    expect(store.notifications.list(1)[0]?.channel).toBe("in_app");
    expect(
      store.notifications.getLatestByDedupe(
        "in_app",
        "loop.updated:loop:loop_1",
      )?.status,
    ).toBe("success");
    expect(
      store.worktrees.getByBranch("project_1", "feature/loop-1")?.status,
    ).toBe("active");

    const health = store.schema.healthcheck();
    expect(health.ok).toBe(true);
    expect(health.migration.latestAppliedId).toBe("0003_scheduler_queue");
    expect(health.lastUpdatedAt).toBeString();

    const backupPath = store.schema.backup();
    await access(backupPath);

    store.close();
  });

  test("rolls back transactional writes on failure", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(() =>
      store.withTransaction((tx) => {
        tx.loops.upsert({
          id: "loop_rollback",
          projectId: "project_1",
          type: "worker",
          targetType: "project",
          targetId: "project_1",
          repo: null,
          prNumber: null,
          status: "queued",
          configJson: null,
          metadataJson: null,
          lastRunAt: null,
          nextRunAt: null,
          createdAt: now,
          updatedAt: now,
        });
        tx.events.append({
          id: "event_loop_rollback",
          eventType: "loop.created",
          projectId: "project_1",
          entityType: "loop",
          entityId: "loop_rollback",
          payloadJson: "{}",
          createdAt: now,
        });

        throw new Error("abort transaction");
      }),
    ).toThrow("abort transaction");

    expect(store.loops.getById("loop_rollback")).toBeNull();
    expect(store.events.listByEntity("loop", "loop_rollback")).toHaveLength(0);

    store.close();
  });

  test("re-acquires an expired lock using the injected clock", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({
      dbPath: fixture.dbPath,
      now: () => new Date("2026-04-11T12:10:00.000Z"),
    });
    store.initialize({ autoMigrate: true });

    expect(
      store.locks.acquire({
        key: "task:123",
        owner: "worker-a",
        reason: "initial",
        expiresAt: "2026-04-11T12:00:00.000Z",
        createdAt: "2026-04-11T11:50:00.000Z",
        updatedAt: "2026-04-11T11:50:00.000Z",
      }),
    ).toBe(true);

    expect(
      store.locks.acquire({
        key: "task:123",
        owner: "worker-b",
        reason: "takeover",
        expiresAt: "2026-04-11T12:20:00.000Z",
        createdAt: "2026-04-11T12:10:00.000Z",
        updatedAt: "2026-04-11T12:10:00.000Z",
      }),
    ).toBe(true);
    expect(store.locks.get("task:123")?.owner).toBe("worker-b");

    store.close();
  });

  test("filters scheduled items when loops are paused and supports retry markers", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    const now = "2026-04-11T12:00:00.000Z";
    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: now,
      updatedAt: now,
    });
    store.loops.upsert({
      id: "loop_paused",
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "paused",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: now,
      createdAt: now,
      updatedAt: now,
    });
    store.queue.upsert({
      id: "queue_worker_1",
      projectId: "project_1",
      loopId: "loop_paused",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      dedupeKey: "worker:loop_paused",
      priority: 3,
      status: "queued",
      availableAt: now,
      attempts: 0,
      maxAttempts: 3,
      claimedBy: null,
      claimedAt: null,
      startedAt: null,
      finishedAt: null,
      lockKey: "worker:loop_paused",
      payloadJson: null,
      lastError: null,
      lastErrorKind: null,
      createdAt: now,
      updatedAt: now,
    });

    expect(store.queue.listScheduled(now)).toHaveLength(0);

    const pausedLoop = store.loops.getById("loop_paused");
    if (!pausedLoop) {
      throw new Error("expected paused loop to exist");
    }

    store.loops.upsert({
      ...pausedLoop,
      status: "queued",
      updatedAt: "2026-04-11T12:00:01.000Z",
    });

    expect(store.queue.listScheduled(now).map((item) => item.id)).toEqual([
      "queue_worker_1",
    ]);

    store.queue.markRetry({
      id: "queue_worker_1",
      availableAt: "2026-04-11T12:00:30.000Z",
      attempts: 1,
      errorKind: "retryable_after_resume",
      errorMessage: "resume later",
      updatedAt: "2026-04-11T12:00:05.000Z",
    });
    expect(store.queue.getById("queue_worker_1")).toMatchObject({
      status: "queued",
      attempts: 1,
      availableAt: "2026-04-11T12:00:30.000Z",
      lastErrorKind: "retryable_after_resume",
    });

    store.close();
  });

  test("lists runs by status in stable newest-first order", async () => {
    const fixture = await createStoreFixture();
    const store = new SqliteStore({ dbPath: fixture.dbPath });
    store.initialize({ autoMigrate: true });

    store.projects.upsert({
      id: "project_1",
      name: "Looper",
      repoPath: "/tmp/looper",
      baseBranch: "main",
      archived: false,
      metadataJson: null,
      createdAt: "2026-04-11T11:59:00.000Z",
      updatedAt: "2026-04-11T11:59:00.000Z",
    });
    store.loops.upsert({
      id: "loop_1",
      projectId: "project_1",
      type: "worker",
      targetType: "project",
      targetId: "project_1",
      repo: null,
      prNumber: null,
      status: "running",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: "2026-04-11T12:00:00.000Z",
      createdAt: "2026-04-11T11:59:30.000Z",
      updatedAt: "2026-04-11T11:59:30.000Z",
    });

    store.runs.upsert({
      id: "run_old_running",
      loopId: "loop_1",
      status: "running",
      currentStep: "step_old",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:00:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:00:00.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
    });
    store.runs.upsert({
      id: "run_new_running",
      loopId: "loop_1",
      status: "running",
      currentStep: "step_new",
      lastCompletedStep: null,
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:05:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:05:00.000Z",
      endedAt: null,
      createdAt: "2026-04-11T12:05:00.000Z",
      updatedAt: "2026-04-11T12:05:00.000Z",
    });
    store.runs.upsert({
      id: "run_done",
      loopId: "loop_1",
      status: "completed",
      currentStep: "done",
      lastCompletedStep: "done",
      checkpointJson: null,
      summary: null,
      errorMessage: null,
      startedAt: "2026-04-11T12:10:00.000Z",
      lastHeartbeatAt: "2026-04-11T12:10:00.000Z",
      endedAt: "2026-04-11T12:12:00.000Z",
      createdAt: "2026-04-11T12:10:00.000Z",
      updatedAt: "2026-04-11T12:12:00.000Z",
    });

    const running = store.runs.listByStatus("running");
    expect(running.map((run) => run.id)).toEqual([
      "run_new_running",
      "run_old_running",
    ]);
    expect(running.every((run) => run.status === "running")).toBe(true);

    store.close();
  });
});
