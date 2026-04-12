import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { Logger } from "../bootstrap/logger";
import type { AgentResult, AgentRunInput } from "../infra/agent";
import { SchedulerQueue } from "../scheduler/index";
import { SqliteStore } from "../storage/sqlite/sqlite-store";
import type { WorktreeRecord } from "../storage/types";
import {
  type WorkerAgentExecution,
  type WorkerAgentExecutor,
  type WorkerGitGateway,
  type WorkerGitHubGateway,
  WorkerLoopRunner,
  type WorkerValidationResult,
} from "./index";

const cleanupPaths: string[] = [];

afterEach(async () => {
  while (cleanupPaths.length > 0) {
    const path = cleanupPaths.pop();
    if (path) {
      await rm(path, { recursive: true, force: true });
    }
  }
});

async function createFixture() {
  const rootDir = await mkdtemp(join(tmpdir(), "looper-worker-"));
  cleanupPaths.push(rootDir);
  const repoPath = join(rootDir, "repo");
  const worktreeRoot = join(rootDir, "worktrees");
  await mkdir(repoPath, { recursive: true });
  await mkdir(worktreeRoot, { recursive: true });
  await writeFile(
    join(repoPath, "spec.md"),
    "# Spec\n\nImplement worker loop MVP.\n",
  );

  const store = new SqliteStore({
    dbPath: join(rootDir, "state", "looper.sqlite"),
  });
  store.initialize({ autoMigrate: true });

  const now = new Date("2026-04-11T12:00:00.000Z");
  const nowIso = now.toISOString();

  store.projects.upsert({
    id: "project_1",
    name: "Looper",
    repoPath,
    baseBranch: "main",
    archived: false,
    metadataJson: JSON.stringify({ worktreeRoot }),
    createdAt: nowIso,
    updatedAt: nowIso,
  });
  store.loops.upsert({
    id: "loop_worker_1",
    projectId: "project_1",
    type: "worker",
    targetType: "project",
    targetId: "project_1",
    repo: "acme/looper",
    prNumber: null,
    status: "queued",
    configJson: null,
    metadataJson: null,
    lastRunAt: null,
    nextRunAt: nowIso,
    createdAt: nowIso,
    updatedAt: nowIso,
  });
  const queue = new SchedulerQueue({
    store,
    retryMaxAttempts: 3,
    retryBaseDelayMs: 0,
    now: () => now,
  });
  queue.enqueue({
    projectId: "project_1",
    loopId: "loop_worker_1",
    type: "worker",
    targetType: "project",
    targetId: "project_1",
    repo: "acme/looper",
    dedupeKey: "worker:loop_worker_1",
    payloadJson: JSON.stringify({
      title: "Implement worker loop",
      specPath: "spec.md",
      repo: "acme/looper",
      baseBranch: "main",
    }),
  });

  return { rootDir, repoPath, worktreeRoot, store, queue, now };
}

class FakeGitGateway implements WorkerGitGateway {
  public createWorktreeCalls = 0;
  public pushCalls = 0;

  constructor(private readonly worktreeRoot: string) {}

  public async createWorktree(input: {
    projectId: string;
    repoPath: string;
    worktreeRoot: string;
    branch: string;
    baseBranch: string;
    protectedBranches?: string[];
  }): Promise<WorktreeRecord> {
    this.createWorktreeCalls += 1;
    const worktreePath = join(
      this.worktreeRoot,
      input.branch.replace(/[^a-zA-Z0-9._-]+/g, "-"),
    );
    await mkdir(worktreePath, { recursive: true });
    return {
      id: "worktree_1",
      projectId: input.projectId,
      repoPath: input.repoPath,
      worktreePath,
      branch: input.branch,
      baseBranch: input.baseBranch,
      status: "active",
      headSha: "abc123",
      metadataJson: null,
      createdAt: "2026-04-11T12:00:00.000Z",
      updatedAt: "2026-04-11T12:00:00.000Z",
      cleanedAt: null,
    };
  }

  public async push(): Promise<void> {
    this.pushCalls += 1;
  }
}

class FakeGitHubGateway implements WorkerGitHubGateway {
  public createPullRequestCalls: Array<{
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }> = [];

  public async createPullRequest(input: {
    repo: string;
    headBranch: string;
    baseBranch: string;
    title: string;
    body?: string;
    cwd?: string;
  }): Promise<{ number?: number; url: string }> {
    this.createPullRequestCalls.push(input);
    return {
      number: 101,
      url: "https://example.test/acme/looper/pull/101",
    };
  }
}

class FakeAgentExecutor implements WorkerAgentExecutor {
  public starts: AgentRunInput[] = [];

  constructor(private readonly results: AgentResult[]) {}

  public async start(input: AgentRunInput): Promise<WorkerAgentExecution> {
    this.starts.push(input);
    const result = this.results.shift();
    if (!result) {
      throw new Error("No agent result queued");
    }

    return {
      wait: async () => result,
    };
  }
}

function completedAgentResult(
  summary: string,
  commits: string[] = [],
): AgentResult {
  return {
    status: "completed",
    summary,
    artifacts: [],
    changedFiles: ["apps/looperd/src/worker/index.ts"],
    commits,
    rawLogs: { stdout: `${summary}\n`, stderr: "" },
    parseStatus: "parsed",
    completionSignal: "done",
    heartbeatCount: 1,
    resourceUsage: {
      wallTimeMs: 10,
      outputBytes: summary.length,
    },
    pid: 1234,
  };
}

function createCapturingLogger() {
  const entries: Array<{
    level: "info" | "error";
    message: string;
    context?: Record<string, unknown>;
  }> = [];
  const logger: Logger = {
    debug: () => {},
    warn: () => {},
    info: (message, context) =>
      entries.push({ level: "info", message, context }),
    error: (message, context) =>
      entries.push({ level: "error", message, context }),
  };

  return { logger, entries };
}

describe("WorkerLoopRunner", () => {
  test("processNext does not claim queue items for other loop types", async () => {
    const fixture = await createFixture();
    const nowIso = fixture.now.toISOString();
    fixture.store.loops.upsert({
      id: "loop_reviewer_1",
      projectId: "project_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      status: "queued",
      configJson: null,
      metadataJson: null,
      lastRunAt: null,
      nextRunAt: nowIso,
      createdAt: nowIso,
      updatedAt: nowIso,
    });
    fixture.queue.enqueue({
      projectId: "project_1",
      loopId: "loop_reviewer_1",
      type: "reviewer",
      targetType: "pull_request",
      targetId: "pr:acme/looper:42",
      repo: "acme/looper",
      prNumber: 42,
      dedupeKey: "reviewer:acme/looper:42",
    });

    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git: new FakeGitGateway(fixture.worktreeRoot),
      github: new FakeGitHubGateway(),
      agentExecutor: new FakeAgentExecutor([
        completedAgentResult("Implemented slice", ["abc123"]),
      ]),
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const result = await runner.processNext("worker-1");

    expect(result).not.toBeNull();
    expect(
      fixture.store.queue.findActiveByDedupe("reviewer:acme/looper:42")?.status,
    ).toBe("queued");

    fixture.store.close();
  });

  test("opens a pull request after a successful worker run", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const logs = createCapturingLogger();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(101);
    expect(agent.starts).toHaveLength(1);
    expect(git.createWorktreeCalls).toBe(1);
    expect(git.pushCalls).toBe(1);
    expect(github.createPullRequestCalls).toHaveLength(1);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe(
      "completed",
    );
    fixture.store.close();
  });

  test("emits agent-start callback when worker executor starts", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const notifications: Array<{
      executionId: string;
      projectId: string;
      loopId: string;
      runId: string;
      subtitle: string;
      body: string;
      dedupeKey: string;
    }> = [];
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      onAgentExecutionStarted: (input) => {
        notifications.push(input);
      },
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    await runner.processClaimedItem(claimed);

    expect(notifications).toHaveLength(1);
    expect(notifications[0]?.subtitle).toBe("Implement worker loop");
    expect(notifications[0]?.body).toBe("Worker started");
    expect(notifications[0]?.dedupeKey).toMatch(
      /^runtime\.agent\.started:worker:/,
    );

    fixture.store.close();
  });

  test("keeps loop queued when validation fails and requeues", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice"),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: false,
        summary: "tests failed",
        output: "tests failed",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("failed");
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe("paused");

    fixture.store.close();
  });

  test("resumes from open-pr after a retryable PR creation failure", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    let failuresRemaining = 1;
    github.createPullRequest = async (input) => {
      github.createPullRequestCalls.push(input);
      if (failuresRemaining > 0) {
        failuresRemaining -= 1;
        throw new Error("temporary GitHub failure");
      }
      return {
        number: 101,
        url: "https://example.test/acme/looper/pull/101",
      };
    };
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const logs = createCapturingLogger();
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: logs.logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const firstClaim = fixture.queue.claimNext("worker-1");
    if (!firstClaim) {
      throw new Error("Expected first worker queue item claim");
    }

    const firstResult = await runner.processClaimedItem(firstClaim);
    expect(firstResult.status).toBe("failed");
    expect(firstResult.failureKind).toBe("retryable_after_resume");
    expect(agent.starts).toHaveLength(1);

    const retryClaim = fixture.queue.claimNext("worker-1");
    if (!retryClaim) {
      throw new Error("Expected retry worker queue item claim");
    }

    const retryResult = await runner.processClaimedItem(retryClaim);
    expect(retryResult.status).toBe("success");
    expect(agent.starts).toHaveLength(1);
    expect(retryResult.pullRequestNumber).toBe(101);

    fixture.store.close();
  });

  test("skips PR creation when loop already tracks a pull request", async () => {
    const fixture = await createFixture();
    fixture.store.loops.upsert({
      ...(fixture.store.loops.getById("loop_worker_1") ?? {
        id: "loop_worker_1",
        projectId: "project_1",
        type: "worker",
        targetType: "project",
        targetId: "project_1",
        repo: "acme/looper",
        prNumber: 101,
        status: "queued",
        configJson: null,
        metadataJson: null,
        lastRunAt: null,
        nextRunAt: fixture.now.toISOString(),
        createdAt: fixture.now.toISOString(),
        updatedAt: fixture.now.toISOString(),
      }),
      prNumber: 101,
      metadataJson: JSON.stringify({
        prUrl: "https://example.test/acme/looper/pull/101",
      }),
      updatedAt: fixture.now.toISOString(),
    });

    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("success");
    expect(result.pullRequestNumber).toBe(101);
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });

  test("pauses for manual PR opening when auto push is disabled", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      allowAutoPush: false,
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("Auto push disabled");
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);
    expect(fixture.store.loops.getById("loop_worker_1")?.status).toBe(
      "completed",
    );

    fixture.store.close();
  });

  test("skips agent execution when auto commit is disabled", async () => {
    const fixture = await createFixture();
    const git = new FakeGitGateway(fixture.worktreeRoot);
    const github = new FakeGitHubGateway();
    const agent = new FakeAgentExecutor([
      completedAgentResult("Implemented slice and committed changes", [
        "abc123",
      ]),
    ]);
    const runner = new WorkerLoopRunner({
      store: fixture.store,
      scheduler: fixture.queue,
      git,
      github,
      agentExecutor: agent,
      logger: createCapturingLogger().logger,
      now: () => fixture.now,
      validationRunner: async (): Promise<WorkerValidationResult> => ({
        passed: true,
        summary: "ok",
        output: "ok",
      }),
      openPrStrategy: "all_done",
      allowAutoCommit: false,
    });

    const claimed = fixture.queue.claimNext("worker-1");
    if (!claimed) {
      throw new Error("Expected claimed worker queue item");
    }

    const result = await runner.processClaimedItem(claimed);
    expect(result.status).toBe("skipped");
    expect(result.summary).toContain("Auto commit disabled");
    expect(agent.starts).toHaveLength(0);
    expect(git.pushCalls).toBe(0);
    expect(github.createPullRequestCalls).toHaveLength(0);

    fixture.store.close();
  });
});
