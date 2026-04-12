import { randomUUID } from "node:crypto";

import type { AgentConfig, AgentVendor } from "../config/index";
import type { Store } from "../storage/store";
import type { AgentExecutionRecord } from "../storage/types";
import { AGENT_COMPLETION_MARKER } from "./agent-prompt";

export interface AgentRunInput {
  executionId?: string;
  projectId?: string;
  loopId?: string;
  runId?: string;
  prompt: string;
  workingDirectory: string;
  timeoutMs: number;
  heartbeatTimeoutMs?: number;
  gracefulShutdownMs?: number;
  maxOutputBytes?: number;
  idempotencyKey?: string;
  metadata?: Record<string, unknown>;
  env?: Record<string, string>;
}

export interface AgentResult {
  status: "completed" | "failed" | "timeout" | "killed";
  summary?: string;
  artifacts: string[];
  changedFiles: string[];
  commits: string[];
  rawLogs: {
    stdout: string;
    stderr: string;
  };
  parseStatus: "parsed" | "missing" | "invalid_json";
  completionSignal?: string;
  heartbeatCount: number;
  resourceUsage: {
    wallTimeMs: number;
    outputBytes: number;
  };
  pid: number;
}

export interface AgentExecution {
  pid: number;
  startedAt: string;
  status: "running" | "completed" | "failed" | "timeout" | "killed";
  wait(): Promise<AgentResult>;
  kill(reason: string): Promise<void>;
}

export interface AgentExecutorOptions {
  config: AgentConfig & { vendor: AgentVendor };
  store?: Store;
  now?: () => Date;
}

type ConfiguredAgentConfig = AgentConfig & { vendor: AgentVendor };

export interface ResolvedAgentSpawn {
  command: string;
  args: string[];
}

const COMPLETION_MARKER = `${AGENT_COMPLETION_MARKER}=`;

export class ConfiguredAgentExecutor {
  private readonly now: () => Date;

  constructor(private readonly options: AgentExecutorOptions) {
    this.now = options.now ?? (() => new Date());
  }

  public async start(input: AgentRunInput): Promise<AgentExecution> {
    const executionId = input.executionId ?? randomUUID();
    const startedAtIso = this.now().toISOString();
    const { command, args } = resolveAgentSpawn(
      this.options.config,
      input.prompt,
    );
    const env = {
      ...this.options.config.env,
      ...input.env,
      LOOPER_PROMPT: input.prompt,
      LOOPER_COMPLETION_MARKER: COMPLETION_MARKER,
    };

    const subprocess = Bun.spawn({
      cmd: [command, ...args],
      cwd: input.workingDirectory,
      env,
      stdout: "pipe",
      stderr: "pipe",
    });

    let status: AgentExecution["status"] = "running";
    let heartbeatCount = 0;
    let lastHeartbeatAt = startedAtIso;
    let stdout = "";
    let stderr = "";
    const maxOutputBytes = input.maxOutputBytes ?? 256 * 1024;
    let timedOut = false;
    let killed = false;
    let killReason: string | undefined;

    this.upsertExecution({
      id: executionId,
      projectId: input.projectId,
      loopId: input.loopId,
      runId: input.runId,
      vendor: this.options.config.vendor,
      status,
      pid: subprocess.pid,
      commandJson: JSON.stringify({ command, args }),
      cwd: input.workingDirectory,
      heartbeatCount,
      lastHeartbeatAt,
      startedAt: startedAtIso,
      endedAt: null,
      metadataJson: JSON.stringify({
        idempotencyKey: input.idempotencyKey ?? null,
        metadata: input.metadata ?? null,
      }),
      createdAt: startedAtIso,
      updatedAt: startedAtIso,
    });
    this.appendEvent({
      id: randomUUID(),
      eventType: "agent.invoked",
      projectId: input.projectId,
      loopId: input.loopId,
      runId: input.runId,
      entityType: "agent_execution",
      entityId: executionId,
      actorType: "agent",
      actorId: this.options.config.vendor,
      actorDisplayName: this.options.config.vendor,
      payloadJson: JSON.stringify({
        command,
        args,
        cwd: input.workingDirectory,
      }),
      createdAt: startedAtIso,
    });

    const capture = async (
      stream: ReadableStream<Uint8Array> | undefined,
      target: "stdout" | "stderr",
    ) => {
      if (!stream) {
        return;
      }

      const reader = stream.getReader();
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          break;
        }

        if (!value) {
          continue;
        }

        heartbeatCount += 1;
        lastHeartbeatAt = this.now().toISOString();
        const chunk = new TextDecoder().decode(value);
        if (target === "stdout") {
          stdout = appendBounded(stdout, chunk, maxOutputBytes);
        } else {
          stderr = appendBounded(stderr, chunk, maxOutputBytes);
        }

        // TODO: debounce execution heartbeat writes before agent usage gets chatty.
        this.upsertExecution({
          ...(this.options.store?.agentExecutions.getById(executionId) ?? {
            id: executionId,
            vendor: this.options.config.vendor,
            status,
            commandJson: JSON.stringify({ command, args }),
            cwd: input.workingDirectory,
            heartbeatCount: 0,
            startedAt: startedAtIso,
            createdAt: startedAtIso,
            updatedAt: startedAtIso,
          }),
          heartbeatCount,
          lastHeartbeatAt,
          outputJson: JSON.stringify({ stdout, stderr }),
          updatedAt: lastHeartbeatAt,
        });
      }
    };

    const stdoutPromise = capture(subprocess.stdout, "stdout");
    const stderrPromise = capture(subprocess.stderr, "stderr");

    const timeoutHandle = setTimeout(() => {
      timedOut = true;
      status = "timeout";
      subprocess.kill("SIGTERM");
      setTimeout(() => {
        if (subprocess.exitCode === null) {
          subprocess.kill("SIGKILL");
        }
      }, input.gracefulShutdownMs ?? 5_000);
    }, input.timeoutMs);

    const heartbeatTimeoutMs = input.heartbeatTimeoutMs;
    const inactivityHandle = heartbeatTimeoutMs
      ? setInterval(
          () => {
            const elapsed = Date.now() - new Date(lastHeartbeatAt).getTime();
            if (subprocess.exitCode === null && elapsed >= heartbeatTimeoutMs) {
              timedOut = true;
              status = "timeout";
              subprocess.kill("SIGTERM");
            }
          },
          Math.min(heartbeatTimeoutMs, 1_000),
        )
      : undefined;

    const wait = async (): Promise<AgentResult> => {
      const exitCode = await subprocess.exited;
      clearTimeout(timeoutHandle);
      if (inactivityHandle) {
        clearInterval(inactivityHandle);
      }
      await Promise.all([stdoutPromise, stderrPromise]);

      if (!timedOut && !killed) {
        status = exitCode === 0 ? "completed" : "failed";
      } else if (killed && status !== "timeout") {
        status = "killed";
      }

      const parsed = parseCompletion(stdout, stderr);
      const endedAt = this.now().toISOString();
      const finalStatus =
        status === "running"
          ? exitCode === 0
            ? "completed"
            : "failed"
          : status;

      const result: AgentResult = {
        status: finalStatus,
        summary: parsed.summary,
        artifacts: parsed.artifacts,
        changedFiles: parsed.changedFiles,
        commits: parsed.commits,
        rawLogs: { stdout, stderr },
        parseStatus: parsed.parseStatus,
        completionSignal: parsed.completionSignal,
        heartbeatCount,
        resourceUsage: {
          wallTimeMs: Date.now() - new Date(startedAtIso).getTime(),
          outputBytes: Buffer.byteLength(stdout) + Buffer.byteLength(stderr),
        },
        pid: subprocess.pid,
      };

      this.upsertExecution({
        ...(this.options.store?.agentExecutions.getById(executionId) ?? {
          id: executionId,
          vendor: this.options.config.vendor,
          commandJson: JSON.stringify({ command, args }),
          cwd: input.workingDirectory,
          heartbeatCount,
          startedAt: startedAtIso,
          createdAt: startedAtIso,
          updatedAt: endedAt,
        }),
        projectId: input.projectId ?? null,
        loopId: input.loopId ?? null,
        runId: input.runId ?? null,
        status: finalStatus,
        pid: subprocess.pid,
        summary: result.summary ?? null,
        parseStatus: result.parseStatus,
        completionSignal: result.completionSignal ?? null,
        heartbeatCount,
        lastHeartbeatAt,
        outputJson: JSON.stringify(result.rawLogs),
        errorMessage:
          finalStatus === "failed" ||
          finalStatus === "timeout" ||
          finalStatus === "killed"
            ? stderr || killReason || undefined
            : undefined,
        endedAt,
        updatedAt: endedAt,
      });
      this.appendEvent({
        id: randomUUID(),
        eventType:
          finalStatus === "timeout"
            ? "agent.timed_out"
            : finalStatus === "killed"
              ? "agent.killed"
              : "agent.completed",
        projectId: input.projectId,
        loopId: input.loopId,
        runId: input.runId,
        entityType: "agent_execution",
        entityId: executionId,
        actorType: "agent",
        actorId: this.options.config.vendor,
        actorDisplayName: this.options.config.vendor,
        payloadJson: JSON.stringify({
          status: finalStatus,
          parseStatus: result.parseStatus,
          heartbeatCount,
          summary: result.summary,
        }),
        createdAt: endedAt,
      });

      return result;
    };

    return {
      pid: subprocess.pid,
      startedAt: startedAtIso,
      get status() {
        return status;
      },
      wait,
      kill: async (reason: string) => {
        killed = true;
        killReason = reason;
        status = "killed";
        subprocess.kill("SIGTERM");
      },
    };
  }

  private upsertExecution(record: AgentExecutionRecord): void {
    this.options.store?.agentExecutions.upsert(record);
  }

  private appendEvent(record: Parameters<Store["events"]["append"]>[0]): void {
    this.options.store?.events.append(record);
  }
}

export function resolveAgentSpawn(
  config: ConfiguredAgentConfig,
  prompt: string,
): ResolvedAgentSpawn {
  return {
    command: resolveCommand(config),
    args: resolveArgs(config, prompt),
  };
}

function resolveCommand(config: ConfiguredAgentConfig): string {
  const override = config.params?.command;
  if (typeof override === "string" && override.length > 0) {
    return override;
  }

  switch (config.vendor) {
    case "claude-code":
      return "claude";
    case "cursor-cli":
      return "agent";
    default:
      return config.vendor;
  }
}

function resolveArgs(config: ConfiguredAgentConfig, prompt: string): string[] {
  const args = config.params?.args;
  const resolvedArgs = Array.isArray(args)
    ? args.filter((value): value is string => typeof value === "string")
    : [];

  switch (config.vendor) {
    case "claude-code":
      return resolveClaudeArgs(config, resolvedArgs, prompt);
    case "codex":
      return resolveCodexArgs(config, resolvedArgs, prompt);
    case "opencode":
      return resolveOpenCodeArgs(config, resolvedArgs, prompt);
    case "cursor-cli":
      return resolveCursorArgs(config, resolvedArgs, prompt);
  }
}

function resolveClaudeArgs(
  config: ConfiguredAgentConfig,
  args: string[],
  prompt: string,
): string[] {
  const resolved = prependModelFlag(args, config.model, "--model", ["--model"]);
  if (hasAnyFlag(resolved, ["-p", "--print"])) {
    return resolved;
  }

  return [...resolved, "--print", prompt];
}

function resolveCodexArgs(
  config: ConfiguredAgentConfig,
  args: string[],
  prompt: string,
): string[] {
  const resolved = args.includes("exec") ? [...args] : ["exec", ...args];
  const withModel = prependModelFlag(resolved, config.model, "--model", [
    "--model",
    "-m",
  ]);
  if (withModel.includes("-")) {
    return withModel;
  }

  return [...withModel, prompt];
}

function resolveOpenCodeArgs(
  config: ConfiguredAgentConfig,
  args: string[],
  prompt: string,
): string[] {
  const resolved = args.includes("run") ? [...args] : ["run", ...args];
  const withModel = prependModelFlag(resolved, config.model, "--model", [
    "--model",
    "-m",
  ]);
  if (hasAnyFlag(withModel, ["-p", "--prompt", "-f", "--file"])) {
    return withModel;
  }

  return [...withModel, prompt];
}

function resolveCursorArgs(
  config: ConfiguredAgentConfig,
  args: string[],
  prompt: string,
): string[] {
  const resolved = prependModelFlag(args, config.model, "--model", ["--model"]);
  if (hasAnyFlag(resolved, ["-p", "--print"])) {
    return resolved;
  }

  return [...resolved, "--print", prompt];
}

function prependModelFlag(
  args: string[],
  model: string | undefined,
  flag: string,
  recognizedFlags: string[],
): string[] {
  if (!model || hasAnyFlag(args, recognizedFlags)) {
    return [...args];
  }

  if (args[0] === "exec" || args[0] === "run") {
    return [args[0], flag, model, ...args.slice(1)];
  }

  return [flag, model, ...args];
}

function hasAnyFlag(args: string[], flags: string[]): boolean {
  return flags.some((flag) => args.includes(flag));
}

function appendBounded(
  current: string,
  chunk: string,
  maxBytes: number,
): string {
  const combined = current + chunk;
  const bytes = Buffer.from(combined);
  if (bytes.byteLength <= maxBytes) {
    return combined;
  }

  return bytes.subarray(bytes.byteLength - maxBytes).toString();
}

function parseCompletion(stdout: string, stderr: string) {
  const raw = `${stdout}\n${stderr}`;
  const line = raw
    .split(/\r?\n/)
    .reverse()
    .find((candidate) => candidate.startsWith(COMPLETION_MARKER));

  if (!line) {
    return {
      parseStatus: "missing" as const,
      completionSignal: undefined,
      summary: summarizeLogs(stdout, stderr),
      artifacts: [],
      changedFiles: [],
      commits: [],
    };
  }

  const payload = line.slice(COMPLETION_MARKER.length);
  try {
    const parsed = JSON.parse(payload) as Record<string, unknown>;
    return {
      parseStatus: "parsed" as const,
      completionSignal: COMPLETION_MARKER,
      summary:
        typeof parsed.summary === "string"
          ? parsed.summary
          : summarizeLogs(stdout, stderr),
      artifacts: asStringArray(parsed.artifacts),
      changedFiles: asStringArray(parsed.changedFiles),
      commits: asStringArray(parsed.commits),
    };
  } catch {
    return {
      parseStatus: "invalid_json" as const,
      completionSignal: COMPLETION_MARKER,
      summary: summarizeLogs(stdout, stderr),
      artifacts: [],
      changedFiles: [],
      commits: [],
    };
  }
}

function summarizeLogs(stdout: string, stderr: string): string | undefined {
  const text = `${stdout}\n${stderr}`.trim();
  if (!text) {
    return undefined;
  }

  return text.split(/\r?\n/).filter(Boolean).at(-1);
}

function asStringArray(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}
