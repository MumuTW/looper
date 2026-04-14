import { homedir } from "node:os";
import { join } from "node:path";

import type { LooperConfig } from "./types";

export function createDefaultLooperConfig(cwd = process.cwd()): LooperConfig {
  const looperHome = join(homedir(), ".looper");

  return {
    server: {
      host: "127.0.0.1",
      port: 4310,
      authMode: "none",
    },
    storage: {
      mode: "sqlite",
      dbPath: join(looperHome, "looper.sqlite"),
      backupDir: join(looperHome, "backups"),
    },
    scheduler: {
      pollIntervalSeconds: 30,
      maxConcurrentRuns: 1,
      retryMaxAttempts: 5,
      retryBaseDelayMs: 5_000,
    },
    agent: {
      env: {},
      params: {},
    },
    logging: {
      level: "info",
      maxSizeMB: 10,
      maxFiles: 5,
    },
    notifications: {
      inApp: true,
      osascript: {
        enabled: true,
        soundForLevels: ["action_required", "failure"],
        throttleWindowSeconds: 60,
      },
    },
    tools: {},
    daemon: {
      mode: "foreground",
      logDir: join(looperHome, "logs"),
      workingDirectory: cwd,
      environment: {},
    },
    package: {
      distribution: "npm",
      autoMigrateOnStartup: true,
      requireBackupBeforeMigrate: false,
    },
    defaults: {
      baseBranch: "main",
      allowAutoCommit: true,
      allowAutoPush: true,
      allowAutoApprove: false,
      allowAutoMerge: false,
      allowRiskyFixes: false,
      openPrStrategy: "manual",
    },
    projects: [],
  };
}

export function getDefaultConfigPath(): string {
  return join(homedir(), ".looper", "config.json");
}
