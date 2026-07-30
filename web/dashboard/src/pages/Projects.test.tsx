import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@/lib/toast";
import { ProjectsPage } from "./Projects";

describe("Projects page contracts", () => {
  it("renders loading, then populated state", async () => {
    let resolveProjects: (value: unknown) => void;
    const projectsPending = new Promise((r) => { resolveProjects = r; });

    const fetchMock = vi.fn(async (url: string) => {
      if (url.includes("/api/v1/projects")) {
        const result = await projectsPending;
        return new Response(JSON.stringify(result), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      return new Response(JSON.stringify({ healthy: true }), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    global.fetch = fetchMock;

    render(<ToastProvider><ProjectsPage /></ToastProvider>);
    expect(screen.getByText(/loading/i)).toBeTruthy();

    resolveProjects!({ items: [{ id: "p1", name: "Project One", repoPath: "/tmp/p1" }] });
    await waitFor(() => {
      expect(screen.getByText("Project One")).toBeTruthy();
    });
  });

  it("renders empty state", async () => {
    const fetchMock = vi.fn(async () => {
      return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    global.fetch = fetchMock;

    render(<ToastProvider><ProjectsPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/no projects/i)).toBeTruthy();
    });
  });

  it("renders error state", async () => {
    const fetchMock = vi.fn(async () => {
      return new Response(JSON.stringify({ error: "database unavailable" }), {
        status: 500,
        headers: { "Content-Type": "application/json" },
      });
    });
    global.fetch = fetchMock;

    render(<ToastProvider><ProjectsPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText(/error|unavailable/i)).toBeTruthy();
    });
  });

  it("filters projects by name", async () => {
    const fetchMock = vi.fn(async () => {
      return new Response(JSON.stringify({
        items: [
          { id: "p1", name: "Alpha", repoPath: "/tmp/alpha" },
          { id: "p2", name: "Beta", repoPath: "/tmp/beta" },
        ],
      }), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    global.fetch = fetchMock;

    render(<ToastProvider><ProjectsPage /></ToastProvider>);
    await waitFor(() => {
      expect(screen.getByText("Alpha")).toBeTruthy();
      expect(screen.getByText("Beta")).toBeTruthy();
    });

    const filter = screen.getByPlaceholderText(/filter/i);
    fireEvent.change(filter, { target: { value: "alpha" } });
    expect(screen.getByText("Alpha")).toBeTruthy();
    expect(screen.queryByText("Beta")).toBeFalsy();
  });
});
