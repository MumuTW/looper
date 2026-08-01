import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "@/App";
import { ProjectsPage } from "@/pages/Projects";
import { DashboardDataProvider } from "@/lib/DashboardDataContext";
import { ProjectFilterProvider } from "@/lib/ProjectFilterContext";
import { ToastProvider } from "@/lib/toast";
import type { Project } from "@/lib/api";

function response(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400 ? { ok: false, error: data } : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
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

type DaemonOptions = {
  projects?: () => Response | Promise<Response>;
};

function stubDaemon(opts: DaemonOptions = {}): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/v1/healthz") return response({ healthy: true });
    if (url === "/api/v1/projects") return opts.projects?.() ?? response({ items: [] });
    if (url === "/api/v1/runs/active") return response({ items: [] });
    return response({ items: [] });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function renderProjects(initialPath = "/projects") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <ToastProvider>
        <DashboardDataProvider>
          <ProjectFilterProvider>
            <Routes>
              <Route path="/projects" element={<ProjectsPage />} />
              <Route
                path="/loops"
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

describe("ProjectsPage", { timeout: 30_000 }, () => {
  it("shows a loading state before the first projects response", async () => {
    stubDaemon({ projects: () => new Promise<Response>(() => {}) });

    renderProjects();

    expect(await screen.findByText("Loading projects…")).toBeTruthy();
  });

  it("shows the empty state when there are no projects", async () => {
    stubDaemon();

    renderProjects();

    expect(await screen.findByText("No projects")).toBeTruthy();
  });

  it("surfaces a panel error with a retry affordance when the projects fetch fails", async () => {
    let calls = 0;
    stubDaemon({
      projects: () => {
        calls += 1;
        return calls === 1
          ? response({ message: "projects backend down" }, 500)
          : response({ items: [projectFixture()] });
      },
    });

    renderProjects();

    expect(await screen.findByText("projects backend down")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(screen.getByText("looper")).toBeTruthy();
    });
  });

  it("renders project authority fields and flags archived projects", async () => {
    stubDaemon({
      projects: () =>
        response({
          items: [
            projectFixture(),
            projectFixture({
              id: "project_2",
              name: "legacy",
              repo: "acme/legacy",
              baseBranch: "develop",
              provider: "gitlab",
              archived: true,
            }),
          ],
        }),
    });

    renderProjects();

    expect(await screen.findByText("looper")).toBeTruthy();
    expect(screen.getByText("acme/looper")).toBeTruthy();
    expect(screen.getByText("main")).toBeTruthy();
    expect(screen.getByText("github")).toBeTruthy();
    // Archived project is flagged for the operator.
    expect(screen.getByText("legacy")).toBeTruthy();
    expect(screen.getByText("(archived)")).toBeTruthy();
    expect(screen.getByText("develop")).toBeTruthy();
  });

  it("sets the project filter and navigates to Loops when a row is clicked", async () => {
    stubDaemon({
      projects: () => response({ items: [projectFixture()] }),
    });

    renderProjects();

    const row = await screen.findByText("looper");
    fireEvent.click(row.closest("tr")!);

    await waitFor(() => {
      expect(screen.getByTestId("loops-route").textContent).toBe("/loops");
    });
    expect(localStorage.getItem("looper.dashboard.projectFilter")).toBe("project_1");
});

it("lists projects and exposes no registration control", async () => {
  window.history.replaceState({}, "", "/dashboard/projects");
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    switch (String(input)) {
      case "/api/v1/healthz": return response({ healthy: true });
      case "/api/v1/runs/active": return response({ items: [] });
      case "/api/v1/projects": return response({ items: [{ id: "project_1", name: "Looper", repoPath: "/tmp/looper", baseBranch: "main", archived: false, provider: "github", repo: "MumuTW/looper", createdAt: "2026-07-30T00:00:00Z", updatedAt: "2026-07-30T00:00:00Z" }] });
      default: throw new Error(`unexpected request: ${input}`);
    }
  }));

  render(<App />);
  expect(await screen.findByRole("heading", { name: "Projects" })).toBeTruthy();
  expect(await screen.findByRole("cell", { name: "Looper" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Refresh" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: /add|create|register/i })).toBeNull();
});
});
