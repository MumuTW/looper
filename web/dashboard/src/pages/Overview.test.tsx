import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OverviewPage } from "@/pages/Overview";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import type {
  ActiveRun,
  ActiveRunAgent,
  ActiveRunTarget,
  HealthzData,
  Project,
  StatusData,
} from "@/lib/api";

function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400 ? { ok: false, error: data } : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
}

function agentFixture(): ActiveRunAgent {
  return {
    active: true,
    activeCount: 1,
    executionId: "exec_7",
    vendor: "codex",
    pid: 1234,
    startedAt: "2026-07-30T12:00:00.000Z",
    heartbeatCount: 1,
    status: "running",
  };
}

function targetFixture(overrides: Partial<ActiveRunTarget> = {}): ActiveRunTarget {
  return {
    type: "pull_request",
    projectId: "project_1",
    repo: "acme/looper",
    prNumber: 42,
    label: "acme/looper#42",
    ...overrides,
  };
}

function activeRunFixture(overrides: Partial<ActiveRun> = {}): ActiveRun {
  return {
    seq: 7,
    runId: "run_7",
    loopId: "loop_7",
    projectId: "project_1",
    type: "fixer",
    status: "running",
    loopStatus: "running",
    displayStatus: "running",
    attempts: 1,
    maxAttempts: 3,
    target: targetFixture(),
    agent: agentFixture(),
    startedAt: "2026-07-30T12:00:00.000Z",
    ...overrides,
  };
}

function projectFixture(overrides: Partial<Project> = {}): Project {
  return {
    id: "project_1",
    name: "looper",
    repoPath: "/tmp/looper",
    baseBranch: "main",
    archived: false,
    provider: "github",
    repo: "acme/looper",
    createdAt: "2026-07-30T12:00:00.000Z",
    updatedAt: "2026-07-30T12:00:00.000Z",
    ...overrides,
  };
}

function statusFixture(overrides: Partial<StatusData> = {}): StatusData {
  return {
    service: {
      healthy: true,
      version: "0.0.1",
      daemonMode: "managed",
      startedAt: "2026-07-30T11:00:00.000Z",
    },
    scheduler: {
      healthy: true,
      queuedItems: 1,
      runningItems: 2,
      activeRuns: 3,
      totalRuns: 10,
      failedItems: 0,
    },
    loops: { fixer: { running: 2, queued: 1, failed: 0 } },
    storage: { healthy: true, mode: "sqlite", dbPath: "/tmp/looper.sqlite" },
    agent: { vendor: "codex" },
    ...overrides,
  };
}

function healthzFixture(overrides: Partial<HealthzData> = {}): HealthzData {
  return {
    healthy: true,
    startedAt: "2026-07-30T11:00:00.000Z",
    storage: { ok: true, mode: "sqlite", dbPath: "/tmp/looper.sqlite" },
    ...overrides,
  };
}

type DaemonOptions = {
  healthz?: () => Response | Promise<Response>;
  activeRuns?: () => Response | Promise<Response>;
  projects?: () => Response | Promise<Response>;
  status?: () => Response | Promise<Response>;
  stop?: (selector: string) => Response | Promise<Response>;
};

function stubDaemon(opts: DaemonOptions = {}): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/v1/healthz") return opts.healthz?.() ?? response(healthzFixture());
    if (url === "/api/v1/projects") return opts.projects?.() ?? response({ items: [projectFixture()] });
    if (url === "/api/v1/status") return opts.status?.() ?? response(statusFixture());
    if (url === "/api/v1/runs/active") return opts.activeRuns?.() ?? response({ items: [] });
    if (url.includes("/runs/active/") && url.endsWith("/stop") && init?.method === "POST") {
      const sel = url.split("/runs/active/")[1]?.replace("/stop", "") ?? "";
      return opts.stop?.(sel) ?? response({ stopped: true, loopId: "loop_7" });
    }
    return response({ items: [] });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderOverview(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route
                path="/"
                element={<OverviewPage onHealthChange={() => {}} />}
              />
              <Route
                path="/loops/:selector"
                element={<RouteProbe />}
              />
            </Routes>
          </ProjectFilterProvider>
        </DashboardDataProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function RouteProbe() {
  return <div data-testid="loops-route">{useLocation().pathname}</div>;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

// These multi-step flows (confirm dialogs, post-mutation refreshes) take
// seconds each on a loaded CI machine, so keep a generous backstop local to
// this suite instead of weakening every dashboard test.
describe("OverviewPage", { timeout: 30_000 }, () => {
  it("shows a loading state while the first healthz is in flight", async () => {
    // Never-resolving healthz keeps loading=true with no data and no error.
    stubDaemon({
      healthz: () => new Promise<Response>(() => {}),
      status: () => new Promise<Response>(() => {}),
    });

    renderOverview();

    expect(await screen.findByText("Loading overview…")).toBeTruthy();
  });

  it("renders the daemon-unreachable error surface and recovers on retry", async () => {
    let healthCalls = 0;
    stubDaemon({
      healthz: () => {
        healthCalls += 1;
        return healthCalls === 1
          ? Promise.reject(new Error("connection refused"))
          : response(healthzFixture());
      },
    });

    renderOverview();

    expect(await screen.findByRole("heading", { name: "Daemon unreachable" })).toBeTruthy();
    expect(screen.getByText(/GET \/api\/v1\/healthz/)).toBeTruthy();
    expect(healthCalls).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "Overview" })).toBeTruthy();
    });
    expect(healthCalls).toBeGreaterThanOrEqual(2);
  });

  it("presents service, scheduler, loop, and storage authority fields when populated", async () => {
    stubDaemon();

    renderOverview();

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "Overview" })).toBeTruthy();
    });
    // Service card authority fields.
    expect(screen.getByText("0.0.1")).toBeTruthy();
    expect(screen.getByText("managed")).toBeTruthy();
    // Scheduler card authority fields.
    expect(screen.getByText("Active runs")).toBeTruthy();
    // Keep the active-run count distinct from the running-item count and scope
    // it to the exact scheduler row.
    const activeRunsRow = screen.getByText("Active runs").parentElement;
    expect(within(activeRunsRow!).getByText("3")).toBeTruthy();
    // Loops card totals (summed across roles).
    const loopsCard = screen.getByText("running").closest("dl");
    expect(loopsCard?.textContent).toContain("2");
    // Storage card authority fields.
    expect(screen.getByText("sqlite")).toBeTruthy();
    expect(screen.getByText("/tmp/looper.sqlite")).toBeTruthy();
  });

  it("lists running loops with their seq, target, and status", async () => {
    stubDaemon({
      activeRuns: () =>
        response({ items: [activeRunFixture()] }),
    });

    renderOverview();

    expect(await screen.findByText("acme/looper#42")).toBeTruthy();
    expect(screen.getByText("7")).toBeTruthy();
    // The running row's status chip is an authority field, distinct from the
    // Loops card's "running" total label.
    const row = screen.getByText("acme/looper#42").closest("tr")!;
    expect(row.textContent).toContain("running");
  });

  it("filters running loops by the persisted project filter and rewrites the empty state", async () => {
    localStorage.setItem("looper.dashboard.projectFilter", "project_1");
    stubDaemon({
      projects: () =>
        response({
          items: [projectFixture(), projectFixture({ id: "project_2", name: "other" })],
        }),
      activeRuns: () =>
        response({
          items: [
            activeRunFixture({ seq: 7, projectId: "project_1" }),
            activeRunFixture({
              seq: 8,
              loopId: "loop_8",
              runId: "run_8",
              projectId: "project_2",
              target: targetFixture({ projectId: "project_2", label: "acme/other#8" }),
            }),
          ],
        }),
    });

    renderOverview();

    expect(await screen.findByText("Running loops · project project_1")).toBeTruthy();
    expect(screen.getByText("7")).toBeTruthy();
    expect(screen.queryByText("8")).toBeNull();
    expect(screen.queryByText("acme/other#8")).toBeNull();
  });

  it("shows the project-scoped empty state when no running loops match the filter", async () => {
    localStorage.setItem("looper.dashboard.projectFilter", "project_1");
    stubDaemon({
      activeRuns: () =>
        response({
          items: [activeRunFixture({ projectId: "project_2" })],
        }),
    });

    renderOverview();

    expect(await screen.findByText("No running loops for this project")).toBeTruthy();
  });

  it("navigates to the loop detail route when a running row is clicked", async () => {
    stubDaemon({
      activeRuns: () => response({ items: [activeRunFixture()] }),
    });

    renderOverview();

    const row = await screen.findByText("acme/looper#42");
    // The seq cell is the clickable row's first cell; click the target cell row.
    fireEvent.click(row.closest("tr")!);

    await waitFor(() => {
      expect(screen.getByTestId("loops-route").textContent).toBe("/loops/7");
    });
  });

  it("marks the Loops and Scheduler cards degraded when the status fetch fails", async () => {
    stubDaemon({
      status: () => response({ message: "status backend down" }, 500),
    });

    renderOverview();

    // Scheduler card surfaces the failure with a retry affordance.
    expect(await screen.findByText(/Failed to load status/)).toBeTruthy();
    // Scope this authority state to Loops: Triage also degrades on status
    // failures and has the same copy.
    const loopsCard = screen.getByRole("heading", { name: "Loops" }).closest("section")!;
    expect(within(loopsCard).getByText("Unavailable (status failed)")).toBeTruthy();
  });

  it("refreshes the running loops list after a destructive stop mutation", async () => {
    let stopped = false;
    const fetchMock = stubDaemon({
      activeRuns: () =>
        response(
          stopped ? { items: [] } : { items: [activeRunFixture()] },
        ),
      stop: (selector) => {
        expect(selector).toBe("7");
        stopped = true;
        return response({ stopped: true, loopId: "loop_7" });
      },
    });

    renderOverview();

    expect(await screen.findByText("acme/looper#42")).toBeTruthy();
    const activeRunsCallsBeforeStop = fetchMock.mock.calls.filter(
      ([input]) => String(input) === "/api/v1/runs/active",
    ).length;

    // Open the stop confirmation from the running row's action bar.
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop" }));

    await waitFor(() => {
      expect(screen.getByText("No running loops")).toBeTruthy();
    });
    await waitFor(() => {
      const activeRunsCallsAfterStop = fetchMock.mock.calls.filter(
        ([input]) => String(input) === "/api/v1/runs/active",
      ).length;
      expect(activeRunsCallsAfterStop).toBeGreaterThan(activeRunsCallsBeforeStop);
    });
  });
});
