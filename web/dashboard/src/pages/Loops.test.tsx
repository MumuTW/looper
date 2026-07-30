import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { LoopsPage } from "@/pages/Loops";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import type { ActiveRun, Loop, Project } from "@/lib/api";

function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400 ? { ok: false, error: data } : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
}

function loopFixture(overrides: Partial<Loop> = {}): Loop {
  return {
    id: "loop_7",
    seq: 7,
    projectId: "project_1",
    type: "fixer",
    targetType: "pull_request",
    targetId: "pr:acme/looper:7",
    repo: "acme/looper",
    prNumber: 7,
    status: "failed",
    createdAt: "2026-07-30T12:00:00.000Z",
    updatedAt: "2026-07-30T12:00:00.000Z",
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
    target: {
      type: "pull_request",
      projectId: "project_1",
      repo: "acme/looper",
      prNumber: 7,
      label: "acme/looper#7",
    },
    agent: {
      active: true,
      activeCount: 1,
      executionId: "exec_7",
      vendor: "codex",
      pid: 1234,
      startedAt: "2026-07-30T12:00:00.000Z",
      heartbeatCount: 1,
      status: "running",
    },
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

type LoopsHandler = (query: URLSearchParams) => Response | Promise<Response>;

type DaemonOptions = {
  loops?: LoopsHandler;
  activeRuns?: () => Response | Promise<Response>;
  projects?: () => Response | Promise<Response>;
  stop?: (selector: string) => Response | Promise<Response>;
};

function stubDaemon(opts: DaemonOptions = {}): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/v1/healthz") return response({ healthy: true });
    if (url === "/api/v1/projects") return opts.projects?.() ?? response({ items: [projectFixture()] });
    if (url === "/api/v1/runs/active") return opts.activeRuns?.() ?? response({ items: [] });
    if (url.includes("/runs/active/") && url.endsWith("/stop") && init?.method === "POST") {
      const sel = url.split("/runs/active/")[1]?.replace("/stop", "") ?? "";
      return opts.stop?.(sel) ?? response({ stopped: true, loopId: "loop_7" });
    }
    if (url.startsWith("/api/v1/loops")) {
      const qs = url.includes("?") ? new URLSearchParams(url.split("?")[1]) : new URLSearchParams();
      return opts.loops?.(qs) ?? response({ items: [], total: 0 });
    }
    return response({ items: [] });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderLoops(initialPath = "/loops") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route path="/loops" element={<LoopsPage />} />
              <Route
                path="/loops/:selector"
                element={<div data-testid="loops-route">loops</div>}
              />
            </Routes>
          </ProjectFilterProvider>
        </DashboardDataProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
});

// Multi-step flows (pagination, confirm dialogs, post-mutation refreshes) take
// seconds each on a loaded CI machine; keep a generous backstop local to this
// suite instead of weakening every dashboard test.
describe("LoopsPage", { timeout: 30_000 }, () => {
  it("shows a loading state before the first loops response", async () => {
    stubDaemon({
      loops: () => new Promise<Response>(() => {}),
    });

    renderLoops();

    expect(await screen.findByText("Loading loops…")).toBeTruthy();
  });

  it("shows the empty state when there are no loops", async () => {
    stubDaemon();

    renderLoops();

    expect(await screen.findByText("No loops")).toBeTruthy();
  });

  it("surfaces a panel error with a retry affordance when the loops fetch fails", async () => {
    let calls = 0;
    stubDaemon({
      loops: () => {
        calls += 1;
        return calls === 1
          ? response({ message: "loops backend down" }, 500)
          : response({ items: [loopFixture()], total: 1 });
      },
    });

    renderLoops();

    expect(await screen.findByText("loops backend down")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(screen.getByText("acme/looper#7")).toBeTruthy();
    });
  });

  it("renders loop rows and the pagination range footer when populated", async () => {
    const items = Array.from({ length: 25 }, (_, i) =>
      loopFixture({
        id: `loop_${i + 1}`,
        seq: i + 1,
        prNumber: i + 1,
        repo: "acme/looper",
      }),
    );
    stubDaemon({
      loops: (query) => {
        const limit = Number(query.get("limit") ?? 25);
        const offset = Number(query.get("offset") ?? 0);
        return response({ items: items.slice(offset, offset + limit), total: 25 });
      },
    });

    renderLoops();

    expect(await screen.findByText("acme/looper#1")).toBeTruthy();
    // Default page size is 25, so all rows fit on one page.
    expect(screen.getByText(/1–25 of 25/)).toBeTruthy();
  });

  it("filters by status and rewrites the empty state for the active filter", async () => {
    let lastStatus: string | null = null;
    stubDaemon({
      loops: (query) => {
        lastStatus = query.get("status");
        return response({ items: [], total: 0 });
      },
    });

    renderLoops();
    await screen.findByText("No loops");

    fireEvent.change(screen.getByLabelText("Status"), { target: { value: "failed" } });

    await waitFor(() => {
      expect(screen.getByText("No loops with status=failed")).toBeTruthy();
    });
    expect(lastStatus).toBe("failed");
  });

  it("paginates by page size and requests the matching offset", async () => {
    const items = Array.from({ length: 25 }, (_, i) =>
      loopFixture({ id: `loop_${i + 1}`, seq: i + 1, prNumber: i + 1 }),
    );
    const fetchMock = stubDaemon({
      loops: (query) => {
        const limit = Number(query.get("limit") ?? 25);
        const offset = Number(query.get("offset") ?? 0);
        return response({ items: items.slice(offset, offset + limit), total: 25 });
      },
    });

    renderLoops();
    await screen.findByText("acme/looper#1");

    // Shrink to 10 per page → 3 pages.
    fireEvent.change(screen.getByLabelText("Per page"), { target: { value: "10" } });
    await waitFor(() => {
      expect(screen.getByText(/1–10 of 25/)).toBeTruthy();
    });

    // Advance to page 2 → offset 10.
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() => {
      expect(screen.getByText(/11–20 of 25/)).toBeTruthy();
    });
    expect(
      fetchMock.mock.calls.some(
        ([input]) => String(input).includes("offset=10") && String(input).includes("limit=10"),
      ),
    ).toBe(true);
  });

  it("clamps the page back when the total shrinks below the current page", async () => {
    let total = 25;
    const items = Array.from({ length: 25 }, (_, i) =>
      loopFixture({ id: `loop_${i + 1}`, seq: i + 1, prNumber: i + 1 }),
    );
    stubDaemon({
      loops: (query) => {
        const limit = Number(query.get("limit") ?? 25);
        const offset = Number(query.get("offset") ?? 0);
        return response({ items: items.slice(offset, offset + limit), total });
      },
    });

    renderLoops();
    await screen.findByText("acme/looper#1");
    fireEvent.change(screen.getByLabelText("Per page"), { target: { value: "10" } });
    await waitFor(() => expect(screen.getByText(/1–10 of 25/)).toBeTruthy());

    // Jump to the last page (3 of 3).
    fireEvent.click(screen.getByRole("button", { name: "Last page" }));
    await waitFor(() => expect(screen.getByText(/21–25 of 25/)).toBeTruthy());

    // The total shrinks on the next refresh; page 3 is now out of range.
    total = 2;
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => {
      expect(screen.getByText(/1–2 of 2/)).toBeTruthy();
    });
  });

  it("scopes requests to the persisted project filter and shows the project label", async () => {
    localStorage.setItem("looper.dashboard.projectFilter", "project_1");
    let lastProject: string | null = null;
    stubDaemon({
      projects: () => response({ items: [projectFixture()] }),
      loops: (query) => {
        lastProject = query.get("projectId");
        return response({ items: [loopFixture()], total: 1 });
      },
    });

    renderLoops();

    expect(await screen.findByText("project: project_1")).toBeTruthy();
    expect(lastProject).toBe("project_1");
  });

  it("navigates to the loop detail route when a row is clicked", async () => {
    stubDaemon({
      loops: () => response({ items: [loopFixture()], total: 1 }),
    });

    renderLoops();

    const row = await screen.findByText("acme/looper#7");
    fireEvent.click(row.closest("tr")!);

    await waitFor(() => {
      expect(screen.getByTestId("loops-route")).toBeTruthy();
    });
  });

  it("refreshes both loops and active runs after a destructive stop mutation", async () => {
    let stopped = false;
    const fetchMock = stubDaemon({
      activeRuns: () => response(stopped ? { items: [] } : { items: [activeRunFixture()] }),
      loops: () =>
        response({
          items: [loopFixture({ status: stopped ? "paused" : "running" })],
          total: 1,
        }),
      stop: () => {
        stopped = true;
        return response({ stopped: true, loopId: "loop_7" });
      },
    });

    renderLoops();

    // The running loop is joined to its active run, so Stop is enabled.
    expect(await screen.findByText("acme/looper#7")).toBeTruthy();
    // The row status chip is an authority field, distinct from the "running"
    // option in the status filter dropdown.
    const row = screen.getByText("acme/looper#7").closest("tr")!;
    expect(row.textContent).toContain("running");

    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop" }));

    await waitFor(() => {
      expect(screen.getByText("paused")).toBeTruthy();
    });
    expect(
      fetchMock.mock.calls.some(([input]) => String(input) === "/api/v1/runs/active"),
    ).toBe(true);
    expect(
      fetchMock.mock.calls.some(([input]) => String(input).startsWith("/api/v1/loops")),
    ).toBe(true);
  });
});
