import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import App from "@/App";

function response(data: unknown): Response {
  return new Response(JSON.stringify({ ok: true, data, error: null }), {
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
});

it("lists projects and exposes no registration control", async () => {
  window.history.replaceState({}, "", "/dashboard/projects");
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    switch (String(input)) {
      case "/api/v1/healthz": return response({ healthy: true });
      case "/api/v1/runs/active": return response({ items: [] });
      case "/api/v1/projects": return response({ items: [{ id: "project_1", name: "Looper", repoPath: "/tmp/looper", baseBranch: "main", archived: false, provider: "github", repo: "MumuTW/looper", gatekeeperTrust: "advise", createdAt: "2026-07-30T00:00:00Z", updatedAt: "2026-07-30T00:00:00Z" }] });
      default: throw new Error(`unexpected request: ${input}`);
    }
  }));

  render(<App />);
  expect(await screen.findByRole("heading", { name: "Projects" })).toBeTruthy();
  expect(await screen.findByRole("cell", { name: "Looper" })).toBeTruthy();
  expect(await screen.findByRole("cell", { name: "advise" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Refresh" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /add|create|register/i })).toBeNull();
});
